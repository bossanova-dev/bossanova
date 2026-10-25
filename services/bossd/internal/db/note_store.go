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

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
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
		if err := pruneTx(ctx, conn, func(q *sqlcgen.Queries) error {
			arg := sqlcgen.DeleteExpiredNotesParams{RepoID: repoID, CreatedAt: cutoff}
			if err := q.DeleteExpiredNoteSyncStates(ctx, sqlcgen.DeleteExpiredNoteSyncStatesParams(arg)); err != nil {
				return err
			}
			return q.DeleteExpiredNotes(ctx, arg)
		}); err != nil {
			s.pruneLogger().Warn().Err(err).Str("repo_id", repoID).Str("prune", "expired").
				Msg("note retention: prune of expired notes failed")
		}
	}
	if s.retention.MaxPerRepo > 0 {
		if err := pruneTx(ctx, conn, func(q *sqlcgen.Queries) error {
			arg := sqlcgen.DeleteOverCapNotesParams{RepoID: repoID, ID: newID, Offset: int64(s.retention.MaxPerRepo - 1)}
			if err := q.DeleteOverCapNoteSyncStates(ctx, sqlcgen.DeleteOverCapNoteSyncStatesParams(arg)); err != nil {
				return err
			}
			return q.DeleteOverCapNotes(ctx, arg)
		}); err != nil {
			s.pruneLogger().Warn().Err(err).Str("repo_id", repoID).Str("prune", "over_cap").
				Msg("note retention: prune of over-cap notes failed")
		}
	}
}

// pruneTx runs the generated outbox and note deletes in one immediate
// transaction on conn, which must be in
// autocommit. Deleting the sync rows first is what lets both statements select
// the same notes. A failure rolls back both, on a non-cancellable context so an
// aborted request still releases the write lock.
func pruneTx(ctx context.Context, conn *sql.Conn, prune func(*sqlcgen.Queries) error) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin note prune: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err := prune(sqlcgen.New(conn)); err != nil {
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
// Normalisation is deliberately LOSSY (see domain.Note): folding case is what
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
func optionalTrimmed(s *string) *string {
	if s == nil {
		return nil
	}
	if v := strings.TrimSpace(*s); v != "" {
		return &v
	}
	return nil
}

func idempotencyKey(s *string) (*string, error) {
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
	return &key, nil
}

func (s *SQLiteNoteStore) Create(ctx context.Context, params CreateNoteParams) (*domain.Note, error) {
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

	q := sqlcgen.New(conn)
	_, err = q.CreateNote(ctx, sqlcgen.CreateNoteParams{ID: id, RepoID: repoID, SessionID: optionalTrimmed(params.SessionID), ChatID: optionalTrimmed(params.ChatID), Body: params.Body, IdempotencyKey: key, CreatedAt: now, UpdatedAt: now})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("insert note: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {

		if key == nil {
			return nil, fmt.Errorf("note insert was ignored without an idempotency key")
		}
		existing, err := q.GetNoteByIdempotencyKey(ctx, sqlcgen.GetNoteByIdempotencyKeyParams{RepoID: repoID, IdempotencyKey: key})
		if err != nil {
			return nil, fmt.Errorf("find idempotent note: %w", notFound(err))
		}
		existingID := existing.ID
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

func (s *SQLiteNoteStore) Get(ctx context.Context, id string) (*domain.Note, error) {
	return getNote(ctx, s.db, id)
}

func (s *SQLiteNoteStore) List(ctx context.Context, filter ListNotesFilter) ([]*domain.Note, error) {
	tags := listFilterTags(filter.Tags)
	if len(filter.Tags) > 0 && len(tags) == 0 {
		return nil, nil
	}
	// Column1/3/5/7/9 are the presence flags for repo/session/chat/search/tags.
	// Positional binds keep sqlc.slice expansion from shifting numbered arguments.
	arg := sqlcgen.ListNotesParams{Column1: filter.RepoID != nil, RepoID: textValue(trimmedText(filter.RepoID)), Column3: filter.SessionID != nil, SessionID: trimmedText(filter.SessionID), Column5: filter.ChatID != nil, ChatID: trimmedText(filter.ChatID), Column9: len(tags) > 0, Tags: tags, Limit: -1}
	if filter.Search != nil {
		if term := strings.TrimSpace(*filter.Search); term != "" {
			value := "%" + escapeLikeTerm(strings.ToLower(term)) + "%"
			arg.Column7 = true
			arg.Column8 = value
		}
	}
	if filter.Limit != nil && *filter.Limit > 0 {
		arg.Limit = int64(*filter.Limit)
	}
	rows, err := sqlcgen.New(s.db).ListNotes(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	notes := mapRows(rows, noteFromRow)
	if err := s.attachInBatches(ctx, notes, s.attachTagBatch); err != nil {
		return nil, err
	}
	if err := s.attachInBatches(ctx, notes, s.attachSyncBatch); err != nil {
		return nil, err
	}
	return notes, nil
}

func (s *SQLiteNoteStore) Update(ctx context.Context, params UpdateNoteParams) (*domain.Note, error) {
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

	q := sqlcgen.New(conn)
	now := sqlutil.TimeNow()
	if _, err := q.UpdateNote(ctx, sqlcgen.UpdateNoteParams{ID: id, Body: params.Body, UpdatedAt: now}); err != nil {
		return nil, fmt.Errorf("update note: %w", notFound(err))
	}
	if params.Tags != nil {
		// Replace, not merge: delete-then-insert in the SAME transaction so the
		// note is never observable with a partial tag set.
		if err := q.DeleteNoteTags(ctx, id); err != nil {
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
	row, err := sqlcgen.New(conn).DeleteNote(ctx, id)
	source := noteTombstoneSource{repoID: row.RepoID, createdAt: row.CreatedAt}
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
		if err := sqlcgen.New(conn).CreateNoteTag(ctx, sqlcgen.CreateNoteTagParams{NoteID: noteID, Tag: tag}); err != nil {
			return fmt.Errorf("insert note tag: %w", err)
		}
	}
	return nil
}

// getNote reads a note, its tags and its sync state from any queryer. Parameterising on
// sqlcgen.DBTX rather than *sql.DB lets the pool-level Get and the
// still-checked-out-connection read-backs in Create/Update share one
// implementation: both *sql.DB and *sql.Conn satisfy it, so the single-note
// read exists once instead of once per handle type.
func getNote(ctx context.Context, q sqlcgen.DBTX, id string) (*domain.Note, error) {
	row, err := sqlcgen.New(q).GetNote(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	note := noteFromRow(row)
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
func tagsFor(ctx context.Context, q sqlcgen.DBTX, noteID string) ([]string, error) {
	tags, err := sqlcgen.New(q).ListNoteTags(ctx, noteID)
	if err != nil {
		return nil, fmt.Errorf("select note tags: %w", err)
	}
	return tags, nil
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
func (s *SQLiteNoteStore) attachInBatches(ctx context.Context, notes []*domain.Note,
	attach func(ctx context.Context, byID map[string]*domain.Note, args []string) error,
) error {
	if len(notes) == 0 {
		return nil
	}
	byID := make(map[string]*domain.Note, len(notes))
	for _, n := range notes {
		byID[n.ID] = n
	}
	for start := 0; start < len(notes); start += noteTagBatchSize {
		end := min(start+noteTagBatchSize, len(notes))
		batch := notes[start:end]
		args := make([]string, 0, len(batch))
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
func (s *SQLiteNoteStore) attachTagBatch(ctx context.Context, byID map[string]*domain.Note, args []string) error {
	rows, err := sqlcgen.New(s.db).ListNoteTagsByNoteIDs(ctx, args)
	if err != nil {
		return fmt.Errorf("select note tags: %w", err)
	}
	for _, row := range rows {
		if n, ok := byID[row.NoteID]; ok {
			n.Tags = append(n.Tags, row.Tag)
		}
	}
	return nil
}

// attachSyncBatch fills in the sync state for one chunk of note ids.
func (s *SQLiteNoteStore) attachSyncBatch(ctx context.Context, byID map[string]*domain.Note, args []string) error {
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

func noteFromRow(row sqlcgen.Note) *domain.Note {
	return &domain.Note{ID: row.ID, RepoID: row.RepoID, SessionID: row.SessionID, ChatID: row.ChatID, Body: row.Body, CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt)}
}

func trimmedText(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

// escapeLikeTerm keeps filter terms literal, including LIKE wildcards.
func escapeLikeTerm(term string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term)
}
