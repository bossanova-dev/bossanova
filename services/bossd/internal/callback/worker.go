package callback

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// Default delivery-worker tuning.
const (
	// DefaultPollInterval is how often the worker scans for deliverable callbacks.
	DefaultPollInterval = 15 * time.Second
	// DefaultLeaseDuration bounds how long a delivery attempt may hold a claim
	// before another worker may recover it (crash recovery).
	DefaultLeaseDuration = 2 * time.Minute
	// baseRetryBackoff is the first retry delay; it doubles each attempt.
	baseRetryBackoff = 30 * time.Second
	// maxRetryBackoff caps the exponential retry backoff.
	maxRetryBackoff = 15 * time.Minute
	// reconcileEveryTicks triggers a periodic full reconcile as a safety net for
	// enduring states whose webhook was missed (see DeliveryWorker.Run).
	reconcileEveryTicks = 20
)

// ChatDeliverer delivers a rendered callback prompt to a target chat. The
// bossd wiring adapts Server.SendChatMessage (wake + submit) to this interface.
type ChatDeliverer interface {
	Deliver(ctx context.Context, agentSessionID, message string) error
}

// DelivererFunc adapts a function to ChatDeliverer.
type DelivererFunc func(ctx context.Context, agentSessionID, message string) error

// Deliver implements ChatDeliverer.
func (f DelivererFunc) Deliver(ctx context.Context, agentSessionID, message string) error {
	return f(ctx, agentSessionID, message)
}

// reconciler re-evaluates all active callbacks (Evaluator satisfies it).
type reconciler interface {
	ReconcileAll(ctx context.Context) error
}

// workerStore is the subset of db.GithubCallbackStore the worker uses.
type workerStore interface {
	List(ctx context.Context, filter db.ListGithubCallbacksFilter) ([]*models.GithubCallback, error)
	ExpireOverdue(ctx context.Context, now time.Time) (int, error)
	AcquireLease(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*models.GithubCallback, error)
	MarkDelivered(ctx context.Context, id, owner string, now time.Time) error
	ScheduleRetry(ctx context.Context, id, owner string, params db.ScheduleGithubCallbackRetryParams) error
}

// DeliveryWorker leases triggered callbacks and delivers their registered
// message to the target chat with bounded, capped exponential-backoff retry.
//
// Per tick it: (1) lazily expires overdue callbacks; (2) lists triggered
// callbacks and, for each, tries to acquire the lease — the store enforces the
// next_attempt_at backoff and recovers a dead lease, so a claim lost to another
// worker or a not-yet-due retry surfaces as ErrGithubCallbackLeaseConflict and
// is skipped; (3) on a held lease, delivers and MarkDelivered on success, or
// ScheduleRetry on failure. At-least-once: a rare duplicate is possible after a
// crash between Deliver and MarkDelivered; the callback ID in the prompt lets a
// consumer recognize it.
type DeliveryWorker struct {
	store        workerStore
	deliverer    ChatDeliverer
	reconciler   reconciler
	now          func() time.Time
	logger       zerolog.Logger
	owner        string
	pollInterval time.Duration
	leaseFor     time.Duration
}

// WorkerConfig configures a DeliveryWorker. Store and Deliverer are required;
// the rest default.
type WorkerConfig struct {
	Store        workerStore
	Deliverer    ChatDeliverer
	Reconciler   reconciler // optional: periodic reconcile safety net
	Now          func() time.Time
	Logger       zerolog.Logger
	Owner        string // lease owner id; generated when empty
	PollInterval time.Duration
	LeaseFor     time.Duration
}

// NewDeliveryWorker constructs a DeliveryWorker, applying defaults.
func NewDeliveryWorker(cfg WorkerConfig) *DeliveryWorker {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	owner := cfg.Owner
	if owner == "" {
		if id, err := sqlutil.NewID(); err == nil {
			owner = "callback-worker-" + id
		} else {
			owner = "callback-worker"
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
		owner:        owner,
		pollInterval: poll,
		leaseFor:     lease,
	}
}

// Run blocks, scanning on each tick until ctx is cancelled. Start it via
// safego.Go. It runs one scan immediately, then every pollInterval.
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
			// Periodic reconcile safety net: catch enduring states whose webhook
			// was missed while the daemon was disconnected. Cheap when no active
			// callbacks exist.
			if w.reconciler != nil && ticks%reconcileEveryTicks == 0 {
				if err := w.reconciler.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
					w.logger.Warn().Err(err).Msg("callback worker: periodic reconcile failed")
				}
			}
		}
	}
}

// scan runs one delivery pass.
func (w *DeliveryWorker) scan(ctx context.Context) {
	now := w.now()
	if _, err := w.store.ExpireOverdue(ctx, now); err != nil && ctx.Err() == nil {
		w.logger.Warn().Err(err).Msg("callback worker: expire overdue failed")
	}

	triggered := models.GithubCallbackStateTriggered
	cbs, err := w.store.List(ctx, db.ListGithubCallbacksFilter{State: &triggered})
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn().Err(err).Msg("callback worker: list triggered failed")
		}
		return
	}
	for _, cb := range cbs {
		if ctx.Err() != nil {
			return
		}
		w.deliverOne(ctx, cb.ID)
	}
}

// deliverOne attempts to lease and deliver a single callback. All store CAS
// conflicts are tolerated as benign lost races.
func (w *DeliveryWorker) deliverOne(ctx context.Context, id string) {
	now := w.now()
	cb, err := w.store.AcquireLease(ctx, id, w.owner, now, w.leaseFor)
	if err != nil {
		if errors.Is(err, db.ErrGithubCallbackLeaseConflict) || errors.Is(err, sql.ErrNoRows) {
			return // another worker holds it, or backoff not yet elapsed, or gone
		}
		w.logger.Warn().Err(err).Str("callback_id", id).Msg("callback worker: acquire lease failed")
		return
	}

	verified := verifiedStateLabel(cb)
	prompt := BuildCallbackPrompt(cb, verified)

	if derr := w.deliverer.Deliver(ctx, cb.TargetChatID, prompt); derr != nil {
		w.scheduleRetry(ctx, cb, derr)
		return
	}

	if err := w.store.MarkDelivered(ctx, cb.ID, w.owner, w.now()); err != nil {
		// The claim no longer matches (lease recovered, expired, or already
		// delivered by a racer). Benign under at-least-once; log and move on.
		w.logger.Info().Err(err).Str("callback_id", cb.ID).
			Msg("callback worker: mark delivered did not apply (benign race)")
		return
	}
	w.logger.Info().Str("callback_id", cb.ID).Str("chat_id", cb.TargetChatID).
		Msg("callback worker: delivered")
}

// scheduleRetry records a failed delivery with capped exponential backoff. If
// the next attempt would fall at or after expiry, the retry is pinned to
// ExpiresAt so the backoff guard keeps the callback out of contention until the
// lazy expiry sweep reaps it — i.e. it is left to expire rather than retried.
func (w *DeliveryWorker) scheduleRetry(ctx context.Context, cb *models.GithubCallback, cause error) {
	now := w.now()
	next := now.Add(backoffFor(cb.AttemptCount))
	expiring := false
	if !next.Before(cb.ExpiresAt) {
		next = cb.ExpiresAt
		expiring = true
	}
	// last_error is a diagnostic string and must never carry the message body;
	// cause is the deliverer's transport error, which does not contain it.
	if err := w.store.ScheduleRetry(ctx, cb.ID, w.owner, db.ScheduleGithubCallbackRetryParams{
		NextAttemptAt: next,
		LastError:     cause.Error(),
		LastEvent:     string(cb.Trigger),
	}); err != nil {
		w.logger.Warn().Err(err).Str("callback_id", cb.ID).
			Msg("callback worker: schedule retry failed")
		return
	}
	w.logger.Info().Str("callback_id", cb.ID).Int("attempt", cb.AttemptCount+1).
		Time("next_attempt_at", next).Bool("expiring", expiring).
		Msg("callback worker: delivery failed, retry scheduled")
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

// verifiedStateLabel returns the best available human label of the verified
// state for the prompt: the store-recorded last_event when present, else the
// requested trigger name.
func verifiedStateLabel(cb *models.GithubCallback) string {
	if cb.LastEvent != nil && *cb.LastEvent != "" {
		return *cb.LastEvent
	}
	return string(cb.Trigger)
}
