package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"

	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// PendingTrackerMerge is the domain retry entry.
type PendingTrackerMerge = domain.PendingTrackerMerge

// PendingTrackerMergeStore is the durable queue of post-merge tracker writes
// still owed, so a failed one is retried across daemon restarts.
type PendingTrackerMergeStore interface {
	// Upsert records the session's next try, replacing any earlier entry.
	Upsert(ctx context.Context, p PendingTrackerMerge) error
	// ListByDueAt returns the entries due at or before now, earliest first.
	ListByDueAt(ctx context.Context, now time.Time) ([]PendingTrackerMerge, error)
	// Delete drops the session's entry; deleting a missing one is not an error.
	Delete(ctx context.Context, sessionID string) error
}

var _ PendingTrackerMergeStore = (*SQLitePendingTrackerMergeStore)(nil)

// SQLitePendingTrackerMergeStore implements PendingTrackerMergeStore over the
// pending_tracker_merges table.
type SQLitePendingTrackerMergeStore struct {
	db *sql.DB
}

// NewPendingTrackerMergeStore creates a SQLite-backed PendingTrackerMergeStore.
func NewPendingTrackerMergeStore(db *sql.DB) *SQLitePendingTrackerMergeStore {
	return &SQLitePendingTrackerMergeStore{db: db}
}

func (s *SQLitePendingTrackerMergeStore) Upsert(ctx context.Context, p PendingTrackerMerge) error {
	if p.SessionID == "" {
		return fmt.Errorf("schedule pending tracker merge: session ID required")
	}
	err := sqlcgen.New(s.db).UpsertPendingTrackerMerge(ctx, sqlcgen.UpsertPendingTrackerMergeParams{
		SessionID: p.SessionID, AttemptCount: int64(p.AttemptCount),
		NextAttemptAt: sqlutil.FormatTime(p.NextAttemptAt), LastReason: p.LastReason,
	})
	if err != nil {
		return fmt.Errorf("schedule pending tracker merge: %w", err)
	}
	return nil
}

func (s *SQLitePendingTrackerMergeStore) ListByDueAt(ctx context.Context, now time.Time) ([]PendingTrackerMerge, error) {
	rows, err := sqlcgen.New(s.db).ListPendingTrackerMergesByDueAt(ctx, sqlutil.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("list due pending tracker merges: %w", err)
	}
	return mapRows(rows, pendingTrackerMergeFromRow), nil
}

func pendingTrackerMergeFromRow(row sqlcgen.PendingTrackerMerge) domain.PendingTrackerMerge {
	return domain.PendingTrackerMerge{
		SessionID: row.SessionID, AttemptCount: int(row.AttemptCount),
		NextAttemptAt: textTime(row.NextAttemptAt), LastReason: row.LastReason,
	}
}

func (s *SQLitePendingTrackerMergeStore) Delete(ctx context.Context, sessionID string) error {
	if err := sqlcgen.New(s.db).DeletePendingTrackerMerge(ctx, sessionID); err != nil {
		return fmt.Errorf("delete pending tracker merge: %w", err)
	}
	return nil
}
