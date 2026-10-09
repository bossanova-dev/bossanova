package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var _ NoteStore = (*SQLiteNoteStore)(nil)

// SQLiteNoteStore implements NoteStore using SQLite.
type SQLiteNoteStore struct {
	db        *sql.DB
	retention NoteRetention
	// createMu serialises Create's insert, prune and readback. Each prune
	// spares only its own new note, so two interleaved creates under a small
	// cap could otherwise delete each other's just-written note.
	createMu sync.Mutex
	// logger receives prune warnings. Nil means the process-global log.Logger,
	// resolved at call time so a logger configured after construction is used.
	logger *zerolog.Logger
}

// NoteRetention is the note-pruning policy (BOS-1384). A zero field means
// unlimited: a zero MaxAge keeps notes forever, a zero MaxPerRepo never caps.
type NoteRetention struct {
	MaxAge     time.Duration
	MaxPerRepo int
}

// NoteRetentionFromSettings resolves the settings.json notes block into a
// policy, applying the shipped defaults (180 days, 10,000 per repo) for unset
// or negative values and treating an explicit 0 as unlimited.
func NoteRetentionFromSettings(c config.NotesConfig) NoteRetention {
	// Clamp before multiplying: a larger day count overflows time.Duration and
	// can wrap to a tiny positive window that would prune nearly every note.
	days := min(c.RetentionDaysOrDefault(), maxNoteRetentionDays)
	return NoteRetention{
		MaxAge:     time.Duration(days) * 24 * time.Hour,
		MaxPerRepo: c.MaxPerRepoOrDefault(),
	}
}

// maxNoteRetentionDays is the largest day count time.Duration can represent
// (about 292 years); larger settings are clamped to it.
const maxNoteRetentionDays = int(math.MaxInt64 / int64(24*time.Hour))

// NoteStoreOption configures a SQLiteNoteStore.
type NoteStoreOption func(*SQLiteNoteStore)

// WithNoteRetention overrides the store's pruning policy.
func WithNoteRetention(r NoteRetention) NoteStoreOption {
	return func(s *SQLiteNoteStore) { s.retention = r }
}

// WithNoteLogger routes the store's prune warnings to logger instead of the
// process-global log.Logger.
func WithNoteLogger(logger zerolog.Logger) NoteStoreOption {
	return func(s *SQLiteNoteStore) { s.logger = &logger }
}

// notePruneExpiredSQL deletes the inserting repo's notes older than the
// retention cutoff. It is a range scan on idx_notes_repo_created.
const notePruneExpiredSQL = `DELETE FROM notes WHERE repo_id = ? AND created_at < ?`

// notePruneOverCapSQL keeps the just-inserted note plus the newest cap-1 other
// notes in the repo and deletes the rest, oldest first. It is bound as
// (repoID, newID, cap-1). The new note is excluded by id rather than relying on
// ordering: ids are random and created_at is millisecond-granular, so a
// same-millisecond peer could otherwise outrank it and a small cap would trim
// the note just written. The tiebreak is List's ORDER BY reversed.
const notePruneOverCapSQL = `DELETE FROM notes WHERE id IN (
	SELECT id FROM notes WHERE repo_id = ? AND id <> ?
	ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?)`

// noteSyncPruneExpiredSQL and noteSyncPruneOverCapSQL delete the sync-outbox
// rows of exactly the notes the matching prune statement deletes, bound with
// the same arguments. Retention pruning is a local-storage concern, so it
// removes the outbox row outright instead of leaving a tombstone: the cloud
// expires its copy on its own schedule, and an age-pruned note is already past
// that window. Each pair runs in one transaction, sync rows first, so a failed
// note delete leaves the outbox untouched rather than orphaning live notes.
const (
	noteSyncPruneExpiredSQL = `DELETE FROM note_sync_states WHERE note_id IN (
	SELECT id FROM notes WHERE repo_id = ? AND created_at < ?)`
	noteSyncPruneOverCapSQL = `DELETE FROM note_sync_states WHERE note_id IN (
	SELECT id FROM notes WHERE repo_id = ? AND id <> ?
	ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?)`
)

// noteSQL is the read surface shared by *sql.DB and *sql.Conn, mirroring
// repoSQL in repo_store.go. It lets one query helper serve both the pool and an
// already-checked-out transaction connection.
type noteSQL interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NewNoteStore creates a new SQLite-backed NoteStore. With no options it
// applies the shipped retention defaults (NoteRetentionFromSettings of an empty
// config) and logs prune warnings to the global logger.
func NewNoteStore(db *sql.DB, opts ...NoteStoreOption) *SQLiteNoteStore {
	s := &SQLiteNoteStore{
		db:        db,
		retention: NoteRetentionFromSettings(config.NotesConfig{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *SQLiteNoteStore) pruneLogger() *zerolog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return &log.Logger
}

// prune best-effort applies the retention policy to repoID after a fresh note
// insert has committed. It runs on the caller's still-checked-out conn, because
// a pool lookup would deadlock a single-connection pool. Errors are logged,
// never returned: the note is already durable, and pruning is idempotent, so
// the next insert in the repo catches up.
func (s *SQLiteNoteStore) prune(ctx context.Context, conn *sql.Conn, repoID, newID string) {
	if s.retention.MaxAge > 0 {
		cutoff := time.Now().UTC().Add(-s.retention.MaxAge).Format(sqlutil.TimeLayout)
		if err := pruneTx(ctx, conn, noteSyncPruneExpiredSQL, notePruneExpiredSQL, repoID, cutoff); err != nil {
			s.pruneLogger().Warn().Err(err).Str("repo_id", repoID).Str("prune", "expired").
				Msg("note retention: prune of expired notes failed")
		}
	}
	if s.retention.MaxPerRepo > 0 {
		if err := pruneTx(ctx, conn, noteSyncPruneOverCapSQL, notePruneOverCapSQL,
			repoID, newID, s.retention.MaxPerRepo-1); err != nil {
			s.pruneLogger().Warn().Err(err).Str("repo_id", repoID).Str("prune", "over_cap").
				Msg("note retention: prune of over-cap notes failed")
		}
	}
}

// pruneTx runs one prune kind — the outbox delete, then the note delete, both
// bound with args — in a single immediate transaction on conn, which must be in
// autocommit. Deleting the sync rows first is what lets both statements select
// the same notes. A failure rolls back both, on a non-cancellable context so an
// aborted request still releases the write lock.
func pruneTx(ctx context.Context, conn *sql.Conn, syncSQL, noteSQL string, args ...any) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin note prune: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if _, err := conn.ExecContext(ctx, syncSQL, args...); err != nil {
		return fmt.Errorf("prune note sync states: %w", err)
	}
	if _, err := conn.ExecContext(ctx, noteSQL, args...); err != nil {
		return fmt.Errorf("prune notes: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit note prune: %w", err)
	}
	committed = true
	return nil
}

// normalizeTags trims, lowercases, de-duplicates, and sorts tags, rejecting an
// empty or oversize tag and a set larger than NoteMaxTags.
//
// Normalisation is deliberately LOSSY (see models.Note): folding case is what
// makes the note_tags primary key meaningful, so "Tech-Debt" and "tech-debt"
// can never become two rows that a case-sensitive filter would miss. The cap is
// applied AFTER de-duplication so a caller repeating one tag is not penalised.
func normalizeTags(tags []string) ([]string, error) {
	var invalid error
	out := foldTags(tags, func(tag string) bool {
		if invalid != nil {
			return false
		}
		switch {
		case tag == "":
			invalid = fmt.Errorf("%w: tag must not be empty", ErrNoteInvalid)
		case len(tag) > NoteMaxTagLength:
			// len() is bytes, and the caps are documented in bytes (see
			// models.NoteMaxTagLength and the proto) — say so, rather than
			// telling a caller with multi-byte tags they exceeded a character
			// count they did not.
			invalid = fmt.Errorf("%w: tag %q exceeds %d bytes", ErrNoteInvalid, tag, NoteMaxTagLength)
		default:
			return true
		}
		return false
	})
	if invalid != nil {
		return nil, invalid
	}
	if len(out) > NoteMaxTags {
		return nil, fmt.Errorf("%w: %d tags exceeds the limit of %d", ErrNoteInvalid, len(out), NoteMaxTags)
	}
	slices.Sort(out)
	return out, nil
}

// foldTags applies the ONE canonical tag normalisation — trim, lowercase,
// de-duplicate — and keeps each folded tag that accept returns true for.
//
// Both the write path (normalizeTags) and the read path (listFilterTags) go
// through it so the two can never drift: the whole point of folding case on
// write is that a filter spelled differently still matches, an invariant that
// two hand-maintained copies of the same loop cannot guarantee. They differ
// only in the accept policy — write REJECTS an invalid tag, read DROPS it.
func foldTags(tags []string, accept func(tag string) bool) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if !accept(tag) {
			continue
		}
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

// validateNoteBody checks a body is present and within the size cap. The body
// is stored verbatim; only the emptiness check trims.
func validateNoteBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%w: body is required", ErrNoteInvalid)
	}
	if len(body) > NoteMaxBodyBytes {
		return fmt.Errorf("%w: body of %d bytes exceeds the limit of %d", ErrNoteInvalid, len(body), NoteMaxBodyBytes)
	}
	return nil
}

// optionalTrimmed maps a nullable provenance pointer to a driver value: nil, or
// a blank string after trimming, becomes SQL NULL so "absent" has one
// representation rather than two.
func optionalTrimmed(s *string) any {
	if s == nil {
		return nil
	}
	if v := strings.TrimSpace(*s); v != "" {
		return v
	}
	return nil
}

func idempotencyKey(s *string) (any, error) {
	if s == nil {
		return nil, nil
	}
	key := strings.TrimSpace(*s)
	if key == "" {
		return nil, fmt.Errorf("%w: idempotency key must not be empty", ErrNoteInvalid)
	}
	if len(key) > NoteMaxIdempotencyKeyLength {
		return nil, fmt.Errorf("%w: idempotency key exceeds %d bytes", ErrNoteInvalid, NoteMaxIdempotencyKeyLength)
	}
	return key, nil
}

func (s *SQLiteNoteStore) Create(ctx context.Context, params CreateNoteParams) (*models.Note, error) {
	repoID := strings.TrimSpace(params.RepoID)
	if repoID == "" {
		return nil, fmt.Errorf("%w: repo id is required", ErrNoteInvalid)
	}
	if err := validateNoteBody(params.Body); err != nil {
		return nil, err
	}
	tags, err := normalizeTags(params.Tags)
	if err != nil {
		return nil, err
	}
	key, err := idempotencyKey(params.IdempotencyKey)
	if err != nil {
		return nil, err
	}

	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new note id: %w", err)
	}
	now := sqlutil.TimeNow()

	s.createMu.Lock()
	defer s.createMu.Unlock()

	// The note row and its tag rows land together or not at all: a note that
	// materialised without the tags the caller asked for would be silently
	// invisible to the tag filter that is the whole point of the primitive.
	conn, err := beginImmediate(ctx, s.db, "note")
	if err != nil {
		return nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	result, err := conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO notes (id, repo_id, session_id, chat_id, body, idempotency_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, repoID, optionalTrimmed(params.SessionID), optionalTrimmed(params.ChatID),
		params.Body, key, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert note: %w", err)
	}
	created, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check note insert result: %w", err)
	}
	if created == 0 {
		if key == nil {
			return nil, fmt.Errorf("note insert was ignored without an idempotency key")
		}
		var existingID string
		if err := conn.QueryRowContext(ctx,
			`SELECT id FROM notes WHERE repo_id = ? AND idempotency_key = ?`, repoID, key,
		).Scan(&existingID); err != nil {
			return nil, fmt.Errorf("find idempotent note: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, fmt.Errorf("commit idempotent note create: %w", err)
		}
		committed = true
		return getNote(ctx, conn, existingID)
	}
	if err := insertNoteTags(ctx, conn, id, tags); err != nil {
		return nil, err
	}
	// The outbox row commits with the note or not at all, so the sync worker can
	// never miss a created note or see one that rolled back.
	if err := recordNoteChange(ctx, conn, id, false, now, nil); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit note create: %w", err)
	}
	committed = true

	// Prune only after the fresh insert is durable, so a prune failure can never
	// fail or roll back the note. The idempotent-retry branch above inserted
	// nothing and so never reaches here.
	s.prune(ctx, conn, repoID, id)

	// Read back on the same connection: it is still checked out (the deferred
	// closeImmediate has not run yet), so calling s.Get here would deadlock on a
	// single-connection (in-memory) pool. Post-COMMIT the conn is in autocommit.
	return getNote(ctx, conn, id)
}

func (s *SQLiteNoteStore) Get(ctx context.Context, id string) (*models.Note, error) {
	return getNote(ctx, s.db, id)
}

func (s *SQLiteNoteStore) List(ctx context.Context, filter ListNotesFilter) ([]*models.Note, error) {
	var where []string
	var args []any
	if filter.RepoID != nil {
		where = append(where, "repo_id = ?")
		args = append(args, strings.TrimSpace(*filter.RepoID))
	}
	if filter.SessionID != nil {
		where = append(where, "session_id = ?")
		args = append(args, strings.TrimSpace(*filter.SessionID))
	}
	if filter.ChatID != nil {
		where = append(where, "chat_id = ?")
		args = append(args, strings.TrimSpace(*filter.ChatID))
	}
	if len(filter.Tags) > 0 {
		tags := listFilterTags(filter.Tags)
		if len(tags) == 0 {
			// Every supplied tag normalised away (e.g. `--tag ""` or a whitespace
			// entry from the CLI/MCP surfaces). A tag list is a NARROWING
			// predicate, so this must fail CLOSED: dropping the clause here would
			// turn a malformed filter into an unbounded dump of every note, the
			// opposite of what a filter that simply matches nothing returns.
			return nil, nil
		}
		// Any-of (OR), not all-of: one IN clause served by idx_note_tags_tag.
		where = append(where,
			"id IN (SELECT note_id FROM note_tags WHERE tag IN ("+placeholders(len(tags))+"))")
		for _, tag := range tags {
			args = append(args, tag)
		}
	}
	if filter.Search != nil {
		if term := strings.TrimSpace(*filter.Search); term != "" {
			// LOWER both sides so the match is case-insensitive for a body of any
			// case (LIKE's own case-folding is ASCII-only and LOWER() is too
			// without ICU, so this folds ASCII, not Unicode), and escape the term
			// so a literal % or _ in a search string is not silently promoted to a
			// wildcard.
			where = append(where, `LOWER(body) LIKE ? ESCAPE '\'`)
			args = append(args, "%"+escapeLikeTerm(strings.ToLower(term))+"%")
		}
	}

	query := noteSelectSQL
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at ASC, id ASC"
	if filter.Limit != nil && *filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, *filter.Limit)
	}
	// WHERE is built from code-literal fragments plus a generated run of `?`
	// placeholders; every value is bound via ?, not concatenated user text.
	// #nosec G202 -- only code-literal SQL and `?` placeholders are concatenated
	// owner=@recurser; review-by=2027-01-27; issue=BOS-550
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	notes, err := collectNotes(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachInBatches(ctx, notes, s.attachTagBatch); err != nil {
		return nil, err
	}
	if err := s.attachInBatches(ctx, notes, s.attachSyncBatch); err != nil {
		return nil, err
	}
	return notes, nil
}

func (s *SQLiteNoteStore) Update(ctx context.Context, params UpdateNoteParams) (*models.Note, error) {
	id := strings.TrimSpace(params.ID)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrNoteInvalid)
	}
	if params.Body != nil {
		if err := validateNoteBody(*params.Body); err != nil {
			return nil, err
		}
	}
	// A nil Tags slice leaves the tag set alone; a non-nil one replaces it,
	// including the empty slice, which clears every tag.
	var tags []string
	if params.Tags != nil {
		normalized, err := normalizeTags(params.Tags)
		if err != nil {
			return nil, err
		}
		tags = normalized
	}
	if params.Body == nil && params.Tags == nil {
		// Nothing to change: return the current note rather than bumping
		// updated_at for a write that alters no field.
		return s.Get(ctx, id)
	}

	conn, err := beginImmediate(ctx, s.db, "note")
	if err != nil {
		return nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	// Confirm the row exists inside the transaction so an absent id is a clean
	// sql.ErrNoRows rather than a silent no-op that reports success.
	var existing string
	if err := conn.QueryRowContext(ctx, "SELECT id FROM notes WHERE id = ?", id).Scan(&existing); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("select note for update: %w", err)
	}

	now := sqlutil.TimeNow()
	if params.Body != nil {
		if _, err := conn.ExecContext(ctx,
			"UPDATE notes SET body = ?, updated_at = ? WHERE id = ?", *params.Body, now, id,
		); err != nil {
			return nil, fmt.Errorf("update note body: %w", err)
		}
	} else {
		if _, err := conn.ExecContext(ctx,
			"UPDATE notes SET updated_at = ? WHERE id = ?", now, id,
		); err != nil {
			return nil, fmt.Errorf("update note: %w", err)
		}
	}
	if params.Tags != nil {
		// Replace, not merge: delete-then-insert in the SAME transaction so the
		// note is never observable with a partial tag set.
		if _, err := conn.ExecContext(ctx, "DELETE FROM note_tags WHERE note_id = ?", id); err != nil {
			return nil, fmt.Errorf("clear note tags: %w", err)
		}
		if err := insertNoteTags(ctx, conn, id, tags); err != nil {
			return nil, err
		}
	}
	if err := recordNoteChange(ctx, conn, id, false, now, nil); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit note update: %w", err)
	}
	committed = true
	return getNote(ctx, conn, id)
}

func (s *SQLiteNoteStore) Delete(ctx context.Context, id string) error {
	// The note delete and its tombstone land together: a delete that committed
	// without a tombstone would never reach the cloud copy.
	conn, err := beginImmediate(ctx, s.db, "note")
	if err != nil {
		return err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	// Tag rows go with the note via the note_tags ON DELETE CASCADE foreign key
	// (the pool runs with PRAGMA foreign_keys=ON). RETURNING captures what the tombstone needs once the note row is gone:
	// the repository that routes it and the creation time Bosso dates it by.
	var source noteTombstoneSource
	err = conn.QueryRowContext(ctx,
		"DELETE FROM notes WHERE id = ? RETURNING repo_id, created_at", id,
	).Scan(&source.repoID, &source.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Idempotent: an already-absent note is a nil no-op that writes no
		// tombstone (the deferred ROLLBACK ends the empty transaction).
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	if err := recordNoteChange(ctx, conn, id, true, sqlutil.TimeNow(), &source); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit note delete: %w", err)
	}
	committed = true
	return nil
}

// insertNoteTags writes a note's normalised tag rows on an open transaction.
func insertNoteTags(ctx context.Context, conn *sql.Conn, noteID string, tags []string) error {
	for _, tag := range tags {
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO note_tags (note_id, tag) VALUES (?, ?)", noteID, tag,
		); err != nil {
			return fmt.Errorf("insert note tag: %w", err)
		}
	}
	return nil
}

// getNote reads a note, its tags and its sync state from any queryer. Parameterising on
// noteSQL rather than *sql.DB lets the pool-level Get and the
// still-checked-out-connection read-backs in Create/Update share one
// implementation: both *sql.DB and *sql.Conn satisfy it, so the single-note
// read exists once instead of once per handle type.
func getNote(ctx context.Context, q noteSQL, id string) (*models.Note, error) {
	note, err := scanNote(q.QueryRowContext(ctx, noteSelectSQL+" WHERE id = ?", id))
	if err != nil {
		return nil, err
	}
	tags, err := tagsFor(ctx, q, id)
	if err != nil {
		return nil, err
	}
	note.Tags = tags
	syncState, err := syncFor(ctx, q, id)
	if err != nil {
		return nil, err
	}
	note.Sync = syncState
	return note, nil
}

// tagsFor returns one note's tags in ascending order.
func tagsFor(ctx context.Context, q noteSQL, noteID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT tag FROM note_tags WHERE note_id = ? ORDER BY tag ASC", noteID)
	if err != nil {
		return nil, fmt.Errorf("select note tags: %w", err)
	}
	return collectTags(rows)
}

// noteTagBatchSize caps how many note ids one batched List attach query binds. SQLite's
// default SQLITE_MAX_VARIABLE_NUMBER is 32766, and an unlimited List can return
// more notes than that, so the ids are chunked: without this an otherwise valid
// list over a large repo would fail with a bind-parameter error rather than
// simply doing a little more work.
const noteTagBatchSize = 500

// attachInBatches runs attach over every listed note in chunks of
// noteTagBatchSize ids, so List fills in tags and sync state with batched
// queries rather than a per-note round trip.
func (s *SQLiteNoteStore) attachInBatches(ctx context.Context, notes []*models.Note,
	attach func(ctx context.Context, byID map[string]*models.Note, args []any) error,
) error {
	if len(notes) == 0 {
		return nil
	}
	byID := make(map[string]*models.Note, len(notes))
	for _, n := range notes {
		byID[n.ID] = n
	}
	for start := 0; start < len(notes); start += noteTagBatchSize {
		end := min(start+noteTagBatchSize, len(notes))
		batch := notes[start:end]
		args := make([]any, 0, len(batch))
		for _, n := range batch {
			args = append(args, n.ID)
		}
		if err := attach(ctx, byID, args); err != nil {
			return err
		}
	}
	return nil
}

// attachTagBatch appends the tags for one chunk of note ids. Tags arrive in
// ascending order per note, matching the single-note read paths.
func (s *SQLiteNoteStore) attachTagBatch(ctx context.Context, byID map[string]*models.Note, args []any) error {
	// #nosec G202 -- the only interpolation is a generated run of `?`
	// placeholders; every id is bound; owner=@recurser; review-by=2027-01-27; issue=BOS-550
	rows, err := s.db.QueryContext(ctx,
		"SELECT note_id, tag FROM note_tags WHERE note_id IN ("+placeholders(len(args))+") ORDER BY note_id ASC, tag ASC",
		args...)
	if err != nil {
		return fmt.Errorf("select note tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var noteID, tag string
		if err := rows.Scan(&noteID, &tag); err != nil {
			return fmt.Errorf("scan note tag: %w", err)
		}
		if n, ok := byID[noteID]; ok {
			n.Tags = append(n.Tags, tag)
		}
	}
	return rows.Err()
}

// attachSyncBatch fills in the sync state for one chunk of note ids.
func (s *SQLiteNoteStore) attachSyncBatch(ctx context.Context, byID map[string]*models.Note, args []any) error {
	return attachSyncBatch(ctx, s.db, byID, args)
}

// listFilterTags applies the same trim+lowercase+de-dup normalisation to filter
// tags that write applies to stored tags, so a filter cannot miss a row purely
// because the caller spelled the tag with different casing. Unlike the write
// path it drops invalid entries instead of erroring — an unmatched tag simply
// matches nothing. Callers MUST treat an empty result from a non-empty input as
// "match nothing", not "no filter"; List does.
func listFilterTags(tags []string) []string {
	return foldTags(tags, func(tag string) bool { return tag != "" })
}

// placeholders returns "?, ?, ?" for n bound values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// escapeLikeTerm neutralises LIKE's wildcards so a search term is matched
// literally. The escape character itself must be escaped first.
func escapeLikeTerm(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(term)
}

const noteSelectSQL = `SELECT id, repo_id, session_id, chat_id, body, created_at, updated_at
	FROM notes`

func collectNotes(rows *sql.Rows) ([]*models.Note, error) {
	defer func() { _ = rows.Close() }()
	var notes []*models.Note
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func collectTags(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scan note tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func scanNote(sc sqlutil.Scanner) (*models.Note, error) {
	var note models.Note
	var sessionID, chatID sql.NullString
	var createdAt, updatedAt string
	if err := sc.Scan(
		&note.ID, &note.RepoID, &sessionID, &chatID, &note.Body, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	if sessionID.Valid {
		v := sessionID.String
		note.SessionID = &v
	}
	if chatID.Valid {
		v := chatID.String
		note.ChatID = &v
	}
	note.CreatedAt = sqlutil.ParseTime(createdAt)
	note.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &note, nil
}
