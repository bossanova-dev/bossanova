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
	// AcceptedSessionID records a create accepted by the daemon, before bootstrap settles.
	AcceptedSessionID string
	CreatedAt         time.Time
}

// TriggerLaunchStore persists LaunchTriggerSessionCommand idempotency rows.
type TriggerLaunchStore interface {
	// Get returns the row for invocationID, or (nil, nil) when none exists.
	Get(ctx context.Context, invocationID string) (*TriggerLaunch, error)
	// Claim returns an owner token when it wins an in-flight claim. Only an
	// unaccepted, session-less row created before staleBefore can be taken over.
	Claim(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (token string, won bool, err error)
	// SetSession records the session, fenced by the winning claim token.
	SetSession(ctx context.Context, invocationID, token, sessionID string) error
	// Release deletes this owner's unsettled claim, even after acceptance, so a
	// failed bootstrap can be retried. A finished launch is never released.
	Release(ctx context.Context, invocationID, token string) error
	// MarkAccepted durably records the accepted session, fenced by the owner token.
	MarkAccepted(ctx context.Context, invocationID, token, sessionID string) error
	// PromoteAccepted records a stale accepted claim as finished without taking
	// ownership. Accepted claims are never taken over by Claim.
	PromoteAccepted(ctx context.Context, invocationID string, staleBefore time.Time) (bool, error)
	// PruneOlderThan deletes every row created before cutoff and returns how
	// many went.
	PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// ErrTriggerLaunchClaimLost means an owner write no longer matches its claim.
var ErrTriggerLaunchClaimLost = errors.New("trigger launch claim lost")

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
		row               TriggerLaunch
		sessionID         sql.NullString
		acceptedSessionID sql.NullString
		createdAt         string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT invocation_id, trigger_id, session_id, accepted_session_id, created_at FROM trigger_launches WHERE invocation_id = ?`,
		invocationID,
	).Scan(&row.InvocationID, &row.TriggerID, &sessionID, &acceptedSessionID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger_launch: %w", err)
	}
	row.SessionID = sessionID.String
	row.AcceptedSessionID = acceptedSessionID.String
	row.CreatedAt = sqlutil.ParseTime(createdAt)
	return &row, nil
}

// Claim implements TriggerLaunchStore.Claim. The PRIMARY KEY arbitrates
// concurrent claims: SQLite serialises the writes, so exactly one INSERT lands
// and every other caller hits the conflict arm, whose guarded UPDATE only fires
// for an abandoned claim.
func (s *SQLiteTriggerLaunchStore) Claim(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (string, bool, error) {
	if invocationID == "" {
		return "", false, errors.New("claim trigger_launch: invocation id is required")
	}
	token, err := sqlutil.NewID()
	if err != nil {
		return "", false, fmt.Errorf("mint trigger_launch claim token: %w", err)
	}
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO trigger_launches (invocation_id, trigger_id, created_at, claim_token) VALUES (?, ?, ?, ?)
		 ON CONFLICT(invocation_id) DO UPDATE SET trigger_id = excluded.trigger_id, created_at = excluded.created_at, claim_token = excluded.claim_token
		 WHERE trigger_launches.session_id IS NULL AND trigger_launches.accepted_session_id IS NULL AND trigger_launches.created_at < ?`,
		invocationID, triggerID, now, token, sqlutil.FormatTime(staleBefore),
	)
	if err != nil {
		return "", false, fmt.Errorf("claim trigger_launch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("claim trigger_launch rows affected: %w", err)
	}
	if n == 0 {
		return "", false, nil
	}
	return token, true, nil
}

// SetSession implements TriggerLaunchStore.SetSession.
func (s *SQLiteTriggerLaunchStore) SetSession(ctx context.Context, invocationID, token, sessionID string) error {
	if sessionID == "" {
		return errors.New("set trigger_launch session: session id is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE trigger_launches SET session_id = ? WHERE invocation_id = ? AND claim_token = ?`,
		sessionID, invocationID, token,
	)
	if err != nil {
		return fmt.Errorf("set trigger_launch session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set trigger_launch session rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("set trigger_launch session for invocation %q: %w", invocationID, ErrTriggerLaunchClaimLost)
	}
	return nil
}

// Release implements TriggerLaunchStore.Release.
func (s *SQLiteTriggerLaunchStore) Release(ctx context.Context, invocationID, token string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM trigger_launches WHERE invocation_id = ? AND claim_token = ? AND session_id IS NULL`,
		invocationID, token,
	); err != nil {
		return fmt.Errorf("release trigger_launch: %w", err)
	}
	return nil
}

// MarkAccepted implements TriggerLaunchStore.MarkAccepted.
func (s *SQLiteTriggerLaunchStore) MarkAccepted(ctx context.Context, invocationID, token, sessionID string) error {
	if sessionID == "" {
		return errors.New("mark trigger_launch accepted: session id is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE trigger_launches SET accepted_session_id = ? WHERE invocation_id = ? AND claim_token = ? AND session_id IS NULL`,
		sessionID, invocationID, token)
	if err != nil {
		return fmt.Errorf("mark trigger_launch accepted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark trigger_launch accepted rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark trigger_launch accepted for invocation %q: %w", invocationID, ErrTriggerLaunchClaimLost)
	}
	return nil
}

// PromoteAccepted implements TriggerLaunchStore.PromoteAccepted.
func (s *SQLiteTriggerLaunchStore) PromoteAccepted(ctx context.Context, invocationID string, staleBefore time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE trigger_launches SET session_id = accepted_session_id WHERE invocation_id = ? AND session_id IS NULL AND accepted_session_id IS NOT NULL AND created_at < ?`,
		invocationID, sqlutil.FormatTime(staleBefore))
	if err != nil {
		return false, fmt.Errorf("promote trigger_launch accepted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("promote trigger_launch accepted rows affected: %w", err)
	}
	return n == 1, nil
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
