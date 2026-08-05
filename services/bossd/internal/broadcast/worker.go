package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/db"
	daemontelemetry "github.com/recurser/bossd/internal/telemetry"
)

// Default delivery-worker tuning. These mirror callback.DeliveryWorker: the two
// workers do the same job over different rows, and one set of numbers is easier
// to reason about than two.
const (
	// DefaultPollInterval is how often the worker scans for claimable deliveries.
	DefaultPollInterval = 15 * time.Second
	// DefaultLeaseDuration bounds how long a delivery attempt may hold a claim
	// before another worker may recover it (crash recovery).
	DefaultLeaseDuration = 2 * time.Minute
	// baseRetryBackoff is the first retry delay; it doubles each attempt.
	baseRetryBackoff = 30 * time.Second
	// maxRetryBackoff caps the exponential retry backoff.
	maxRetryBackoff = 15 * time.Minute
	// attemptSettleMargin is how much of a lease is reserved for the retry
	// bookkeeping that follows a timed-out attempt (see attemptDeadline).
	attemptSettleMargin = 10 * time.Second
	// reconcileEveryTicks triggers the periodic standing-subscription sweep, the
	// safety net for a session that reached a trigger state with nothing
	// listening (see DeliveryWorker.Run). Same cadence as
	// callback.reconcileEveryTicks, for the same reason the tuning constants
	// above are shared: one set of numbers is easier to reason about than two.
	reconcileEveryTicks = 20
)

// reconciler re-evaluates every live standing subscription
// (broadcast.SubscriptionEvaluator satisfies it).
type reconciler interface {
	ReconcileAll(ctx context.Context) error
}

// ChatDeliverer delivers a rendered broadcast prompt to a target chat. The
// bossd wiring adapts Server.SendChatMessage (wake + submit) to this interface.
//
// It is deliberately a broadcast-owned interface rather than a reuse of
// callback.ChatDeliverer: the two have the same shape, but importing the
// callback package for one method would couple two independent runtimes, and the
// single adapter in main.go satisfies both.
type ChatDeliverer interface {
	Deliver(ctx context.Context, agentSessionID, message string) error
}

// DelivererFunc adapts a function to ChatDeliverer.
type DelivererFunc func(ctx context.Context, agentSessionID, message string) error

// Deliver implements ChatDeliverer.
func (f DelivererFunc) Deliver(ctx context.Context, agentSessionID, message string) error {
	return f(ctx, agentSessionID, message)
}

// workerStore is the subset of db.SQLiteBroadcastStore the worker uses.
//
// There is no claimable-delivery scan on the store, so the worker enumerates
// candidates through the two listings that do exist: List(state=resolved) gives
// every broadcast with a materialised audience (and carries the message body and
// expiry the delivery needs, so no second Get is required), and ListDeliveries
// gives that broadcast's rows. Claimability itself is never decided here — the
// worker offers every pending or leased row to AcquireLease and lets the store's
// CAS predicate (backoff elapsed, lease free or dead, parent live) reject the
// rest. leased rows are offered deliberately: that is how a claim held by a
// worker that died is recovered once its deadline passes.
type workerStore interface {
	List(ctx context.Context, filter db.ListBroadcastsFilter) ([]*models.Broadcast, error)
	ListDeliveries(ctx context.Context, broadcastID string) ([]*models.BroadcastDelivery, error)
	ExpireOverdueDeliveries(ctx context.Context, now time.Time) (int, []db.ExpiredBroadcastDelivery, error)
	AcquireLease(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*models.BroadcastDelivery, error)
	MarkDelivered(ctx context.Context, id, owner string, now time.Time) error
	ScheduleRetry(ctx context.Context, id, owner string, params db.ScheduleBroadcastRetryParams) error
	CompleteIfSettled(ctx context.Context, broadcastID string) error
}

// DeliveryWorker leases the outstanding deliveries of resolved broadcasts and
// sends each target the rendered message, with bounded, capped
// exponential-backoff retry.
//
// Per tick it: (1) lazily expires overdue broadcasts (cascading their
// outstanding deliveries to skipped); (2) lists resolved broadcasts and their
// pending/leased deliveries and, for each, tries to acquire the lease — the
// store enforces the next_attempt_at backoff, the parent's expiry, and dead-lease
// recovery, so a claim lost to another worker or a not-yet-due retry surfaces as
// ErrBroadcastDeliveryLeaseConflict and is skipped; (3) on a held lease, sends
// and MarkDelivered on success, or ScheduleRetry on failure; (4) asks the store
// to complete the parent once nothing is outstanding.
//
// A lost lease always means another worker owns the row: the owner string is the
// store's fencing token, so a conflict is a benign lost race, never an error to
// escalate. Delivery is at-least-once — a crash between the send and
// MarkDelivered can duplicate — and the mitigation is the broadcast id carried in
// the rendered prompt, which lets a receiving agent recognise the repeat.
type DeliveryWorker struct {
	store        workerStore
	deliverer    ChatDeliverer
	reconciler   reconciler
	now          func() time.Time
	logger       zerolog.Logger
	ownerPrefix  string
	pollInterval time.Duration
	leaseFor     time.Duration
	telemetry    libtelemetry.Client
}

// WorkerConfig configures a DeliveryWorker. Store and Deliverer are required;
// the rest default.
type WorkerConfig struct {
	Store     workerStore
	Deliverer ChatDeliverer
	// Reconciler is the standing-subscription sweep run every
	// reconcileEveryTicks ticks. Optional: nil disables it, and only the daemon
	// wires one.
	Reconciler   reconciler
	Now          func() time.Time
	Logger       zerolog.Logger
	Owner        string // lease-owner PREFIX; generated when empty (see leaseOwner)
	PollInterval time.Duration
	LeaseFor     time.Duration
	Telemetry    libtelemetry.Client
}

// NewDeliveryWorker constructs a DeliveryWorker, applying defaults.
func NewDeliveryWorker(cfg WorkerConfig) *DeliveryWorker {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	prefix := cfg.Owner
	if prefix == "" {
		prefix = "broadcast-worker"
		if id, err := sqlutil.NewID(); err == nil {
			prefix += "-" + id
		}
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	lease := cfg.LeaseFor
	if lease <= 0 {
		lease = DefaultLeaseDuration
	}
	return &DeliveryWorker{
		store:        cfg.Store,
		deliverer:    cfg.Deliverer,
		reconciler:   cfg.Reconciler,
		now:          now,
		logger:       cfg.Logger,
		ownerPrefix:  prefix,
		pollInterval: poll,
		leaseFor:     lease,
		telemetry:    cfg.Telemetry,
	}
}

// Run blocks, scanning on each tick until ctx is cancelled. Start it via
// safego.Go. It runs one scan immediately, then every pollInterval.
//
// Every reconcileEveryTicks ticks it also runs the standing-subscription sweep
// (BOS-557), the same way callback.DeliveryWorker runs its callback reconcile.
// The sweep expires overdue subscriptions and fires any whose owning session is
// ALREADY sitting in a trigger state — covering a transition that happened while
// the daemon was down, and the startup bulk orphan advance, which writes state
// through a path the transition hook does not observe. Cheap when no
// subscription is live. It is best-effort: a failure is logged and the tick
// continues, because delivery must not stop because a sweep broke.
func (w *DeliveryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	w.scan(ctx)
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scan(ctx)
			ticks++
			if w.reconciler != nil && ticks%reconcileEveryTicks == 0 {
				if err := w.reconciler.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
					w.logger.Warn().Err(err).Msg("broadcast worker: periodic subscription reconcile failed")
				}
			}
		}
	}
}

// scan runs one delivery pass over every outstanding delivery of every resolved
// broadcast.
func (w *DeliveryWorker) scan(ctx context.Context) {
	now := w.now()
	_, skipped, err := w.store.ExpireOverdueDeliveries(ctx, now)
	if err != nil && ctx.Err() == nil {
		w.logger.Warn().Err(err).Msg("broadcast worker: expire overdue failed")
	} else if err == nil {
		for _, delivery := range skipped {
			daemontelemetry.Capture(ctx, w.telemetry, libtelemetry.EventBroadcastDelivered, map[string]any{
				"status": "skipped", "attempt_count": delivery.AttemptCount,
			})
		}
	}

	resolved := models.BroadcastStateResolved
	broadcasts, err := w.store.List(ctx, db.ListBroadcastsFilter{State: &resolved})
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn().Err(err).Msg("broadcast worker: list resolved failed")
		}
		return
	}
	for _, b := range broadcasts {
		if ctx.Err() != nil {
			return
		}
		deliveries, derr := w.store.ListDeliveries(ctx, b.ID)
		if derr != nil {
			if ctx.Err() == nil {
				w.logger.Warn().Err(derr).Str("broadcast_id", b.ID).
					Msg("broadcast worker: list deliveries failed")
			}
			continue
		}
		outstanding := 0
		for _, d := range deliveries {
			if ctx.Err() != nil {
				return
			}
			if d.State != models.BroadcastDeliveryStatePending && d.State != models.BroadcastDeliveryStateLeased {
				continue // terminal: delivered, failed or skipped
			}
			outstanding++
			// Cheap pre-filter on the backoff we already read. The store's
			// AcquireLease predicate remains the authority; skipping a row whose
			// retry is not yet due only avoids issuing a guarded UPDATE (plus its
			// 0-row diagnostic SELECT) that provably cannot claim it. At the
			// 64-target fan-out cap with every target failing, offering all of
			// them every tick is sustained write-lock contention against a
			// database every other bossd subsystem shares.
			if d.NextAttemptAt != nil && d.NextAttemptAt.After(now) {
				continue
			}
			w.deliverOne(ctx, b, d.ID)
		}
		// Self-heal the completion write. CompleteIfSettled is otherwise only
		// reached from a delivery this worker itself settled, so a broadcast whose
		// last delivery settled in a previous process — a daemon restart between
		// MarkDelivered and CompleteIfSettled — or whose completion UPDATE lost a
		// transient SQLITE_BUSY would sit in resolved until ExpireOverdue retired
		// it as `expired`, reporting "never delivered" for a broadcast every target
		// actually received. Offering a broadcast with nothing outstanding costs
		// one guarded UPDATE and is a nil no-op when it is already terminal.
		if outstanding == 0 {
			w.completeIfSettled(ctx, b.ID)
		}
	}
}

// deliverOne attempts to lease and deliver a single delivery of b. All store CAS
// conflicts are tolerated as benign lost races.
func (w *DeliveryWorker) deliverOne(ctx context.Context, b *models.Broadcast, deliveryID string) {
	owner, err := w.leaseOwner()
	if err != nil {
		w.logger.Warn().Err(err).Str("delivery_id", deliveryID).
			Msg("broadcast worker: could not mint a lease owner")
		return
	}

	now := w.now()
	d, err := w.store.AcquireLease(ctx, deliveryID, owner, now, w.leaseFor)
	if err != nil {
		if isBenignClaimLoss(err) {
			return // another worker holds it, backoff not yet elapsed, or gone
		}
		if ctx.Err() == nil {
			w.logger.Warn().Err(err).Str("delivery_id", deliveryID).
				Msg("broadcast worker: acquire lease failed")
		}
		return
	}

	// The prompt is the only place the secret body appears; it is never logged.
	prompt := BuildBroadcastPrompt(b, d)

	// Bound EACH ATTEMPT by (a fraction under) the lease it holds. Deliver wakes
	// an asleep chat — spawning tmux and waiting on the agent's readiness marker
	// — and scan runs deliveries serially on Run's single goroutine under the
	// daemon-lifetime context, so without a deadline one wedged chat stalls every
	// other delivery and the per-tick expiry sweep forever. leaseFor is the
	// system's answer to "how long may one attempt hold this row": past it
	// another worker may recover the claim, so blocking longer buys nothing. This
	// bounds one attempt, not one scan: a whole scan is still serial, which is
	// the callback precedent's shape and acceptable while bossd runs one worker.
	if derr := w.deliverWithin(ctx, d.TargetChatID, prompt); derr != nil {
		w.scheduleRetry(ctx, b, d, owner, derr)
		return
	}

	if err := w.store.MarkDelivered(ctx, d.ID, owner, w.now()); err != nil {
		// Two very different failures reach here and must not be conflated. A lost
		// CAS (lease recovered, row swept) is the benign at-least-once race: the
		// message is already sent, so retrying would double-send. Anything else is
		// a real store fault that ALSO leaves the row leased — it will be
		// re-delivered once the lease dies — and must be visible at Warn rather
		// than mislabelled "benign". Same shape deliverOne applies to AcquireLease.
		if isBenignClaimLoss(err) {
			w.logger.Info().Err(err).Str("delivery_id", d.ID).Str("broadcast_id", b.ID).
				Msg("broadcast worker: mark delivered did not apply (benign race)")
			return
		}
		if ctx.Err() == nil {
			w.logger.Warn().Err(err).Str("delivery_id", d.ID).Str("broadcast_id", b.ID).
				Msg("broadcast worker: mark delivered failed")
		}
		return
	}
	w.logger.Info().Str("delivery_id", d.ID).Str("broadcast_id", b.ID).
		Str("chat_id", d.TargetChatID).Msg("broadcast worker: delivered")
	daemontelemetry.Capture(ctx, w.telemetry, libtelemetry.EventBroadcastDelivered, map[string]any{
		"status": "delivered", "attempt_count": d.AttemptCount + 1,
	})
	w.completeIfSettled(ctx, b.ID)
}

// scheduleRetry records a failed delivery with capped exponential backoff. If
// the next attempt would fall at or after the parent broadcast's expiry, the
// retry is pinned to ExpiresAt so the backoff guard keeps the delivery out of
// contention until the lazy expiry sweep reaps it — i.e. it is left to expire
// rather than retried. This is the rule callback/worker.go:214-221 applies to a
// callback, transcribed to a delivery.
//
// A pinned retry needs no CompleteIfSettled call here: ScheduleRetry returns the
// row to pending, so the store's "nothing pending or leased" predicate cannot
// match, and once the sweep does move the row it also flips the parent to
// expired, which the state = resolved guard excludes. scan's per-broadcast
// settle check is what actually completes a broadcast whose last delivery
// settled outside this call.
func (w *DeliveryWorker) scheduleRetry(ctx context.Context, b *models.Broadcast, d *models.BroadcastDelivery, owner string, cause error) {
	now := w.now()
	next := now.Add(backoffFor(d.AttemptCount))
	expiring := false
	if !next.Before(b.ExpiresAt) {
		next = b.ExpiresAt
		expiring = true
	}
	if err := w.store.ScheduleRetry(ctx, d.ID, owner, db.ScheduleBroadcastRetryParams{
		NextAttemptAt: next,
		LastError:     deliveryDiagnostic(cause, b.Message),
	}); err != nil {
		// A lost claim is routine here, not a fault: when the parent expires
		// mid-attempt the sweep cascades this row to skipped and NULLs its
		// lease_owner, so the retry CAS necessarily misses. Warn is reserved for a
		// store fault a human should look at.
		switch {
		case isBenignClaimLoss(err):
			w.logger.Debug().Err(err).Str("delivery_id", d.ID).
				Msg("broadcast worker: retry not scheduled, claim already lost")
		case ctx.Err() == nil:
			w.logger.Warn().Err(err).Str("delivery_id", d.ID).
				Msg("broadcast worker: schedule retry failed")
		}
		return
	}
	w.logger.Info().Str("delivery_id", d.ID).Str("broadcast_id", b.ID).
		Int("attempt", d.AttemptCount+1).Time("next_attempt_at", next).Bool("expiring", expiring).
		Msg("broadcast worker: delivery failed, retry scheduled")
}

// deliverWithin runs one delivery attempt under a deadline strictly inside the
// lease it holds, so a wedged target cannot block the worker's single scan
// goroutine forever.
//
// The deadline is deliberately SHORTER than leaseFor. Timing out at exactly the
// lease deadline would fire ScheduleRetry at the one instant another owner may
// legally recover the row, so the CAS would miss and the attempt would vanish
// with no attempt_count, no last_error and no backoff. Leaving a margin means
// the retry bookkeeping still lands under the claim that produced it.
func (w *DeliveryWorker) deliverWithin(ctx context.Context, chatID, prompt string) error {
	dctx, cancel := context.WithTimeout(ctx, attemptDeadline(w.leaseFor))
	defer cancel()
	return w.deliverer.Deliver(dctx, chatID, prompt)
}

// attemptDeadline returns how long one delivery attempt may run under a lease of
// leaseFor, reserving a margin for the retry write that follows a timeout.
func attemptDeadline(leaseFor time.Duration) time.Duration {
	d := leaseFor - attemptSettleMargin
	if d <= 0 {
		// A lease shorter than the margin (only reachable from tests) still needs
		// a positive deadline; take most of it rather than clamping to zero.
		d = leaseFor / 2
	}
	if d <= 0 {
		// A sub-2ns lease leaves nothing to halve. Stay strictly inside it rather
		// than returning leaseFor, which is the one value the margin exists to
		// avoid.
		d = 1
	}
	return d
}

// redactedBodyMarker replaces the broadcast body wherever a transport error
// echoed it back. redactionSentinel is the intermediate stand-in used while
// redacting; it is a control character precisely so no needle drawn from a body
// can collide with it the way a needle can collide with the marker's words.
const (
	redactedBodyMarker = "[broadcast message redacted]"
	redactionSentinel  = "\x00"
)

// maxDiagnosticLen bounds what a single failure may write into last_error.
const maxDiagnosticLen = 512

// deliveryDiagnostic renders a failed attempt's transport error into the
// body-free, length-bounded string stored in broadcast_deliveries.last_error.
//
// The redaction is REQUIRED, not defensive tidying. The delivery path is
// Server.SendChatMessage -> tmux.Client.SendMessage, whose submit verifier
// reports a payload that never left the composer by formatting the WHOLE
// payload back into its error ("command was not submitted; %q is still present
// at the tmux prompt", tmux/tmux_submit_verify.go). That payload is the rendered
// prompt, which carries the broadcast body verbatim — so storing cause.Error()
// as-is would write the secret body straight into last_error, the one column the
// store's secret-body rule singles out ("Nothing may copy the body into a
// delivery column, least of all the last_error diagnostic"), and from there onto
// every read surface that returns a delivery. The worker is the only layer that
// holds both the body and the diagnostic, so the scrub belongs here.
//
// Both renderings are stripped: the raw body, and the Go-quoted form a %q
// verb produces (where a multi-line body's newlines arrive as literal \n
// escapes, which a raw-substring replace alone would miss).
func deliveryDiagnostic(cause error, body string) string {
	text := ""
	if cause != nil {
		text = cause.Error()
	}

	// Longest needle first, so a shorter overlapping form cannot leave a
	// fragment of a longer one behind. Each match is replaced with a sentinel
	// rather than the marker itself, and the marker substituted once at the end:
	// replacing straight into human-readable text lets a later, shorter needle
	// match INSIDE the marker an earlier one inserted (a body of "message" would
	// rewrite the marker's own wording), garbling the diagnostic.
	needles := bodyRedactionNeedles(body)
	sort.Slice(needles, func(i, j int) bool { return len(needles[i]) > len(needles[j]) })
	for _, needle := range needles {
		text = strings.ReplaceAll(text, needle, redactionSentinel)
	}
	text = strings.ReplaceAll(text, redactionSentinel, redactedBodyMarker)

	text = strings.TrimSpace(text)
	if text == "" {
		return "delivery failed"
	}
	if len(text) > maxDiagnosticLen {
		// Cut on a rune boundary; the marker survives because redaction ran first.
		text = strings.ToValidUTF8(text[:maxDiagnosticLen], "") + "…"
	}
	return text
}

// bodyRedactionNeedles returns every rendering of the broadcast body a transport
// error might echo. Very short bodies are still redacted: a false positive
// mangles a diagnostic, while a miss leaks the secret.
func bodyRedactionNeedles(body string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, form := range []string{body, strings.TrimSpace(body)} {
		add(form)
		// strconv.Quote wraps in quotes and escapes the interior; the interior is
		// what appears inside a larger %q-formatted string.
		if quoted := strconv.Quote(form); len(quoted) >= 2 {
			add(quoted[1 : len(quoted)-1])
		}
	}
	return out
}

// completeIfSettled asks the store to complete the parent broadcast once none of
// its deliveries is still outstanding. A broadcast with work left, or one that is
// already terminal, is a nil no-op, so this is called after every settled
// delivery without branching.
func (w *DeliveryWorker) completeIfSettled(ctx context.Context, broadcastID string) {
	err := w.store.CompleteIfSettled(ctx, broadcastID)
	switch {
	case err == nil:
	case isBenignClaimLoss(err):
		// A DeleteBroadcast landing mid-flight takes the row away; nothing to
		// complete and nothing to fix.
		w.logger.Debug().Err(err).Str("broadcast_id", broadcastID).
			Msg("broadcast worker: broadcast gone before it could be completed")
	case ctx.Err() == nil:
		w.logger.Warn().Err(err).Str("broadcast_id", broadcastID).
			Msg("broadcast worker: complete if settled failed")
	}
}

// isBenignClaimLoss reports whether err is the store telling this worker it no
// longer owns the row (or the row is gone) rather than reporting a fault. Both
// are expected outcomes of the CAS design and must not be logged as failures.
func isBenignClaimLoss(err error) bool {
	return errors.Is(err, db.ErrBroadcastDeliveryLeaseConflict) || errors.Is(err, sql.ErrNoRows)
}

// leaseOwner mints a fresh fencing token for one acquisition.
//
// The store's lease contract requires the owner string to be non-empty and
// UNIQUE PER ACQUISITION rather than a stable per-worker id: MarkDelivered and
// ScheduleRetry carry no fence beyond lease_owner, so a stable id would let a
// hung attempt whose lease expired come back and settle the newer claim that
// replaced it. The configured Owner is therefore only a prefix, kept for
// readable logs and diagnostics.
func (w *DeliveryWorker) leaseOwner() (string, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return "", err
	}
	return w.ownerPrefix + ":" + id, nil
}

// backoffFor returns the retry delay for the given prior attempt count, doubling
// from baseRetryBackoff and capped at maxRetryBackoff.
func backoffFor(attemptCount int) time.Duration {
	d := baseRetryBackoff
	for range attemptCount {
		d *= 2
		if d >= maxRetryBackoff {
			return maxRetryBackoff
		}
	}
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}
