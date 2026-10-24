package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ NoteSyncStore = (*SQLiteNoteSyncStore)(nil)

// SQLiteNoteSyncStore implements NoteSyncStore over the note_sync_states
// outbox that SQLiteNoteStore writes in the same transaction as every note
// create, update and delete (BOS-1429).
type SQLiteNoteSyncStore struct {
	db *sql.DB
}

// NewNoteSyncStore creates a SQLite-backed NoteSyncStore.
func NewNoteSyncStore(db *sql.DB) *SQLiteNoteSyncStore {
	return &SQLiteNoteSyncStore{db: db}
}

// noteTombstoneSource is what a delete captures about the note it removes, so
// the sync worker can still route and date the tombstone once the note row is
// gone.
type noteTombstoneSource struct {
	repoID    string
	createdAt string
}

// recordNoteChange writes the outbox change for one note write on an open
// transaction connection. tombstone is nil for a create or update.
func recordNoteChange(ctx context.Context, conn *sql.Conn, noteID string, isDeleted bool, now string, tombstone *noteTombstoneSource) error {
	var repoID, createdAt *string
	if tombstone != nil {
		repoID, createdAt = &tombstone.repoID, &tombstone.createdAt
	}
	if _, err := sqlcgen.New(conn).RecordNoteChange(ctx, sqlcgen.RecordNoteChangeParams{
		NoteID: noteID, IsDeleted: isDeleted, RepoID: repoID, NoteCreatedAt: createdAt,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("record note sync change: %w", err)
	}
	return nil
}

func noteSyncStateFromRow(row sqlcgen.NoteSyncState) *domain.NoteSyncState {
	return &domain.NoteSyncState{
		State: domain.NoteSyncStatus(row.SyncState), SourceVersion: row.SourceVersion,
		SyncedVersion: row.SyncedVersion, IsDeleted: row.IsDeleted, AttemptCount: int(row.AttemptCount),
		NextAttemptAt: optionalTextTime(row.NextAttemptAt), LastAttemptedAt: optionalTextTime(row.LastAttemptedAt),
		SyncedAt: optionalTextTime(row.SyncedAt), OrganizationID: row.OrganizationID, LastError: row.LastError,
	}
}

// syncFor returns one note's outbox state, or nil when it has no row.
func syncFor(ctx context.Context, q sqlcgen.DBTX, noteID string) (*domain.NoteSyncState, error) {
	row, err := sqlcgen.New(q).GetNoteSyncStateByNoteID(ctx, noteID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select note sync state: %w", err)
	}
	return noteSyncStateFromRow(sqlcgen.NoteSyncState{
		NoteID: row.NoteID, SyncState: row.SyncState, SourceVersion: row.SourceVersion, SyncedVersion: row.SyncedVersion, IsDeleted: row.IsDeleted, AttemptCount: row.AttemptCount, NextAttemptAt: row.NextAttemptAt, LastAttemptedAt: row.LastAttemptedAt, SyncedAt: row.SyncedAt, OrganizationID: row.OrganizationID, LastError: row.LastError,
	}), nil
}

// attachSyncBatch fills in Sync for one chunk of listed note ids.
func attachSyncBatch(ctx context.Context, q sqlcgen.DBTX, byID map[string]*domain.Note, args []string) error {
	rows, err := sqlcgen.New(q).ListNoteSyncStatesByNoteIDs(ctx, args)
	if err != nil {
		return fmt.Errorf("select note sync states: %w", err)
	}
	for _, row := range rows {
		if n, ok := byID[row.NoteID]; ok {
			n.Sync = noteSyncStateFromRow(row)
		}
	}
	return nil
}

// MarkDueClaimed implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) MarkDueClaimed(ctx context.Context, now time.Time, limit int) ([]NoteSyncChange, error) {
	if limit <= 0 {
		return nil, nil
	}
	nowStr := sqlutil.FormatTime(now)

	// One immediate transaction so the content read matches the version
	// claimed: no note write can land between the row select and the read.
	conn, err := beginImmediate(ctx, s.db, "note sync claim")
	if err != nil {
		return nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	changes, err := selectDueChanges(ctx, conn, nowStr, limit)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		c := &changes[i]
		if _, err := sqlcgen.New(conn).MarkNoteSyncAttempt(ctx, sqlcgen.MarkNoteSyncAttemptParams{
			LastAttemptedAt: &nowStr, NoteID: c.NoteID,
		}); err != nil {
			return nil, fmt.Errorf("mark note sync attempt: %w", err)
		}
		c.AttemptCount++
		if c.IsDeleted {
			continue
		}
		note, err := getNote(ctx, conn, c.NoteID)
		if err != nil {
			return nil, fmt.Errorf("read claimed note %s: %w", c.NoteID, err)
		}
		c.Note = note
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit note sync claim: %w", err)
	}
	committed = true
	return changes, nil
}

// selectDueChanges reads the due rows; it closes its result set before the
// caller issues further statements on the same connection.
func selectDueChanges(ctx context.Context, conn *sql.Conn, now string, limit int) ([]NoteSyncChange, error) {
	rows, err := sqlcgen.New(conn).ListDueNoteSyncChanges(ctx, sqlcgen.ListDueNoteSyncChangesParams{
		NextAttemptAt: &now, Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("select due note sync changes: %w", err)
	}
	var changes []NoteSyncChange
	for _, row := range rows {
		c := NoteSyncChange{
			NoteID: row.NoteID, SourceVersion: row.SourceVersion,
			IsDeleted: row.IsDeleted, AttemptCount: int(row.AttemptCount),
		}
		if c.IsDeleted {
			c.TombstoneRepoID = textValue(row.RepoID)
			c.TombstoneNoteCreatedAt = optionalTextTime(row.NoteCreatedAt)
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// RecordOutcome implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) RecordOutcome(
	ctx context.Context, noteID string, version int64, state domain.NoteSyncStatus, details NoteSyncOutcomeDetails,
) (bool, error) {
	if !state.Valid() || state == domain.NoteSyncPending {
		return false, fmt.Errorf("%w: outcome state %q", ErrNoteSyncInvalid, state)
	}
	now := sqlutil.TimeNow()
	var orgID *string
	if details.OrganizationID != nil {
		if v := strings.TrimSpace(*details.OrganizationID); v != "" {
			orgID = &v
		}
	}
	var (
		n   int64
		err error
	)
	queries := sqlcgen.New(s.db)
	if state == domain.NoteSyncSynced {
		n, err = queries.RecordNoteSyncSuccess(ctx, sqlcgen.RecordNoteSyncSuccessParams{
			SyncedVersion: version, SyncedAt: &now, OrganizationID: orgID,
			UpdatedAt: now, NoteID: noteID, SourceVersion: version,
		})
	} else {
		n, err = queries.RecordNoteSyncFailure(ctx, sqlcgen.RecordNoteSyncFailureParams{
			SyncState: string(state), LastError: optionalText(details.Error),
			NextAttemptAt: timeText(details.NextAttemptAt), OrganizationID: orgID,
			UpdatedAt: now, NoteID: noteID, SourceVersion: version,
		})
	}
	if err != nil {
		return false, fmt.Errorf("record note sync outcome: %w", err)
	}
	return n > 0, nil
}

// CountByState implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) CountByState(ctx context.Context) (map[domain.NoteSyncStatus]int64, error) {
	rows, err := sqlcgen.New(s.db).CountNoteSyncStatesByState(ctx)
	if err != nil {
		return nil, fmt.Errorf("count note sync states: %w", err)
	}
	counts := map[domain.NoteSyncStatus]int64{}
	for _, row := range rows {
		counts[domain.NoteSyncStatus(row.SyncState)] = row.Count
	}
	return counts, nil
}

// NoteCloudTTL is Bosso's hard time to live for a synced note, counted from
// the source note's creation (OrganizationNote.expires_at). A tombstone last
// written before now - NoteCloudTTL deletes a note created even earlier, whose
// cloud copy has therefore already expired, so dropping it loses nothing.
const NoteCloudTTL = 90 * 24 * time.Hour

// DeleteSettledTombstones implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) DeleteSettledTombstones(ctx context.Context, olderThan time.Time) (int64, error) {
	// Never age out an unsettled tombstone whose cloud copy may still be live:
	// clamp the caller's cutoff to the cloud TTL horizon.
	if horizon := time.Now().Add(-NoteCloudTTL); olderThan.After(horizon) {
		olderThan = horizon
	}
	n, err := sqlcgen.New(s.db).DeleteSettledNoteSyncTombstones(ctx, sqlutil.FormatTime(olderThan))
	if err != nil {
		return 0, fmt.Errorf("purge settled note tombstones: %w", err)
	}
	return n, nil
}
