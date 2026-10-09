package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/sqlutil"
)

// TriggerLaunch is the daemon's idempotency record for one inbound trigger
// invocation (BOS-1418). A row with an empty SessionID is a claim whose launch
// is still in flight; a row with a SessionID is a finished launch that every
// replay of the same invocation must return.
type TriggerLaunch struct {
	InvocationID string
	TriggerID    string
	// SessionID is empty while the launch is in flight.
	SessionID string
	CreatedAt time.Time
}

// TriggerLaunchStore persists LaunchTriggerSessionCommand idempotency rows.
type TriggerLaunchStore interface {
	// Get returns the row for invocationID, or (nil, nil) when none exists.
	Get(ctx context.Context, invocationID string) (*TriggerLaunch, error)
	// Claim inserts an in-flight row for invocationID and reports whether this
	// caller won it. A competing caller that finds the row already present
	// loses (false, nil) — unless that row is an abandoned claim: still without
	// a session and created before staleBefore, as a daemon crash mid-launch
	// leaves behind. Such a row is taken over (and its age reset) so the
	// invocation is not wedged forever.
	Claim(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (bool, error)
	// SetSession records the session a won claim launched.
	SetSession(ctx context.Context, invocationID, sessionID string) error
	// Release deletes an in-flight claim after a failed launch so a retry can
	// try again. A row that already records a session is never released.
	Release(ctx context.Context, invocationID string) error
	// PruneOlderThan deletes every row created before cutoff and returns how
	// many went.
	PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

var _ TriggerLaunchStore = (*SQLiteTriggerLaunchStore)(nil)

// SQLiteTriggerLaunchStore implements TriggerLaunchStore using SQLite.
type SQLiteTriggerLaunchStore struct {
	db *sql.DB
}

// NewTriggerLaunchStore creates a new SQLite-backed TriggerLaunchStore.
func NewTriggerLaunchStore(db *sql.DB) *SQLiteTriggerLaunchStore {
	return &SQLiteTriggerLaunchStore{db: db}
}

// Get implements TriggerLaunchStore.Get.
func (s *SQLiteTriggerLaunchStore) Get(ctx context.Context, invocationID string) (*TriggerLaunch, error) {
	var (
		row       TriggerLaunch
		sessionID sql.NullString
		createdAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT invocation_id, trigger_id, session_id, created_at FROM trigger_launches WHERE invocation_id = ?`,
		invocationID,
	).Scan(&row.InvocationID, &row.TriggerID, &sessionID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger_launch: %w", err)
	}
	row.SessionID = sessionID.String
	row.CreatedAt = sqlutil.ParseTime(createdAt)
	return &row, nil
}

// Claim implements TriggerLaunchStore.Claim. The PRIMARY KEY arbitrates
// concurrent claims: SQLite serialises the writes, so exactly one INSERT lands
// and every other caller hits the conflict arm, whose guarded UPDATE only fires
// for an abandoned claim.
func (s *SQLiteTriggerLaunchStore) Claim(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (bool, error) {
	if invocationID == "" {
		return false, errors.New("claim trigger_launch: invocation id is required")
	}
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO trigger_launches (invocation_id, trigger_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(invocation_id) DO UPDATE SET trigger_id = excluded.trigger_id, created_at = excluded.created_at
		 WHERE trigger_launches.session_id IS NULL AND trigger_launches.created_at < ?`,
		invocationID, triggerID, now, sqlutil.FormatTime(staleBefore),
	)
	if err != nil {
		return false, fmt.Errorf("claim trigger_launch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim trigger_launch rows affected: %w", err)
	}
	return n == 1, nil
}

// SetSession implements TriggerLaunchStore.SetSession.
func (s *SQLiteTriggerLaunchStore) SetSession(ctx context.Context, invocationID, sessionID string) error {
	if sessionID == "" {
		return errors.New("set trigger_launch session: session id is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE trigger_launches SET session_id = ? WHERE invocation_id = ?`,
		sessionID, invocationID,
	)
	if err != nil {
		return fmt.Errorf("set trigger_launch session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set trigger_launch session rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("set trigger_launch session: no claim for invocation %q", invocationID)
	}
	return nil
}

// Release implements TriggerLaunchStore.Release.
func (s *SQLiteTriggerLaunchStore) Release(ctx context.Context, invocationID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM trigger_launches WHERE invocation_id = ? AND session_id IS NULL`,
		invocationID,
	); err != nil {
		return fmt.Errorf("release trigger_launch: %w", err)
	}
	return nil
}

// PruneOlderThan implements TriggerLaunchStore.PruneOlderThan.
func (s *SQLiteTriggerLaunchStore) PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM trigger_launches WHERE created_at < ?`,
		sqlutil.FormatTime(cutoff),
	)
	if err != nil {
		return 0, fmt.Errorf("prune trigger_launches: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune trigger_launches rows affected: %w", err)
	}
	return n, nil
}
