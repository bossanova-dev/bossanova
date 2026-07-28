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

// ErrBroadcastDeliveryLeaseConflict is returned by AcquireLease when a delivery
// is not claimable — another owner holds an unexpired lease, the retry backoff
// has not elapsed, the row is already terminal, or its parent broadcast is past
// expiry — and by MarkDelivered/ScheduleRetry when the caller's lease no longer
// matches (it was recovered after expiry or the row advanced). Callers treat it
// as a benign lost claim, not a hard failure. It is the broadcast counterpart of
// ErrGithubCallbackLeaseConflict and provides the same contract.
var ErrBroadcastDeliveryLeaseConflict = errors.New("broadcast delivery lease held by another owner")

// ErrBroadcastAlreadyResolved is returned by CreateDeliveries when the target
// broadcast is not pending — its audience was already materialised, or it went
// terminal (expired/canceled/completed) first.
//
// Resolution is deliberately one-shot. Without that guard a repeated resolve —
// an at-least-once RPC, a re-queued job, a daemon that crashed after the commit
// but before the caller recorded success — would append a second full set of
// delivery rows and overwrite target_count with just the new batch's length.
// Every duplicate row is independently claimable, so a wired worker would send
// the message to every target twice. Failing loudly is the only safe answer.
//
// It covers both "already resolved" (a benign at-least-once retry) and "went
// terminal first" (the audience was never materialised and never will be).
// Both mean "stop trying"; the message names the offending state, and a caller
// that must distinguish them should Get the broadcast.
var ErrBroadcastAlreadyResolved = errors.New("broadcast audience already resolved")

// ListBroadcastsFilter narrows BroadcastStore.List. All pointer fields are
// optional; a nil field is not constrained, and multiple fields intersect.
// Results are ordered by created_at then id for a stable, deterministic listing,
// mirroring ListGithubCallbacksFilter.
type ListBroadcastsFilter struct {
	// State matches the broadcast's lifecycle state exactly.
	State *models.BroadcastState
	// OriginChatID matches the chat a broadcast was issued from. Operator-issued
	// broadcasts have none and never match.
	OriginChatID *string
	// TargetChatID matches through broadcast_deliveries: "which broadcasts were
	// addressed to this chat".
	TargetChatID *string
	// Limit caps the number of rows returned. Zero or negative means unlimited.
	Limit int
}

// ScheduleBroadcastRetryParams carries retry bookkeeping for a claimed delivery
// whose send failed.
//
// LastError is a diagnostic string and must never contain the broadcast message
// body — see the secret-body rule on SQLiteBroadcastStore, which the callback
// worker states identically at services/bossd/internal/callback/worker.go.
type ScheduleBroadcastRetryParams struct {
	// NextAttemptAt is when the delivery becomes claimable again. A zero value
	// writes NULL, which means "no backoff" — claimable on the very next tick.
	// Callers wanting a delay must set it.
	NextAttemptAt time.Time
	// LastError is a short diagnostic. Empty clears the column.
	LastError string
}

// SQLiteBroadcastStore persists broadcasts and their per-target deliveries.
//
// Delivery state transitions are compare-and-swap guarded so concurrent
// delivery workers cannot double-send one target, and so a claim held by a
// worker that died can be recovered once its lease deadline passes. The lease
// method set is deliberately shaped as a superset of callback.workerStore with
// the broadcast types substituted, so a delivery worker can be written against
// it unchanged.
//
// Three rules cut across the whole type. They are stated once here; the methods
// that depend on one point back rather than restate it.
//
// # Secret body
//
// Broadcast.Message is a secret at rest. The store reads and returns it because
// it is the layer the delivery path gets the body from and there is no other
// source — so redaction binds every surface built on top: any RPC, TUI, or
// inspect path returning a Broadcast must drop Message before it leaves the
// process. Nothing may copy the body into a delivery column, least of all the
// last_error diagnostic. models.GithubCallback sets the same precedent.
//
// # Lease ownership
//
// The owner string IS the fencing token. Callers must make it non-empty and
// unique per acquisition, not a stable per-worker id:
//
//	attempt, err := sqlutil.NewID() // returns an error; it does not panic
//	if err != nil {
//		return err
//	}
//	owner := "worker-3:" + attempt
//
// MarkDelivered and ScheduleRetry carry no fence beyond lease_owner, and
// AcquireLease deliberately lets the current owner re-acquire, so a stable id
// lets a hung attempt whose lease expired come back and settle the *newer*
// claim that replaced it. A per-acquisition owner makes that CAS miss, which is
// the whole point of the conflict error. The store cannot enforce it: it cannot
// tell a re-acquisition from a resumed zombie when both present the same
// string; it only rejects the empty owner, which would void the fence outright.
// (callback.DeliveryWorker uses a stable owner and inherits the same hazard.)
//
// A consequence that reads like a contradiction: under that contract the
// `lease_owner = ?` arm of AcquireLease's claimability predicate can never
// match, since a fresh per-acquisition string never equals the stored one. It
// stays because the method shape is pinned to callback.workerStore, and it
// serves a stable-owner caller renewing its own lease. Inert, not wrong.
//
// # At-least-once delivery
//
// A delivery is sent before it is recorded, so the two can diverge. An expiry
// sweep landing in that gap retires the row as skipped even though the message
// went out. skipped therefore means "no delivery was recorded", never "nothing
// was sent" — an under-report, never a duplicate, because nothing resends a
// skipped row.
type SQLiteBroadcastStore struct {
	db *sql.DB
}

// validateLeaseOwner rejects an owner that cannot act as a fencing token. An
// empty owner is the dangerous case: every row a blank owner claims looks
// claimable to the next blank owner, so two workers hold it at once.
func validateLeaseOwner(owner string) error {
	if strings.TrimSpace(owner) == "" {
		return errors.New("lease owner is required")
	}
	return nil
}

// NewBroadcastStore creates a new SQLite-backed broadcast store.
func NewBroadcastStore(db *sql.DB) *SQLiteBroadcastStore {
	return &SQLiteBroadcastStore{db: db}
}

// Create inserts a broadcast row. It stamps a generated id and created/updated
// timestamps onto b when they are unset (an explicit CreatedAt is honoured so
// callers can backfill), and defaults the state to pending.
//
// pending is the only state a broadcast may be created in. Any other is a dead
// row on arrival: CreateDeliveries resolves only a pending broadcast, so a
// broadcast created resolved or completed could never have an audience
// materialised and would sit unreachable forever.
func (s *SQLiteBroadcastStore) Create(ctx context.Context, b *models.Broadcast) error {
	if b == nil {
		return errors.New("create broadcast: broadcast is required")
	}
	if b.State == "" {
		b.State = models.BroadcastStatePending
	}
	if b.State != models.BroadcastStatePending {
		return fmt.Errorf("create broadcast: state %q: a broadcast must be created pending", b.State)
	}
	if strings.TrimSpace(b.Selector) == "" {
		return errors.New("create broadcast: selector is required")
	}
	if strings.TrimSpace(b.Message) == "" {
		return errors.New("create broadcast: message is required")
	}
	if b.ExpiresAt.IsZero() {
		return errors.New("create broadcast: expires_at is required")
	}
	if b.ID == "" {
		id, err := sqlutil.NewID()
		if err != nil {
			return fmt.Errorf("new broadcast id: %w", err)
		}
		b.ID = id
	}

	now := time.Now().UTC()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = b.CreatedAt
	}

	// Normalise in memory as well as on the way to the row: a blank origin is
	// stored as NULL, so leaving b.OriginChatID pointing at "  " would hand the
	// caller a struct that disagrees with what Get returns.
	if b.OriginChatID != nil {
		o := strings.TrimSpace(*b.OriginChatID)
		if o == "" {
			b.OriginChatID = nil
		} else {
			b.OriginChatID = &o
		}
	}
	var originVal any
	if b.OriginChatID != nil {
		originVal = *b.OriginChatID
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO broadcasts
			(id, origin_chat_id, selector, message, state, target_count, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, originVal, b.Selector, b.Message, b.State.String(), b.TargetCount,
		sqlutil.FormatTime(b.ExpiresAt), sqlutil.FormatTime(b.CreatedAt), sqlutil.FormatTime(b.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert broadcast: %w", err)
	}
	return nil
}

// CreateDeliveries materialises a broadcast's resolved audience in one
// transaction: it stamps target_count, promotes the broadcast pending ->
// resolved, then inserts one row per target.
//
// The whole batch is atomic on purpose. A half-written audience would be
// indistinguishable from a fully resolved one — the worker would deliver to a
// subset and CompleteIfSettled would mark the broadcast complete — so a failure
// part-way must leave zero delivery rows and an unchanged target_count. The
// count is written before the inserts precisely so the rollback of a failed
// insert has to undo it too.
//
// It is also one-shot: the stamping UPDATE is guarded on state = pending, so a
// broadcast's audience can be materialised exactly once. See
// ErrBroadcastAlreadyResolved for why a second resolve must fail rather than
// append.
//
// Only the addressing fields of each target are honoured — ID (generated when
// blank), TargetChatID, TargetDaemonID and State. The lease, backoff and
// timestamp fields are the store's to own, and are always written fresh: a
// resolver cannot pre-seed an attempt count or a lease. State may only be
// pending (the default) or skipped, for a target already known to be gone at
// resolve time; seeding an in-flight or delivered state would let a broadcast
// complete over a row that was never sent.
//
// The batch must address each chat at most once. Two rows for one chat are two
// independently claimable deliveries, so the CAS lease — which guards a row,
// not a target — would happily send the message to that chat twice. Chat ids
// are compared after trimming, so a caller that deduped on the raw strings
// still gets the collision reported rather than a silent double-send.
//
// Generated delivery ids are not written back onto targets (unlike Create,
// which stamps the broadcast it is given). Call ListDeliveries if you need them
// and match rows by TargetChatID — every row in a batch shares one created_at,
// so the listing's created_at/id ordering does not reproduce the batch order.
//
// Returns sql.ErrNoRows if broadcastID does not exist, and
// ErrBroadcastAlreadyResolved if it exists but is no longer pending.
func (s *SQLiteBroadcastStore) CreateDeliveries(ctx context.Context, broadcastID string, targets []models.BroadcastDelivery) error {
	nowStr := sqlutil.FormatTime(time.Now().UTC())

	// Validate before opening the transaction so a bad batch never takes a write
	// lock, and pre-generate ids so a failed id draw is not a rollback.
	ids := make([]string, len(targets))
	chatIDs := make([]string, len(targets))
	seenChat := make(map[string]int, len(targets))
	for i, target := range targets {
		// Store the trimmed id, not merely validate against it: a padded value
		// would persist padded and then miss every target_chat_id = ? lookup,
		// including the List filter and idx_broadcast_deliveries_chat.
		chatIDs[i] = strings.TrimSpace(target.TargetChatID)
		if chatIDs[i] == "" {
			return fmt.Errorf("create broadcast deliveries: target %d: target chat id is required", i)
		}
		// Reject a chat addressed twice in one batch. The one-shot state=pending
		// predicate only stops a *repeat call* from appending a second audience;
		// it says nothing about the contents of a single slice, and two rows for
		// one chat are two independently claimable deliveries.
		if prev, dup := seenChat[chatIDs[i]]; dup {
			return fmt.Errorf("create broadcast deliveries: target %d duplicates target %d: chat id %q",
				i, prev, chatIDs[i])
		}
		seenChat[chatIDs[i]] = i
		if target.BroadcastID != "" && target.BroadcastID != broadcastID {
			return fmt.Errorf("create broadcast deliveries: target %d: broadcast id %q does not match %q",
				i, target.BroadcastID, broadcastID)
		}
		if target.State != "" && !isSeedableDeliveryState(target.State) {
			return fmt.Errorf("create broadcast deliveries: target %d: %w: %q is not a seedable state",
				i, models.ErrUnknownBroadcastDeliveryState, target.State)
		}
		if target.ID != "" {
			ids[i] = target.ID
			continue
		}
		id, err := sqlutil.NewID()
		if err != nil {
			return fmt.Errorf("new broadcast delivery id: %w", err)
		}
		ids[i] = id
	}

	conn, err := beginImmediate(ctx, s.db, "broadcast")
	if err != nil {
		return err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	res, err := conn.ExecContext(ctx,
		`UPDATE broadcasts
		 SET target_count = ?, state = ?, updated_at = ?
		 WHERE id = ? AND state = ?`,
		len(targets), models.BroadcastStateResolved.String(), nowStr,
		broadcastID, models.BroadcastStatePending.String(),
	)
	if err != nil {
		return fmt.Errorf("stamp broadcast target count: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Separate "no such broadcast" from "already resolved / gone terminal".
		// Read on conn: the transaction still holds this connection.
		var state string
		serr := conn.QueryRowContext(ctx, "SELECT state FROM broadcasts WHERE id = ?", broadcastID).Scan(&state)
		switch {
		case errors.Is(serr, sql.ErrNoRows):
			return sql.ErrNoRows
		case serr != nil:
			return fmt.Errorf("inspect broadcast before resolve: %w", serr)
		default:
			return fmt.Errorf("%w: broadcast %s is %s, not pending", ErrBroadcastAlreadyResolved, broadcastID, state)
		}
	}

	for i, target := range targets {
		state := target.State
		if state == "" {
			state = models.BroadcastDeliveryStatePending
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO broadcast_deliveries
				(id, broadcast_id, target_chat_id, target_daemon_id, state, attempt_count, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
			ids[i], broadcastID, chatIDs[i], strings.TrimSpace(target.TargetDaemonID), state.String(), nowStr, nowStr,
		); err != nil {
			return fmt.Errorf("insert broadcast delivery %d: %w", i, err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit broadcast deliveries: %w", err)
	}
	committed = true
	return nil
}

// Get returns a broadcast by id, or sql.ErrNoRows if absent.
func (s *SQLiteBroadcastStore) Get(ctx context.Context, id string) (*models.Broadcast, error) {
	row := s.db.QueryRowContext(ctx, broadcastSelectSQL+" WHERE id = ?", id)
	return scanBroadcast(row)
}

// List returns broadcasts matching filter, ordered by created_at then id.
func (s *SQLiteBroadcastStore) List(ctx context.Context, filter ListBroadcastsFilter) ([]*models.Broadcast, error) {
	var where []string
	var args []any
	if filter.State != nil {
		where = append(where, "state = ?")
		args = append(args, filter.State.String())
	}
	// Both chat filters are trimmed to match how the write paths normalise, so a
	// caller can round-trip the same string it passed to Create/CreateDeliveries.
	if filter.OriginChatID != nil {
		where = append(where, "origin_chat_id = ?")
		args = append(args, strings.TrimSpace(*filter.OriginChatID))
	}
	if filter.TargetChatID != nil {
		// A target is a delivery row, not a column on broadcasts, so the filter
		// reaches through the child table.
		where = append(where, "id IN (SELECT broadcast_id FROM broadcast_deliveries WHERE target_chat_id = ?)")
		args = append(args, strings.TrimSpace(*filter.TargetChatID))
	}
	query := broadcastSelectSQL
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at ASC, id ASC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	// #nosec G202 -- WHERE is built from code-literal `col = ?` fragments; every
	// value is bound via ?, not concatenated user text; owner=@recurser; review-by=2027-01-26; issue=BOS-554
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list broadcasts: %w", err)
	}
	return collectBroadcasts(rows)
}

// ListDeliveries returns every delivery of a broadcast, ordered by created_at
// then id.
func (s *SQLiteBroadcastStore) ListDeliveries(ctx context.Context, broadcastID string) ([]*models.BroadcastDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		broadcastDeliverySelectSQL+" WHERE broadcast_id = ? ORDER BY created_at ASC, id ASC", broadcastID)
	if err != nil {
		return nil, fmt.Errorf("list broadcast deliveries: %w", err)
	}
	return collectBroadcastDeliveries(rows)
}

// Delete removes a broadcast. Its deliveries go with it via the ON DELETE
// CASCADE foreign key (the connection runs with PRAGMA foreign_keys=ON).
// Idempotent: deleting an absent id is a nil no-op, so repeated cancel is safe.
//
// It does not respect leases. A delivery being worked on right now is cascaded
// away underneath its owner, whose MarkDelivered then reports sql.ErrNoRows
// rather than the ErrBroadcastDeliveryLeaseConflict callers are told to
// tolerate. Callers that need to retire a broadcast while workers may be live
// should prefer a state transition (the canceled state) over a hard delete.
func (s *SQLiteBroadcastStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM broadcasts WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete broadcast: %w", err)
	}
	return nil
}

// ExpireOverdue transitions every live broadcast whose expires_at is at or
// before now to expired, and cascades their still-open (pending/leased)
// deliveries to skipped. Already-delivered rows are history and are left alone.
// It returns the number of broadcasts expired.
//
// Both writes share one transaction so a broadcast can never be observed
// expired while its deliveries are still claimable.
//
// leased rows are swept too, deliberately: leaving them would strand a claim
// whose owner may be dead behind a parent AcquireLease will never re-admit, and
// nothing else would ever retire it. The cost is the at-least-once under-report
// documented on SQLiteBroadcastStore.
func (s *SQLiteBroadcastStore) ExpireOverdue(ctx context.Context, now time.Time) (int, error) {
	nowStr := sqlutil.FormatTime(now)

	conn, err := beginImmediate(ctx, s.db, "broadcast")
	if err != nil {
		return 0, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	res, err := conn.ExecContext(ctx,
		`UPDATE broadcasts
		 SET state = ?, updated_at = ?
		 WHERE state IN (?, ?) AND expires_at <= ?`,
		models.BroadcastStateExpired.String(), nowStr,
		models.BroadcastStatePending.String(), models.BroadcastStateResolved.String(),
		nowStr,
	)
	if err != nil {
		return 0, fmt.Errorf("expire overdue broadcasts: %w", err)
	}
	n, _ := res.RowsAffected()

	// Cascade to deliveries of every expired broadcast — including any swept by
	// an earlier pass — so a delivery orphaned by a partial sweep still settles.
	// skipped is the "target never reached" terminal state.
	if _, err := conn.ExecContext(ctx,
		`UPDATE broadcast_deliveries
		 SET state = ?, lease_owner = NULL, lease_deadline_at = NULL, updated_at = ?
		 WHERE state IN (?, ?)
		   AND broadcast_id IN (SELECT id FROM broadcasts WHERE state = ? AND expires_at <= ?)`,
		models.BroadcastDeliveryStateSkipped.String(), nowStr,
		models.BroadcastDeliveryStatePending.String(), models.BroadcastDeliveryStateLeased.String(),
		models.BroadcastStateExpired.String(), nowStr,
	); err != nil {
		return 0, fmt.Errorf("skip deliveries of expired broadcasts: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit broadcast expiry sweep: %w", err)
	}
	committed = true
	return int(n), nil
}

// AcquireLease claims a delivery for owner until now+leaseFor and returns the
// claimed row.
//
// Claimable means: the delivery is pending or leased (never terminal), its
// retry backoff has elapsed, and the lease is free — unset, already this
// owner's, or past its deadline (crash recovery) — and its parent broadcast is
// still live and not past expiry. That parent-expiry guard matters because
// expiry is only swept lazily: without it a worker could lease and send a
// delivery belonging to an overdue broadcast that ExpireOverdue is about to
// skip. Timestamps are stored in the fixed-width UTC layout, so the string
// comparisons are chronological.
//
// Returns ErrBroadcastDeliveryLeaseConflict when the row exists but is not
// claimable, or sql.ErrNoRows when it is absent.
//
// owner must be non-empty and unique per acquisition; leaseFor must be
// positive. See the "Lease ownership" rule on SQLiteBroadcastStore.
func (s *SQLiteBroadcastStore) AcquireLease(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*models.BroadcastDelivery, error) {
	if err := validateLeaseOwner(owner); err != nil {
		return nil, fmt.Errorf("acquire broadcast delivery lease: %w", err)
	}
	if leaseFor <= 0 {
		// A non-positive lease is born expired: the claim would report success
		// while already being recoverable by anyone, so two workers could hold it
		// at once. Reject rather than silently clamp — a caller passing 0 has a
		// bug, and clamping to some default would hide it.
		return nil, fmt.Errorf("acquire broadcast delivery lease: leaseFor must be positive, got %s", leaseFor)
	}
	nowStr := sqlutil.FormatTime(now)
	deadline := sqlutil.FormatTime(now.Add(leaseFor))
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_deliveries
		 SET state = ?, lease_owner = ?, lease_deadline_at = ?, updated_at = ?
		 WHERE id = ?
		   AND state IN (?, ?)
		   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		   AND (lease_owner IS NULL OR lease_owner = ? OR lease_deadline_at IS NULL OR lease_deadline_at <= ?)
		   AND EXISTS (
		         SELECT 1 FROM broadcasts b
		         WHERE b.id = broadcast_deliveries.broadcast_id
		           AND b.state IN (?, ?)
		           AND b.expires_at > ?
		       )`,
		models.BroadcastDeliveryStateLeased.String(), owner, deadline, nowStr,
		id,
		models.BroadcastDeliveryStatePending.String(), models.BroadcastDeliveryStateLeased.String(),
		nowStr,
		owner, nowStr,
		models.BroadcastStatePending.String(), models.BroadcastStateResolved.String(),
		nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("acquire broadcast delivery lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish an absent row from a live-but-unclaimable one.
		if _, gerr := s.getDelivery(ctx, id); gerr != nil {
			return nil, gerr
		}
		return nil, ErrBroadcastDeliveryLeaseConflict
	}
	claimed, err := s.getDelivery(ctx, id)
	if err != nil {
		return nil, err
	}
	// The claim UPDATE and this read-back are separate autocommit statements, so
	// an ExpireOverdue sweep landing in the gap can null the lease we just won
	// (its cascade clears lease_owner alongside state = skipped). Re-verify
	// rather than hand back a nil error with a lease the caller does not hold:
	// the documented contract is "err == nil means you own it", and a caller
	// dereferencing LeaseOwner would otherwise panic. This is a postcondition
	// check on a window too narrow to force deterministically from a test; it can
	// only ever turn a would-be panic into the benign conflict error.
	if claimed.LeaseOwner == nil || *claimed.LeaseOwner != owner {
		return nil, ErrBroadcastDeliveryLeaseConflict
	}
	return claimed, nil
}

// MarkDelivered transitions a leased delivery held by owner to delivered and
// releases the lease. CAS on state=leased and lease_owner:
// ErrBroadcastDeliveryLeaseConflict if the claim no longer matches (so a worker
// whose lease was recovered reports a lost claim instead of clobbering the
// current owner), or sql.ErrNoRows if absent.
//
// Deliberate divergence from the mirrored callback store: there is no
// `expires_at > now` guard on the parent here. MarkDelivered is called after the
// message has already reached the chat, so refusing it because the parent's
// expiry elapsed mid-flight would not un-send anything — it would only leave the
// row claimable and produce a duplicate send on the next tick. Recording the
// send that happened is the honest outcome; the parent-expiry guard lives on
// AcquireLease, where it can still prevent the send. (github_callbacks makes the
// opposite trade because an expired callback is terminal by contract.) A sweep
// that lands first still wins the row, per the at-least-once rule on
// SQLiteBroadcastStore.
func (s *SQLiteBroadcastStore) MarkDelivered(ctx context.Context, id, owner string, now time.Time) error {
	if err := validateLeaseOwner(owner); err != nil {
		return fmt.Errorf("mark broadcast delivery delivered: %w", err)
	}
	nowStr := sqlutil.FormatTime(now)
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_deliveries
		 SET state = ?, delivered_at = ?, lease_owner = NULL, lease_deadline_at = NULL,
		     last_error = NULL, updated_at = ?
		 WHERE id = ? AND state = ? AND lease_owner = ?`,
		models.BroadcastDeliveryStateDelivered.String(), nowStr, nowStr,
		id, models.BroadcastDeliveryStateLeased.String(), owner,
	)
	if err != nil {
		return fmt.Errorf("mark broadcast delivery delivered: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.getDelivery(ctx, id); gerr != nil {
			return gerr
		}
		return ErrBroadcastDeliveryLeaseConflict
	}
	return nil
}

// ScheduleRetry records a failed attempt on a delivery held by owner: it
// increments attempt_count, sets next_attempt_at and the diagnostic, and
// releases the lease back to pending so a later attempt can re-acquire it once
// the backoff elapses. CAS on lease_owner, with the same conflict contract as
// MarkDelivered.
//
// params.LastError is stored verbatim, and the store cannot check it: it never
// sees the body on this path. Per the secret-body rule on SQLiteBroadcastStore,
// the worker must build the diagnostic from the transport error, never from the
// payload it tried to send.
func (s *SQLiteBroadcastStore) ScheduleRetry(ctx context.Context, id, owner string, params ScheduleBroadcastRetryParams) error {
	if err := validateLeaseOwner(owner); err != nil {
		return fmt.Errorf("schedule broadcast delivery retry: %w", err)
	}
	var lastErrVal any
	if params.LastError != "" {
		lastErrVal = params.LastError
	}
	var nextAttemptVal any
	if !params.NextAttemptAt.IsZero() {
		nextAttemptVal = sqlutil.FormatTime(params.NextAttemptAt)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_deliveries
		 SET attempt_count = attempt_count + 1,
		     next_attempt_at = ?, last_error = ?,
		     state = CASE WHEN state = ? THEN ? ELSE state END,
		     lease_owner = NULL, lease_deadline_at = NULL, updated_at = ?
		 WHERE id = ? AND lease_owner = ?`,
		nextAttemptVal, lastErrVal,
		models.BroadcastDeliveryStateLeased.String(), models.BroadcastDeliveryStatePending.String(),
		sqlutil.TimeNow(),
		id, owner,
	)
	if err != nil {
		return fmt.Errorf("schedule broadcast delivery retry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.getDelivery(ctx, id); gerr != nil {
			return gerr
		}
		return ErrBroadcastDeliveryLeaseConflict
	}
	return nil
}

// CompleteIfSettled flips a resolved broadcast to completed once none of its
// deliveries is still pending or leased. It is a no-op (nil) when work remains
// or the broadcast is already terminal, and returns sql.ErrNoRows if absent, so
// a worker can call it after every delivery without branching.
//
// Only a resolved broadcast is eligible: a pending one has no delivery rows
// yet, so "nothing outstanding" would otherwise complete a broadcast whose
// audience was never materialised.
func (s *SQLiteBroadcastStore) CompleteIfSettled(ctx context.Context, broadcastID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE broadcasts
		 SET state = ?, updated_at = ?
		 WHERE id = ? AND state = ?
		   AND NOT EXISTS (
		         SELECT 1 FROM broadcast_deliveries d
		         WHERE d.broadcast_id = broadcasts.id AND d.state IN (?, ?)
		       )`,
		models.BroadcastStateCompleted.String(), sqlutil.TimeNow(),
		broadcastID, models.BroadcastStateResolved.String(),
		models.BroadcastDeliveryStatePending.String(), models.BroadcastDeliveryStateLeased.String(),
	)
	if err != nil {
		return fmt.Errorf("complete settled broadcast: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Not settled yet, or already terminal — both benign. Absent is not.
		if _, gerr := s.Get(ctx, broadcastID); gerr != nil {
			return gerr
		}
	}
	return nil
}

// DeleteUnmaterialised removes a broadcast ONLY while it is still pending,
// reporting whether it removed one. An absent id, or one that has moved past
// pending, is a nil no-op reporting false.
//
// It exists so a caller that has decided "this row is an abandoned, never-
// materialised send" can act on that decision ATOMICALLY. The obvious spelling —
// read the state, then call Delete — is a check-then-act race, and losing it is
// expensive rather than merely wasteful: between the read and the delete the
// row's owner can commit CreateDeliveries, and Delete is a hard delete that
// cascades to broadcast_deliveries, so the loser would destroy a fully
// materialised broadcast and every delivery row under it. Folding the state test
// into the DELETE's WHERE clause makes that window structurally unreachable.
//
// Pending is the safe state to delete precisely because a pending broadcast has
// no delivery rows: CreateDeliveries inserts the audience and moves the row
// pending -> resolved in ONE transaction, so nothing can be cascaded away here.
func (s *SQLiteBroadcastStore) DeleteUnmaterialised(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM broadcasts WHERE id = ? AND state = ?",
		id, models.BroadcastStatePending.String(),
	)
	if err != nil {
		return false, fmt.Errorf("delete unmaterialised broadcast: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete unmaterialised broadcast: rows affected: %w", err)
	}
	return n > 0, nil
}

// isSeedableDeliveryState reports whether a resolver may create a delivery
// already in this state. Only pending (awaiting a worker) and skipped (the
// target was already gone when the audience was resolved) qualify. leased and
// delivered would be lies — no lease is held and nothing was sent, and the
// insert writes no delivered_at — yet CompleteIfSettled counts both as settled,
// so seeding one would let a broadcast reach completed over a row that never
// went out. failed likewise claims a retry budget the row never spent.
func isSeedableDeliveryState(state models.BroadcastDeliveryState) bool {
	return state == models.BroadcastDeliveryStatePending || state == models.BroadcastDeliveryStateSkipped
}

func (s *SQLiteBroadcastStore) getDelivery(ctx context.Context, id string) (*models.BroadcastDelivery, error) {
	row := s.db.QueryRowContext(ctx, broadcastDeliverySelectSQL+" WHERE id = ?", id)
	return scanBroadcastDelivery(row)
}

// broadcastSelectSQL includes the secret message column deliberately; see the
// secret-body rule on SQLiteBroadcastStore for why, and for what that obliges
// of every surface built on top.
const broadcastSelectSQL = `SELECT id, origin_chat_id, selector, message, state, target_count,
	expires_at, created_at, updated_at
	FROM broadcasts`

const broadcastDeliverySelectSQL = `SELECT id, broadcast_id, target_chat_id, target_daemon_id, state,
	lease_owner, lease_deadline_at, attempt_count, next_attempt_at, delivered_at, last_error,
	created_at, updated_at
	FROM broadcast_deliveries`

func collectBroadcasts(rows *sql.Rows) ([]*models.Broadcast, error) {
	defer func() { _ = rows.Close() }()
	var out []*models.Broadcast
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// scanBroadcast parses one broadcasts row. The state column goes through
// models.ParseBroadcastState rather than a bare conversion so a value outside
// the vocabulary is a loud error at the read boundary instead of a silent
// non-matching state that quietly changes worker behaviour.
func scanBroadcast(sc sqlutil.Scanner) (*models.Broadcast, error) {
	var b models.Broadcast
	var state string
	var originChatID sql.NullString
	var expiresAt, createdAt, updatedAt string
	if err := sc.Scan(
		&b.ID, &originChatID, &b.Selector, &b.Message, &state, &b.TargetCount,
		&expiresAt, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	parsed, err := models.ParseBroadcastState(state)
	if err != nil {
		return nil, fmt.Errorf("broadcast %s: %w", b.ID, err)
	}
	b.State = parsed
	if originChatID.Valid {
		o := originChatID.String
		b.OriginChatID = &o
	}
	b.ExpiresAt = sqlutil.ParseTime(expiresAt)
	b.CreatedAt = sqlutil.ParseTime(createdAt)
	b.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &b, nil
}

func collectBroadcastDeliveries(rows *sql.Rows) ([]*models.BroadcastDelivery, error) {
	defer func() { _ = rows.Close() }()
	var out []*models.BroadcastDelivery
	for rows.Next() {
		d, err := scanBroadcastDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanBroadcastDelivery parses one broadcast_deliveries row, parsing the state
// column for the same reason scanBroadcast does.
func scanBroadcastDelivery(sc sqlutil.Scanner) (*models.BroadcastDelivery, error) {
	var d models.BroadcastDelivery
	var state string
	var leaseOwner, leaseDeadlineAt, nextAttemptAt, deliveredAt, lastError sql.NullString
	var createdAt, updatedAt string
	if err := sc.Scan(
		&d.ID, &d.BroadcastID, &d.TargetChatID, &d.TargetDaemonID, &state,
		&leaseOwner, &leaseDeadlineAt, &d.AttemptCount, &nextAttemptAt, &deliveredAt, &lastError,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	parsed, err := models.ParseBroadcastDeliveryState(state)
	if err != nil {
		return nil, fmt.Errorf("broadcast delivery %s: %w", d.ID, err)
	}
	d.State = parsed
	if leaseOwner.Valid {
		o := leaseOwner.String
		d.LeaseOwner = &o
	}
	if lastError.Valid {
		e := lastError.String
		d.LastError = &e
	}
	d.LeaseDeadlineAt = optionalTime(leaseDeadlineAt)
	d.NextAttemptAt = optionalTime(nextAttemptAt)
	d.DeliveredAt = optionalTime(deliveredAt)
	d.CreatedAt = sqlutil.ParseTime(createdAt)
	d.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &d, nil
}
