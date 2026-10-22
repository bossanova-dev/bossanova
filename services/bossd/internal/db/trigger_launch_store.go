package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// TriggerLaunchStore persists LaunchTriggerSessionCommand idempotency rows.
type TriggerLaunchStore interface {
	// Get returns the row for invocationID, or (nil, nil) when none exists.
	Get(ctx context.Context, invocationID string) (*domain.TriggerLaunch, error)
	// Upsert returns an owner token when it wins an in-flight claim. Only an
	// unaccepted, session-less row created before staleBefore can be taken over.
	Upsert(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (token string, won bool, err error)
	// SetSession records the session, fenced by the winning claim token.
	SetSession(ctx context.Context, invocationID, token, sessionID string) error
	// Delete deletes this owner's unsettled claim, even after acceptance, so a
	// failed bootstrap can be retried. A finished launch is never released.
	Delete(ctx context.Context, invocationID, token string) error
	// MarkAccepted durably records the accepted session, fenced by the owner token.
	MarkAccepted(ctx context.Context, invocationID, token, sessionID string) error
	// MarkFinished records a stale accepted claim as finished without taking
	// ownership. Accepted claims are never taken over by Upsert.
	MarkFinished(ctx context.Context, invocationID string, staleBefore time.Time) (bool, error)
	// DeleteOlderThan deletes every row created before cutoff and returns how
	// many went.
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
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
func (s *SQLiteTriggerLaunchStore) Get(ctx context.Context, invocationID string) (*domain.TriggerLaunch, error) {
	row, err := sqlcgen.New(s.db).GetTriggerLaunch(ctx, invocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger_launch: %w", err)
	}
	return triggerLaunchFromRow(row), nil
}

// Upsert implements TriggerLaunchStore.Upsert. The PRIMARY KEY arbitrates
// concurrent claims: SQLite serialises the writes, so exactly one INSERT lands
// and the guarded SELECT admits only a new invocation or an abandoned claim
// before the conflict arm replaces its owner token.
func (s *SQLiteTriggerLaunchStore) Upsert(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (string, bool, error) {
	if invocationID == "" {
		return "", false, errors.New("claim trigger_launch: invocation id is required")
	}
	token, err := sqlutil.NewID()
	if err != nil {
		return "", false, fmt.Errorf("mint trigger_launch claim token: %w", err)
	}
	now := sqlutil.TimeNow()
	n, err := sqlcgen.New(s.db).UpsertTriggerLaunch(ctx, sqlcgen.UpsertTriggerLaunchParams{InvocationID: invocationID, TriggerID: triggerID, CreatedAt: now, ClaimToken: token, StaleBefore: sqlutil.FormatTime(staleBefore)})
	if err != nil {
		return "", false, fmt.Errorf("claim trigger_launch: %w", err)
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
	n, err := sqlcgen.New(s.db).SetTriggerLaunchSession(ctx, sqlcgen.SetTriggerLaunchSessionParams{SessionID: &sessionID, InvocationID: invocationID, ClaimToken: token})
	if err != nil {
		return fmt.Errorf("set trigger_launch session: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("set trigger_launch session for invocation %q: %w", invocationID, ErrTriggerLaunchClaimLost)
	}
	return nil
}

// Delete implements TriggerLaunchStore.Delete.
func (s *SQLiteTriggerLaunchStore) Delete(ctx context.Context, invocationID, token string) error {
	if err := sqlcgen.New(s.db).DeleteTriggerLaunch(ctx, sqlcgen.DeleteTriggerLaunchParams{InvocationID: invocationID, ClaimToken: token}); err != nil {
		return fmt.Errorf("release trigger_launch: %w", err)
	}
	return nil
}

// MarkAccepted implements TriggerLaunchStore.MarkAccepted.
func (s *SQLiteTriggerLaunchStore) MarkAccepted(ctx context.Context, invocationID, token, sessionID string) error {
	if sessionID == "" {
		return errors.New("mark trigger_launch accepted: session id is required")
	}
	n, err := sqlcgen.New(s.db).MarkTriggerLaunchAccepted(ctx, sqlcgen.MarkTriggerLaunchAcceptedParams{AcceptedSessionID: &sessionID, InvocationID: invocationID, ClaimToken: token})
	if err != nil {
		return fmt.Errorf("mark trigger_launch accepted: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark trigger_launch accepted for invocation %q: %w", invocationID, ErrTriggerLaunchClaimLost)
	}
	return nil
}

// MarkFinished implements TriggerLaunchStore.MarkFinished.
func (s *SQLiteTriggerLaunchStore) MarkFinished(ctx context.Context, invocationID string, staleBefore time.Time) (bool, error) {
	n, err := sqlcgen.New(s.db).MarkTriggerLaunchFinished(ctx, sqlcgen.MarkTriggerLaunchFinishedParams{InvocationID: invocationID, StaleBefore: sqlutil.FormatTime(staleBefore)})
	if err != nil {
		return false, fmt.Errorf("promote trigger_launch accepted: %w", err)
	}
	return n == 1, nil
}

// DeleteOlderThan implements TriggerLaunchStore.DeleteOlderThan.
func (s *SQLiteTriggerLaunchStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := sqlcgen.New(s.db).DeleteTriggerLaunches(ctx, sqlutil.FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune trigger_launches: %w", err)
	}
	return n, nil
}

func triggerLaunchFromRow(row sqlcgen.GetTriggerLaunchRow) *domain.TriggerLaunch {
	return &domain.TriggerLaunch{InvocationID: row.InvocationID, TriggerID: row.TriggerID, SessionID: textValue(row.SessionID), AcceptedSessionID: textValue(row.AcceptedSessionID), CreatedAt: textTime(row.CreatedAt)}
}
