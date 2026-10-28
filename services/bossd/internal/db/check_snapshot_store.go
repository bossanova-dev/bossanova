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

type CheckSnapshot = domain.CheckSnapshot

// CheckSnapshotStore persists DisplayPoller snapshots.
type CheckSnapshotStore interface {
	Create(context.Context, domain.CheckSnapshot) error
	ListBySession(context.Context, string, int) ([]domain.CheckSnapshot, error)
	ListBySessionHead(context.Context, string, string) ([]domain.CheckSnapshot, error)
	ListBySessionHeadNewestFirst(context.Context, string, string, bool, func(domain.CheckSnapshot) bool) error
	GetAttentionStartsBySessionHead(context.Context, string, string) (int, time.Time, time.Time, bool, error)
}

type SQLiteCheckSnapshotStore struct {
	db      *sql.DB
	queries *sqlcgen.Queries
}

func NewCheckSnapshotStore(db *sql.DB) *SQLiteCheckSnapshotStore {
	return &SQLiteCheckSnapshotStore{db: db, queries: sqlcgen.New(db)}
}

// Create appends a snapshot row. PolledAt defaults to now when unset.
func (s *SQLiteCheckSnapshotStore) Create(ctx context.Context, snap domain.CheckSnapshot) error {
	if snap.SessionID == "" {
		return fmt.Errorf("insert check snapshot: session ID required")
	}
	if snap.PolledAt.IsZero() {
		snap.PolledAt = time.Now()
	}
	conn, err := beginImmediate(ctx, s.db, "check snapshot")
	if err != nil {
		return err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)
	queries := sqlcgen.New(conn)
	statusStart, verifiedStart, err := snapshotRunStarts(ctx, queries, snap)
	if err != nil {
		return fmt.Errorf("derive check snapshot runs: %w", err)
	}
	err = queries.CreateCheckSnapshot(ctx, sqlcgen.CreateCheckSnapshotParams{
		SessionID: snap.SessionID, PolledAt: sqlutil.FormatTime(snap.PolledAt),
		HeadSha: snap.HeadSHA, RawJson: snap.RawJSON, ComputedStatus: int64(snap.ComputedStatus),
		StatusRunStartedAt: statusStart, VerifiedRunStartedAt: verifiedStart,
	})
	if err != nil {
		return fmt.Errorf("insert check snapshot: %w", err)
	}
	if err := queries.MarkCheckSnapshotRunStartsUntracked(ctx, sqlcgen.MarkCheckSnapshotRunStartsUntrackedParams{
		SessionID: snap.SessionID, HeadSha: snap.HeadSHA, PolledAt: sqlutil.FormatTime(snap.PolledAt),
	}); err != nil {
		return fmt.Errorf("invalidate check snapshot runs: %w", err)
	}
	if err := execCommit(ctx, conn); err != nil {
		return fmt.Errorf("commit check snapshot: %w", err)
	}
	committed = true
	return nil
}

func (s *SQLiteCheckSnapshotStore) ListBySessionHead(ctx context.Context, sessionID, headSHA string) ([]domain.CheckSnapshot, error) {
	if headSHA == "" {
		return nil, nil
	}
	rows, err := s.queries.ListCheckSnapshotsBySessionHead(ctx, sqlcgen.ListCheckSnapshotsBySessionHeadParams{SessionID: sessionID, HeadSha: headSHA})
	if err != nil {
		return nil, fmt.Errorf("list check snapshots by head: %w", err)
	}
	return mapRows(rows, checkSnapshotFromRow), nil
}

// ListBySessionHeadNewestFirst reads one row at a time so a stopped visitor
// never reads later raw payloads. Summary reads use the covering head index.
func (s *SQLiteCheckSnapshotStore) ListBySessionHeadNewestFirst(ctx context.Context, sessionID, headSHA string, withRawJSON bool, visit func(domain.CheckSnapshot) bool) error {
	if headSHA == "" {
		return nil
	}
	// Keep every keyset read in the same SQLite snapshot, including when the
	// visitor writes new snapshots through a different connection.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("scan check snapshots by head: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	var beforeID int64
	var beforePolledAt string
	for {
		var row sqlcgen.SessionCheckSnapshot
		var err error
		if withRawJSON {
			if beforeID == 0 {
				row, err = queries.GetNewestCheckSnapshotBySessionHead(ctx, sqlcgen.GetNewestCheckSnapshotBySessionHeadParams{SessionID: sessionID, HeadSha: headSHA})
			} else {
				row, err = queries.GetNextCheckSnapshotBySessionHead(ctx, sqlcgen.GetNextCheckSnapshotBySessionHeadParams{SessionID: sessionID, HeadSha: headSHA, BeforeID: beforeID, BeforePolledAt: beforePolledAt})
			}
		} else if beforeID == 0 {
			summary, queryErr := queries.GetNewestCheckSnapshotSummaryBySessionHead(ctx, sqlcgen.GetNewestCheckSnapshotSummaryBySessionHeadParams{SessionID: sessionID, HeadSha: headSHA})
			err = queryErr
			row = sqlcgen.SessionCheckSnapshot{ID: summary.ID, SessionID: summary.SessionID, PolledAt: summary.PolledAt, HeadSha: summary.HeadSha, ComputedStatus: summary.ComputedStatus}
		} else {
			summary, queryErr := queries.GetNextCheckSnapshotSummaryBySessionHead(ctx, sqlcgen.GetNextCheckSnapshotSummaryBySessionHeadParams{SessionID: sessionID, HeadSha: headSHA, BeforeID: beforeID, BeforePolledAt: beforePolledAt})
			err = queryErr
			row = sqlcgen.SessionCheckSnapshot{ID: summary.ID, SessionID: summary.SessionID, PolledAt: summary.PolledAt, HeadSha: summary.HeadSha, ComputedStatus: summary.ComputedStatus}
		}
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scan check snapshots by head: %w", err)
		}
		if !visit(checkSnapshotFromRow(row)) {
			return nil
		}
		beforeID, beforePolledAt = row.ID, row.PolledAt
	}
}

func (s *SQLiteCheckSnapshotStore) ListBySession(ctx context.Context, sessionID string, limit int) ([]domain.CheckSnapshot, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.queries.ListCheckSnapshotsBySession(ctx, sqlcgen.ListCheckSnapshotsBySessionParams{SessionID: sessionID, Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list check snapshots: %w", err)
	}
	return mapRows(rows, checkSnapshotFromRow), nil
}

func checkSnapshotFromRow(row sqlcgen.SessionCheckSnapshot) domain.CheckSnapshot {
	return domain.CheckSnapshot{ID: row.ID, SessionID: row.SessionID, PolledAt: textTime(row.PolledAt), HeadSHA: row.HeadSha, RawJSON: row.RawJson, ComputedStatus: int(row.ComputedStatus)}
}
