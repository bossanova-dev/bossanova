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
)

// ListBroadcastSubscriptionsFilter narrows BroadcastSubscriptionStore.List. All
// pointer fields are optional; a nil field is not constrained, and multiple
// fields intersect. Results are ordered by created_at then id for a stable,
// deterministic listing, mirroring ListBroadcastsFilter.
type ListBroadcastSubscriptionsFilter struct {
	// OwnerSessionID matches the session whose outcome fires the subscription.
	OwnerSessionID *string
	// OriginChatID matches the chat a subscription was registered from.
	// Operator-issued subscriptions have none and never match.
	OriginChatID *string
	// State matches the subscription's lifecycle state exactly.
	State *models.BroadcastSubscriptionState
	// TriggerEvent matches the trigger class exactly. It does NOT apply the
	// settled-matches-both rule — that is an evaluator policy, not a query one,
	// so a filter for `completed` returns only rows literally registered as
	// completed.
	TriggerEvent *models.BroadcastTrigger
	// Limit caps the number of rows returned. Zero or negative means unlimited.
	Limit int
}

// SQLiteBroadcastSubscriptionStore persists standing broadcast subscriptions: a
// rule that fires a broadcast when its owning session reaches an outcome
// matching the subscription's trigger.
//
// Two rules cut across the whole type.
//
// # Exactly-once firing
//
// MarkFired and Cancel are compare-and-swap writes guarded on state = 'active',
// and MarkFired's CAS is the single guard against a double fire. A session can
// be transitioned from more than one code path, and a reconcile sweep runs
// alongside them, so the caller must win the CAS *before* sending: only a true
// return may proceed to build a broadcast. Losing the CAS is a benign
// (false, nil) no-op, never an error — a loser has nothing to report but "someone
// else already fired this". An absent row is a different thing entirely and
// returns sql.ErrNoRows, so a caller can tell "already fired" from "no such id".
//
// # Secret body
//
// BroadcastSubscription.Message is a secret at rest. The store reads and returns
// it because it is the only source the firing path can get the body from — so
// redaction binds every surface built on top: any RPC, TUI, or inspect path
// returning a BroadcastSubscription must drop Message before it leaves the
// process. Nothing may copy the body into another column, a log line, or an
// error string; the store's own validation errors name the offending field, never
// its value. models.GithubCallback and SQLiteBroadcastStore set the same
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
// row on arrival: MarkFired and Cancel both CAS on active, so a subscription
// created fired, canceled or expired could never fire and would sit unreachable
// until its expiry.
func (s *SQLiteBroadcastSubscriptionStore) Create(ctx context.Context, sub *models.BroadcastSubscription) error {
	if sub == nil {
		return errors.New("create broadcast subscription: subscription is required")
	}
	if sub.State == "" {
		sub.State = models.BroadcastSubscriptionStateActive
	}
	if sub.State != models.BroadcastSubscriptionStateActive {
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
	var originVal any
	if sub.OriginChatID != nil {
		originVal = *sub.OriginChatID
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO broadcast_subscriptions
			(id, owner_session_id, origin_chat_id, trigger_event, selector, message,
			 state, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.OwnerSessionID, originVal, sub.TriggerEvent.String(), sub.Selector, sub.Message,
		sub.State.String(), sqlutil.FormatTime(sub.ExpiresAt),
		sqlutil.FormatTime(sub.CreatedAt), sqlutil.FormatTime(sub.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert broadcast subscription: %w", err)
	}
	return nil
}

// Get returns a subscription by id, or sql.ErrNoRows if absent.
func (s *SQLiteBroadcastSubscriptionStore) Get(ctx context.Context, id string) (*models.BroadcastSubscription, error) {
	row := s.db.QueryRowContext(ctx, broadcastSubscriptionSelectSQL+" WHERE id = ?", id)
	return scanBroadcastSubscription(row)
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
func (s *SQLiteBroadcastSubscriptionStore) ListActiveForSession(ctx context.Context, sessionID string) ([]models.BroadcastSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		broadcastSubscriptionSelectSQL+
			" WHERE owner_session_id = ? AND state = ? AND expires_at > ?"+
			" ORDER BY created_at ASC, id ASC",
		strings.TrimSpace(sessionID),
		models.BroadcastSubscriptionStateActive.String(),
		sqlutil.TimeNow(),
	)
	if err != nil {
		return nil, fmt.Errorf("list active broadcast subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []models.BroadcastSubscription
	for rows.Next() {
		sub, err := scanBroadcastSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

// List returns subscriptions matching filter, ordered by created_at then id.
func (s *SQLiteBroadcastSubscriptionStore) List(ctx context.Context, filter ListBroadcastSubscriptionsFilter) ([]*models.BroadcastSubscription, error) {
	var where []string
	var args []any
	// Both id filters are trimmed to match how Create normalises, so a caller can
	// round-trip the same string it passed in.
	if filter.OwnerSessionID != nil {
		where = append(where, "owner_session_id = ?")
		args = append(args, strings.TrimSpace(*filter.OwnerSessionID))
	}
	if filter.OriginChatID != nil {
		where = append(where, "origin_chat_id = ?")
		args = append(args, strings.TrimSpace(*filter.OriginChatID))
	}
	if filter.State != nil {
		where = append(where, "state = ?")
		args = append(args, filter.State.String())
	}
	if filter.TriggerEvent != nil {
		where = append(where, "trigger_event = ?")
		args = append(args, filter.TriggerEvent.String())
	}
	query := broadcastSubscriptionSelectSQL
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at ASC, id ASC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	// #nosec G202 -- WHERE is built from code-literal `col = ?` fragments; every
	// value is bound via ?, not concatenated user text; owner=@recurser; review-by=2027-01-26; issue=BOS-557
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list broadcast subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*models.BroadcastSubscription
	for rows.Next() {
		sub, err := scanBroadcastSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
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
// absent id (sql.ErrNoRows) or a real database failure returns non-nil.
//
// It is deliberately one-way. A send failure after a winning CAS leaves the row
// fired rather than rolling back to active: the broadcast's own delivery worker
// owns retry, and un-firing here would risk a duplicate fire on the next
// transition.
func (s *SQLiteBroadcastSubscriptionStore) MarkFired(ctx context.Context, id string, broadcastID string, now time.Time) (bool, error) {
	nowStr := sqlutil.FormatTime(now)
	var firedBroadcastVal any
	if trimmed := strings.TrimSpace(broadcastID); trimmed != "" {
		firedBroadcastVal = trimmed
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_subscriptions
		 SET state = ?, fired_at = ?, fired_broadcast_id = ?, updated_at = ?
		 WHERE id = ? AND state = ?`,
		models.BroadcastSubscriptionStateFired.String(), nowStr, firedBroadcastVal, nowStr,
		id, models.BroadcastSubscriptionStateActive.String(),
	)
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

// Cancel transitions a subscription active -> canceled and reports whether this
// caller won. Like MarkFired it is a CAS on active, so a repeated cancel is a
// benign (false, nil) no-op and a cancel racing a fire loses cleanly — the
// broadcast already went out, and flipping the row would misreport history.
// Returns sql.ErrNoRows if the id is absent.
func (s *SQLiteBroadcastSubscriptionStore) Cancel(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_subscriptions
		 SET state = ?, updated_at = ?
		 WHERE id = ? AND state = ?`,
		models.BroadcastSubscriptionStateCanceled.String(), sqlutil.TimeNow(),
		id, models.BroadcastSubscriptionStateActive.String(),
	)
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

// ExpireOverdue transitions every active subscription whose expires_at is at or
// before now to expired, and returns how many it retired.
//
// Only active rows are eligible: fired, canceled and expired are terminal, and a
// fired subscription's history must survive the sweep. Timestamps are stored in
// the fixed-width UTC layout, so the string comparison is chronological.
//
// This is the only reaper this table has. owner_session_id carries no foreign
// key, so a subscription whose session is deleted is not cascaded away — expiry
// is what stops it accumulating forever.
func (s *SQLiteBroadcastSubscriptionStore) ExpireOverdue(ctx context.Context, now time.Time) (int64, error) {
	nowStr := sqlutil.FormatTime(now)
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_subscriptions
		 SET state = ?, updated_at = ?
		 WHERE state = ? AND expires_at <= ?`,
		models.BroadcastSubscriptionStateExpired.String(), nowStr,
		models.BroadcastSubscriptionStateActive.String(), nowStr,
	)
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT owner_session_id FROM broadcast_subscriptions
		 WHERE state = ? AND expires_at > ?
		 ORDER BY owner_session_id ASC`,
		models.BroadcastSubscriptionStateActive.String(), sqlutil.TimeNow(),
	)
	if err != nil {
		return nil, fmt.Errorf("list active broadcast subscription owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan broadcast subscription owner: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// broadcastSubscriptionSelectSQL includes the secret message column
// deliberately; see the secret-body rule on SQLiteBroadcastSubscriptionStore for
// why, and for what that obliges of every surface built on top.
const broadcastSubscriptionSelectSQL = `SELECT id, owner_session_id, origin_chat_id, trigger_event,
	selector, message, state, fired_at, fired_broadcast_id, expires_at, created_at, updated_at
	FROM broadcast_subscriptions`

// scanBroadcastSubscription parses one broadcast_subscriptions row. Both the
// state and trigger columns go through their Parse helpers rather than a bare
// conversion so a value outside the vocabulary is a loud error at the read
// boundary — an unrecognised trigger would otherwise match no class and silently
// never fire, which is the exact failure a standing subscription must not have.
func scanBroadcastSubscription(sc sqlutil.Scanner) (*models.BroadcastSubscription, error) {
	var sub models.BroadcastSubscription
	var state, trigger string
	var originChatID, firedBroadcastID, firedAt sql.NullString
	var expiresAt, createdAt, updatedAt string
	if err := sc.Scan(
		&sub.ID, &sub.OwnerSessionID, &originChatID, &trigger,
		&sub.Selector, &sub.Message, &state, &firedAt, &firedBroadcastID,
		&expiresAt, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	parsedState, err := models.ParseBroadcastSubscriptionState(state)
	if err != nil {
		return nil, fmt.Errorf("broadcast subscription %s: %w", sub.ID, err)
	}
	sub.State = parsedState
	parsedTrigger, err := models.ParseBroadcastTrigger(trigger)
	if err != nil {
		return nil, fmt.Errorf("broadcast subscription %s: %w", sub.ID, err)
	}
	sub.TriggerEvent = parsedTrigger
	if originChatID.Valid {
		o := originChatID.String
		sub.OriginChatID = &o
	}
	if firedBroadcastID.Valid {
		b := firedBroadcastID.String
		sub.FiredBroadcastID = &b
	}
	sub.FiredAt = optionalTime(firedAt)
	sub.ExpiresAt = sqlutil.ParseTime(expiresAt)
	sub.CreatedAt = sqlutil.ParseTime(createdAt)
	sub.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &sub, nil
}
