package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// ListBroadcastSubscriptionsFilter narrows BroadcastSubscriptionStore.List. All
// pointer fields are optional; a nil field is not constrained, and multiple
// fields intersect. Results are ordered by created_at then id for a stable,
// deterministic listing, mirroring ListBroadcastsFilter.
type ListBroadcastSubscriptionsFilter = domain.ListBroadcastSubscriptionsFilter

// SQLiteBroadcastSubscriptionStore persists standing broadcast subscriptions: a
// rule that fires a broadcast when its owning session reaches an outcome
// matching the subscription's trigger.
//
// Two rules cut across the whole type.
//
// # Exactly-once firing
//
// MarkFired and MarkCanceled are compare-and-swap writes guarded on state = 'active',
// and MarkFired's CAS is the single guard against a double fire. A session can
// be transitioned from more than one code path, and a reconcile sweep runs
// alongside them, so the caller must win the CAS *before* sending: only a true
// return may proceed to build a broadcast. Losing the CAS is a benign
// (false, nil) no-op, never an error — a loser has nothing to report but "someone
// else already fired this". An absent row is a different thing entirely and
// returns domain.ErrNotFound, so a caller can tell "already fired" from "no such id".
//
// # Secret body
//
// BroadcastSubscription.Message is a secret at rest. The store reads and returns
// it because it is the only source the firing path can get the body from — so
// redaction binds every surface built on top: any RPC, TUI, or inspect path
// returning a BroadcastSubscription must drop Message before it leaves the
// process. Nothing may copy the body into another column, a log line, or an
// error string; the store's own validation errors name the offending field, never
// its value. domain.GitHubCallback and SQLiteBroadcastStore set the same
// precedent.
type SQLiteBroadcastSubscriptionStore struct {
	db *sql.DB
}

// NewBroadcastSubscriptionStore creates a new SQLite-backed subscription store.
func NewBroadcastSubscriptionStore(db *sql.DB) *SQLiteBroadcastSubscriptionStore {
	return &SQLiteBroadcastSubscriptionStore{db: db}
}

// Create inserts a subscription row. It stamps a generated id and
// created/updated timestamps onto sub when they are unset (an explicit CreatedAt
// is honoured so callers can backfill), and defaults the state to active.
//
// active is the only state a subscription may be created in. Any other is a dead
// row on arrival: MarkFired and MarkCanceled both CAS on active, so a subscription
// created fired, canceled or expired could never fire and would sit unreachable
// until its expiry.
func (s *SQLiteBroadcastSubscriptionStore) Create(ctx context.Context, sub *domain.BroadcastSubscription) error {
	if sub == nil {
		return errors.New("create broadcast subscription: subscription is required")
	}
	if sub.State == "" {
		sub.State = domain.BroadcastSubscriptionStateActive
	}
	if sub.State != domain.BroadcastSubscriptionStateActive {
		return fmt.Errorf("create broadcast subscription: state %q: a subscription must be created active", sub.State)
	}
	if strings.TrimSpace(sub.OwnerSessionID) == "" {
		return errors.New("create broadcast subscription: owner session id is required")
	}
	if !sub.TriggerEvent.Valid() {
		return fmt.Errorf("create broadcast subscription: %w: %q",
			models.ErrUnknownBroadcastTrigger, sub.TriggerEvent)
	}
	if strings.TrimSpace(sub.Selector) == "" {
		return errors.New("create broadcast subscription: selector is required")
	}
	// Deliberately reports the field, not the value: the body is a secret and an
	// error string is a log line waiting to happen. See the secret-body rule.
	if strings.TrimSpace(sub.Message) == "" {
		return errors.New("create broadcast subscription: message is required")
	}
	if sub.ExpiresAt.IsZero() {
		return errors.New("create broadcast subscription: expires_at is required")
	}
	if sub.ID == "" {
		id, err := sqlutil.NewID()
		if err != nil {
			return fmt.Errorf("new broadcast subscription id: %w", err)
		}
		sub.ID = id
	}

	now := time.Now().UTC()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}
	if sub.UpdatedAt.IsZero() {
		sub.UpdatedAt = sub.CreatedAt
	}

	// Normalise in memory as well as on the way to the row, for the reason
	// SQLiteBroadcastStore.Create does: a padded owner id would persist padded and
	// then miss every owner_session_id = ? lookup, including the evaluator's.
	sub.OwnerSessionID = strings.TrimSpace(sub.OwnerSessionID)
	if sub.OriginChatID != nil {
		o := strings.TrimSpace(*sub.OriginChatID)
		if o == "" {
			sub.OriginChatID = nil
		} else {
			sub.OriginChatID = &o
		}
	}

	if _, err := sqlcgen.New(s.db).CreateBroadcastSubscription(ctx, sqlcgen.CreateBroadcastSubscriptionParams{
		ID:             sub.ID,
		OwnerSessionID: sub.OwnerSessionID,
		OriginChatID:   sub.OriginChatID,
		TriggerEvent:   sub.TriggerEvent.String(),
		Selector:       sub.Selector,
		Message:        sub.Message,
		State:          sub.State.String(),
		ExpiresAt:      sqlutil.FormatTime(sub.ExpiresAt),
		CreatedAt:      sqlutil.FormatTime(sub.CreatedAt),
		UpdatedAt:      sqlutil.FormatTime(sub.UpdatedAt),
	}); err != nil {
		return fmt.Errorf("insert broadcast subscription: %w", err)
	}
	return nil
}

// Get returns a subscription by id, or domain.ErrNotFound if absent.
func (s *SQLiteBroadcastSubscriptionStore) Get(ctx context.Context, id string) (*domain.BroadcastSubscription, error) {
	row, err := sqlcgen.New(s.db).GetBroadcastSubscription(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return broadcastSubscriptionFromRow(row)
}

// ListActiveForSession returns the live subscriptions owned by sessionID —
// state = active and not yet past expiry — in creation order. It is the
// evaluator's hot path, run on every classified session transition.
//
// The expiry predicate is evaluated here rather than left to the sweep because
// the sweep is lazy: between a subscription's expiry and the tick that retires
// it, the row is still literally `active`, and a stale registration must never
// fire.
//
// It returns values rather than pointers: the result is a read-only snapshot the
// evaluator iterates and fires from, and copying removes any question of a
// caller mutating rows the store handed out.
func (s *SQLiteBroadcastSubscriptionStore) ListActiveForSession(ctx context.Context, sessionID string) ([]domain.BroadcastSubscription, error) {
	rows, err := sqlcgen.New(s.db).ListActiveBroadcastSubscriptionsBySession(ctx, sqlcgen.ListActiveBroadcastSubscriptionsBySessionParams{
		OwnerSessionID: strings.TrimSpace(sessionID), State: domain.BroadcastSubscriptionStateActive.String(), ExpiresAt: sqlutil.TimeNow()})
	if err != nil {
		return nil, fmt.Errorf("list active broadcast subscriptions: %w", err)
	}
	var out []domain.BroadcastSubscription
	for _, row := range rows {
		sub, err := broadcastSubscriptionFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, nil
}

// List returns subscriptions matching filter, ordered by created_at then id.
func (s *SQLiteBroadcastSubscriptionStore) List(ctx context.Context, filter ListBroadcastSubscriptionsFilter) ([]*domain.BroadcastSubscription, error) {
	arg := sqlcgen.ListBroadcastSubscriptionsParams{RowLimit: int64(filter.Limit)}
	if arg.RowLimit <= 0 {
		arg.RowLimit = -1
	}
	if filter.State != nil {
		value := filter.State.String()
		arg.State = &value
	}
	if filter.TriggerEvent != nil {
		value := filter.TriggerEvent.String()
		arg.TriggerEvent = &value
	}
	if filter.OriginChatID != nil {
		value := strings.TrimSpace(*filter.OriginChatID)
		arg.OriginChatID = &value
	}
	if filter.OwnerSessionID != nil {
		value := strings.TrimSpace(*filter.OwnerSessionID)
		arg.OwnerSessionID = &value
	}
	rows, err := sqlcgen.New(s.db).ListBroadcastSubscriptions(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("list broadcast subscriptions: %w", err)
	}
	var out []*domain.BroadcastSubscription
	for _, row := range rows {
		sub, err := broadcastSubscriptionFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

// MarkFired transitions a subscription active -> fired, recording the firing
// time and the broadcast the caller is about to send, and reports whether this
// caller won.
//
// This is THE guard against a double fire — see the exactly-once rule on
// SQLiteBroadcastSubscriptionStore. Callers must CAS first and send only on a
// true return; the reverse order sends twice and marks once.
//
// A losing CAS is (false, nil): benign, expected, and not an error. Only an
// absent id (domain.ErrNotFound) or a real database failure returns non-nil.
//
// It is deliberately one-way. A send failure after a winning CAS leaves the row
// fired rather than rolling back to active: the broadcast's own delivery worker
// owns retry, and un-firing here would risk a duplicate fire on the next
// transition.
func (s *SQLiteBroadcastSubscriptionStore) MarkFired(ctx context.Context, id string, broadcastID string, now time.Time) (bool, error) {
	nowStr := sqlutil.FormatTime(now)
	var firedBroadcastVal *string
	if trimmed := strings.TrimSpace(broadcastID); trimmed != "" {
		firedBroadcastVal = &trimmed
	}
	res, err := sqlcgen.New(s.db).MarkBroadcastSubscriptionFired(ctx, sqlcgen.MarkBroadcastSubscriptionFiredParams{
		State:            domain.BroadcastSubscriptionStateFired.String(),
		FiredAt:          &nowStr,
		FiredBroadcastID: firedBroadcastVal,
		UpdatedAt:        nowStr,
		ID:               id,
		ActiveState:      domain.BroadcastSubscriptionStateActive.String(),
	})
	if err != nil {
		return false, fmt.Errorf("mark broadcast subscription fired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark broadcast subscription fired: %w", err)
	}
	if n == 0 {
		// Separate "already fired/canceled/expired" (a lost CAS, benign) from "no
		// such subscription" (a caller bug the store must not swallow).
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return false, gerr
		}
		return false, nil
	}
	return true, nil
}

// MarkCanceled transitions a subscription active -> canceled and reports whether this
// caller won. Like MarkFired it is a CAS on active, so a repeated cancel is a
// benign (false, nil) no-op and a cancel racing a fire loses cleanly — the
// broadcast already went out, and flipping the row would misreport history.
// Returns domain.ErrNotFound if the id is absent.
func (s *SQLiteBroadcastSubscriptionStore) MarkCanceled(ctx context.Context, id string) (bool, error) {
	res, err := sqlcgen.New(s.db).MarkBroadcastSubscriptionCanceled(ctx, sqlcgen.MarkBroadcastSubscriptionCanceledParams{
		State:       domain.BroadcastSubscriptionStateCanceled.String(),
		UpdatedAt:   sqlutil.TimeNow(),
		ID:          id,
		ActiveState: domain.BroadcastSubscriptionStateActive.String(),
	})
	if err != nil {
		return false, fmt.Errorf("cancel broadcast subscription: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cancel broadcast subscription: %w", err)
	}
	if n == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return false, gerr
		}
		return false, nil
	}
	return true, nil
}

// MarkExpired transitions every active subscription whose expires_at is at or
// before now to expired, and returns how many it retired.
//
// Only active rows are eligible: fired, canceled and expired are terminal, and a
// fired subscription's history must survive the sweep. Timestamps are stored in
// the fixed-width UTC layout, so the string comparison is chronological.
//
// This is the only reaper this table has. owner_session_id carries no foreign
// key, so a subscription whose session is deleted is not cascaded away — expiry
// is what stops it accumulating forever.
func (s *SQLiteBroadcastSubscriptionStore) MarkExpired(ctx context.Context, now time.Time) (int64, error) {
	nowStr := sqlutil.FormatTime(now)
	res, err := sqlcgen.New(s.db).MarkBroadcastSubscriptionsExpired(ctx, sqlcgen.MarkBroadcastSubscriptionsExpiredParams{
		State:       domain.BroadcastSubscriptionStateExpired.String(),
		UpdatedAt:   nowStr,
		ActiveState: domain.BroadcastSubscriptionStateActive.String(),
		ExpiresAt:   nowStr,
	})
	if err != nil {
		return 0, fmt.Errorf("expire overdue broadcast subscriptions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("expire overdue broadcast subscriptions: %w", err)
	}
	return n, nil
}

// ListActiveOwnerSessionIDs returns the distinct sessions that still carry a
// live (active, not past expiry) subscription, in ascending id order.
//
// It exists for the reconcile sweep: after a daemon restart, a session may have
// reached a trigger state while nothing was listening, so the sweep re-checks
// exactly these sessions rather than every session in the database.
func (s *SQLiteBroadcastSubscriptionStore) ListActiveOwnerSessionIDs(ctx context.Context) ([]string, error) {
	rows, err := sqlcgen.New(s.db).ListActiveBroadcastSubscriptionOwnerSessionIDs(ctx, sqlcgen.ListActiveBroadcastSubscriptionOwnerSessionIDsParams{State: domain.BroadcastSubscriptionStateActive.String(), ExpiresAt: sqlutil.TimeNow()})
	if err != nil {
		return nil, fmt.Errorf("list active broadcast subscription owners: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows, nil
}

// broadcastSubscriptionFromRow rejects unknown state and trigger vocabulary.
func broadcastSubscriptionFromRow(row sqlcgen.BroadcastSubscription) (*domain.BroadcastSubscription, error) {
	state, err := domain.ParseBroadcastSubscriptionState(row.State)
	if err != nil {
		return nil, fmt.Errorf("broadcast subscription %s: %w", row.ID, err)
	}
	trigger, err := models.ParseBroadcastTrigger(row.TriggerEvent)
	if err != nil {
		return nil, fmt.Errorf("broadcast subscription %s: %w", row.ID, err)
	}
	return &domain.BroadcastSubscription{ID: row.ID, OwnerSessionID: row.OwnerSessionID, OriginChatID: row.OriginChatID,
		TriggerEvent: trigger, Selector: row.Selector, Message: row.Message, State: state, FiredAt: optionalTextTime(row.FiredAt),
		FiredBroadcastID: row.FiredBroadcastID, ExpiresAt: textTime(row.ExpiresAt), CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt)}, nil
}
