package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
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

// noteSyncUpsertSQL records one local note write in the outbox. It is bound as
// (noteID, isDeleted, now, now) and always runs on the caller's open
// transaction, so the outbox change commits or rolls back with the note write.
//
// A first write inserts version 1; any later write bumps source_version and
// resets the row to a fresh pending change. attempt_count and last_error reset
// too: a new version is a new payload, so the previous version's retry history
// says nothing about it.
//
// repo_id and note_created_at are the tombstone's routing metadata (BOS-1435):
// only a delete supplies them, and a NULL from a create or update keeps
// whatever the row already holds.
const noteSyncUpsertSQL = `INSERT INTO note_sync_states
	(note_id, source_version, is_deleted, sync_state, repo_id, note_created_at, created_at, updated_at)
	VALUES (?, 1, ?, 'pending', ?, ?, ?, ?)
	ON CONFLICT(note_id) DO UPDATE SET
		source_version  = source_version + 1,
		is_deleted      = excluded.is_deleted,
		sync_state      = 'pending',
		next_attempt_at = NULL,
		attempt_count   = 0,
		last_error      = NULL,
		repo_id         = COALESCE(excluded.repo_id, repo_id),
		note_created_at = COALESCE(excluded.note_created_at, note_created_at),
		updated_at      = excluded.updated_at`

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
	var repoID, createdAt any
	if tombstone != nil {
		repoID, createdAt = tombstone.repoID, tombstone.createdAt
	}
	if _, err := conn.ExecContext(ctx, noteSyncUpsertSQL,
		noteID, sqlutil.BoolToInt(isDeleted), repoID, createdAt, now, now,
	); err != nil {
		return fmt.Errorf("record note sync change: %w", err)
	}
	return nil
}

// noteSyncSelectSQL reads the columns scanNoteSyncState expects, prefixed by
// note_id so batched reads can key the result.
const noteSyncSelectSQL = `SELECT note_id, sync_state, source_version, synced_version, is_deleted,
	attempt_count, next_attempt_at, last_attempted_at, synced_at, organization_id, last_error
	FROM note_sync_states`

// scanNoteSyncState scans one noteSyncSelectSQL row.
func scanNoteSyncState(sc sqlutil.Scanner) (string, *models.NoteSyncState, error) {
	var (
		noteID, state                            string
		out                                      models.NoteSyncState
		isDeleted                                int
		nextAttemptAt, lastAttemptedAt, syncedAt sql.NullString
		organizationID, lastError                sql.NullString
	)
	if err := sc.Scan(&noteID, &state, &out.SourceVersion, &out.SyncedVersion, &isDeleted,
		&out.AttemptCount, &nextAttemptAt, &lastAttemptedAt, &syncedAt, &organizationID, &lastError,
	); err != nil {
		return "", nil, err
	}
	out.State = models.NoteSyncStatus(state)
	out.IsDeleted = isDeleted != 0
	out.NextAttemptAt = parseNullTime(nextAttemptAt)
	out.LastAttemptedAt = parseNullTime(lastAttemptedAt)
	out.SyncedAt = parseNullTime(syncedAt)
	out.OrganizationID = nullStringPtr(organizationID)
	out.LastError = nullStringPtr(lastError)
	return noteID, &out, nil
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// syncFor returns one note's outbox state, or nil when it has no row.
func syncFor(ctx context.Context, q noteSQL, noteID string) (*models.NoteSyncState, error) {
	_, state, err := scanNoteSyncState(q.QueryRowContext(ctx, noteSyncSelectSQL+" WHERE note_id = ?", noteID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select note sync state: %w", err)
	}
	return state, nil
}

// attachSyncBatch fills in Sync for one chunk of listed note ids.
func attachSyncBatch(ctx context.Context, q noteSQL, byID map[string]*models.Note, args []any) error {
	// #nosec G202 -- the only interpolation is a generated run of `?`
	// placeholders; every id is bound; owner=@recurser; review-by=2027-01-27; issue=BOS-1429
	rows, err := q.QueryContext(ctx,
		noteSyncSelectSQL+" WHERE note_id IN ("+placeholders(len(args))+")", args...)
	if err != nil {
		return fmt.Errorf("select note sync states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		noteID, state, err := scanNoteSyncState(rows)
		if err != nil {
			return fmt.Errorf("scan note sync state: %w", err)
		}
		if n, ok := byID[noteID]; ok {
			n.Sync = state
		}
	}
	return rows.Err()
}

// noteSyncClaimableStates are the states the worker retries once
// next_attempt_at is due. not_entitled and refused are retried too, on the
// worker's hourly schedule, so a later plan upgrade or repository mapping fix
// is picked up without a local edit (BOS-1435). Every other state is terminal
// for its version: only a new local write (which resets the row to pending)
// makes the note due again.
const noteSyncClaimableStates = `'pending', 'rate_limited', 'failed', 'not_entitled', 'refused'`

// noteSyncDueSQL selects due outbox rows, oldest change first. A live row whose
// note is gone (an orphan) is skipped: there is nothing to send for it, and
// PurgeSettledTombstones removes it.
const noteSyncDueSQL = `SELECT s.note_id, s.source_version, s.is_deleted, s.attempt_count,
	s.repo_id, s.note_created_at
	FROM note_sync_states s
	WHERE s.sync_state IN (` + noteSyncClaimableStates + `)
	  AND (s.next_attempt_at IS NULL OR s.next_attempt_at <= ?)
	  AND (s.is_deleted = 1 OR EXISTS (SELECT 1 FROM notes n WHERE n.id = s.note_id))
	ORDER BY s.updated_at ASC, s.note_id ASC
	LIMIT ?`

// ClaimDue implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) ClaimDue(ctx context.Context, now time.Time, limit int) ([]NoteSyncChange, error) {
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
		if _, err := conn.ExecContext(ctx,
			`UPDATE note_sync_states SET attempt_count = attempt_count + 1, last_attempted_at = ?
			 WHERE note_id = ?`, nowStr, c.NoteID,
		); err != nil {
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
	rows, err := conn.QueryContext(ctx, noteSyncDueSQL, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select due note sync changes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var changes []NoteSyncChange
	for rows.Next() {
		var c NoteSyncChange
		var isDeleted int
		var repoID, noteCreatedAt sql.NullString
		if err := rows.Scan(&c.NoteID, &c.SourceVersion, &isDeleted, &c.AttemptCount,
			&repoID, &noteCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan due note sync change: %w", err)
		}
		c.IsDeleted = isDeleted != 0
		if c.IsDeleted {
			c.TombstoneRepoID = repoID.String
			c.TombstoneNoteCreatedAt = parseNullTime(noteCreatedAt)
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// RecordOutcome implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) RecordOutcome(
	ctx context.Context, noteID string, version int64, state models.NoteSyncStatus, details NoteSyncOutcomeDetails,
) (bool, error) {
	if !state.Valid() || state == models.NoteSyncPending {
		return false, fmt.Errorf("%w: outcome state %q", ErrNoteSyncInvalid, state)
	}
	now := sqlutil.TimeNow()
	var orgID any
	if details.OrganizationID != nil {
		if v := strings.TrimSpace(*details.OrganizationID); v != "" {
			orgID = v
		}
	}

	var (
		result sql.Result
		err    error
	)
	if state == models.NoteSyncSynced {
		result, err = s.db.ExecContext(ctx,
			`UPDATE note_sync_states SET
				sync_state = 'synced', synced_version = ?, synced_at = ?,
				last_error = NULL, next_attempt_at = NULL,
				organization_id = COALESCE(?, organization_id), updated_at = ?
			 WHERE note_id = ? AND source_version = ?`,
			version, now, orgID, now, noteID, version)
	} else {
		var nextAttemptAt, lastError any
		if details.NextAttemptAt != nil {
			nextAttemptAt = sqlutil.FormatTime(*details.NextAttemptAt)
		}
		if details.Error != "" {
			lastError = details.Error
		}
		result, err = s.db.ExecContext(ctx,
			`UPDATE note_sync_states SET
				sync_state = ?, last_error = ?, next_attempt_at = ?,
				organization_id = COALESCE(?, organization_id), updated_at = ?
			 WHERE note_id = ? AND source_version = ?`,
			string(state), lastError, nextAttemptAt, orgID, now, noteID, version)
	}
	if err != nil {
		return false, fmt.Errorf("record note sync outcome: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check note sync outcome result: %w", err)
	}
	return n > 0, nil
}

// CountByState implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) CountByState(ctx context.Context) (map[models.NoteSyncStatus]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sync_state, COUNT(*) FROM note_sync_states GROUP BY sync_state`)
	if err != nil {
		return nil, fmt.Errorf("count note sync states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[models.NoteSyncStatus]int64{}
	for rows.Next() {
		var (
			state string
			n     int64
		)
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("scan note sync state count: %w", err)
		}
		counts[models.NoteSyncStatus(state)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count note sync states: %w", err)
	}
	return counts, nil
}

// NoteCloudTTL is Bosso's hard time to live for a synced note, counted from
// the source note's creation (OrganizationNote.expires_at). A tombstone last
// written before now - NoteCloudTTL deletes a note created even earlier, whose
// cloud copy has therefore already expired, so dropping it loses nothing.
const NoteCloudTTL = 90 * 24 * time.Hour

// PurgeSettledTombstones implements NoteSyncStore.
func (s *SQLiteNoteSyncStore) PurgeSettledTombstones(ctx context.Context, olderThan time.Time) (int64, error) {
	// Never age out an unsettled tombstone whose cloud copy may still be live:
	// clamp the caller's cutoff to the cloud TTL horizon.
	if horizon := time.Now().Add(-NoteCloudTTL); olderThan.After(horizon) {
		olderThan = horizon
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM note_sync_states
		 WHERE (is_deleted = 1 AND (sync_state IN ('synced', 'suppressed') OR updated_at < ?))
		    OR (is_deleted = 0 AND NOT EXISTS (SELECT 1 FROM notes n WHERE n.id = note_sync_states.note_id))`,
		sqlutil.FormatTime(olderThan))
	if err != nil {
		return 0, fmt.Errorf("purge settled note tombstones: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check note tombstone purge result: %w", err)
	}
	return n, nil
}
