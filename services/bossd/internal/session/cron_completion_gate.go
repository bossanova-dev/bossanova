package session

import (
	"context"
	"sync"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

type cronSessionStore interface {
	Get(ctx context.Context, id string) (*models.Session, error)
}

type cronFinalizer interface {
	FinalizeSession(ctx context.Context, sessionID string) (*FinalizeResult, error)
}

// defaultCronFinalizeTimeout bounds a single gated FinalizeSession call. It
// mirrors the hook server's finalizeDispatchTimeout: five minutes comfortably
// covers the worst-case EnsurePR + push + chat-spawn path on a slow repo while
// preventing a finalize that blocks on git/GitHub (network or credentials)
// from pinning the goroutine — and the session in Finalizing — indefinitely.
const defaultCronFinalizeTimeout = 5 * time.Minute

// CronCompletionGateDeps contains dependencies required by CronCompletionGate.
type CronCompletionGateDeps struct {
	Sessions  cronSessionStore
	Finalizer cronFinalizer
	Logger    zerolog.Logger
	// QuietDelay is the debounce window before a finalize check fires.
	// Defaults to 5s when zero.
	QuietDelay time.Duration
	// FinalizeTimeout bounds each FinalizeSession call. Defaults to
	// defaultCronFinalizeTimeout when zero.
	FinalizeTimeout time.Duration
	// RunIsOver reports whether the cron session's agent run has actually
	// finished (durable agent-log idle + liveness — see Lifecycle.cronRunIsOver).
	// The gate is fed by Claude Code's Stop hook, which fires at the end of
	// EVERY main-agent turn — including when the agent merely pauses (awaiting a
	// background subagent, a slow tool). A Stop is therefore a per-turn hint, not
	// a completion signal: without this check the gate would finalize mid-run,
	// opening a junk PR and Blocking a still-working session. When RunIsOver
	// reports the run is not over, the gate defers to the periodic
	// recoverStrandedCronSessions sweep, which re-evaluates the same criterion and
	// finalizes once the run is genuinely complete. A nil RunIsOver (partial
	// wiring) preserves the legacy finalize-on-any-cron-Stop behavior.
	RunIsOver func(*models.Session) bool
}

// CronCompletionGate debounces cron completion signals before finalization and
// gates finalize on the run actually being over. It is the single chokepoint both
// finalize-trigger paths funnel through — the Claude Stop hook (per-turn) and the
// periodic recoverStrandedCronSessions sweep — so the run-completion criterion
// (RunIsOver → Lifecycle.cronRunIsOver) is enforced in exactly one place. A Stop
// alone never finalizes a still-working run; see CronCompletionGateDeps.RunIsOver.
type CronCompletionGate struct {
	sessions        cronSessionStore
	finalizer       cronFinalizer
	logger          zerolog.Logger
	quietDelay      time.Duration
	finalizeTimeout time.Duration
	runIsOver       func(*models.Session) bool

	mu      sync.Mutex
	pending map[string]*cronCompletionGatePending
}

type cronCompletionGatePending struct {
	cancel context.CancelFunc
}

type cronCompletionGateCheckResult int

const (
	cronCompletionGateCheckDone cronCompletionGateCheckResult = iota
	cronCompletionGateCheckRetry
	cronCompletionGateCheckFinalize
)

func newCronCompletionGatePending(cancel context.CancelFunc) *cronCompletionGatePending {
	return &cronCompletionGatePending{cancel: cancel}
}

// NewCronCompletionGate creates a session-keyed cron completion gate.
func NewCronCompletionGate(deps CronCompletionGateDeps) *CronCompletionGate {
	delay := deps.QuietDelay
	if delay <= 0 {
		delay = 5 * time.Second
	}
	finalizeTimeout := deps.FinalizeTimeout
	if finalizeTimeout <= 0 {
		finalizeTimeout = defaultCronFinalizeTimeout
	}
	return &CronCompletionGate{
		sessions:        deps.Sessions,
		finalizer:       deps.Finalizer,
		logger:          deps.Logger,
		quietDelay:      delay,
		finalizeTimeout: finalizeTimeout,
		runIsOver:       deps.RunIsOver,
		pending:         map[string]*cronCompletionGatePending{},
	}
}

// NotifyCronAgentStopped debounces a cron agent stopped signal for a session.
func (g *CronCompletionGate) NotifyCronAgentStopped(sessionID string) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := newCronCompletionGatePending(cancel)

	g.mu.Lock()
	if prior := g.pending[sessionID]; prior != nil {
		prior.cancel()
	}
	g.pending[sessionID] = pending
	g.mu.Unlock()

	go g.afterQuietPeriod(ctx, sessionID, pending)
}

func (g *CronCompletionGate) afterQuietPeriod(ctx context.Context, sessionID string, pending *cronCompletionGatePending) {
	for {
		timer := time.NewTimer(g.quietDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		g.mu.Lock()
		if g.pending[sessionID] != pending {
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()

		result := g.checkAndFinalize(ctx, sessionID)
		if result == cronCompletionGateCheckRetry {
			continue
		}

		g.mu.Lock()
		if g.pending[sessionID] != pending {
			g.mu.Unlock()
			return
		}
		delete(g.pending, sessionID)
		g.mu.Unlock()

		if result == cronCompletionGateCheckFinalize {
			g.finalize(ctx, sessionID)
		}
		return
	}
}

func (g *CronCompletionGate) finalize(ctx context.Context, sessionID string) {
	// Bound the finalize so a git/GitHub call that blocks on network or
	// credentials can't pin this goroutine — and leave the session stuck in
	// Finalizing — forever. Derive from ctx so a superseding signal's cancel
	// still applies, while adding the deadline the hook server's former
	// finalizeDispatchTimeout used to provide.
	ctx, cancel := context.WithTimeout(ctx, g.finalizeTimeout)
	defer cancel()
	if _, err := g.finalizer.FinalizeSession(ctx, sessionID); err != nil {
		g.logger.Error().Err(err).Str("session", sessionID).Msg("cron completion gate finalization failed")
	}
}

func (g *CronCompletionGate) checkAndFinalize(ctx context.Context, sessionID string) cronCompletionGateCheckResult {
	if g.sessions == nil || g.finalizer == nil {
		g.logger.Warn().Str("session", sessionID).Msg("cron completion gate is not fully wired")
		return cronCompletionGateCheckDone
	}

	session, err := g.sessions.Get(ctx, sessionID)
	if err != nil {
		g.logger.Warn().Err(err).Str("session", sessionID).Msg("cron completion gate could not load session")
		return cronCompletionGateCheckDone
	}
	// Finalize sessions that run autonomously — a scheduled cron job OR a
	// tmux_unattended session (e.g. /boss-epic). Interactive/wake/repair sessions
	// also carry a HookToken (so their Stop hook fires here too), but they must
	// never auto-finalize; the persisted IsTmuxUnattended flag, not HookToken
	// presence, is the autonomy signal.
	if !isUnattendedSession(session) {
		return cronCompletionGateCheckDone
	}

	// The Stop hook fires on every turn boundary, including mid-run pauses (e.g.
	// the agent yielding while a background subagent runs). Finalize only when the
	// run is actually over; otherwise defer to the periodic
	// recoverStrandedCronSessions sweep, which re-evaluates the same criterion and
	// finalizes once the run genuinely completes. A nil runIsOver keeps the legacy
	// finalize-on-any-unattended-Stop behavior for partial wiring.
	if g.runIsOver != nil && !g.runIsOver(session) {
		g.logger.Debug().
			Str("session", sessionID).
			Msg("cron completion gate: stop hook fired but run not over; deferring to recovery sweep")
		return cronCompletionGateCheckDone
	}

	return cronCompletionGateCheckFinalize
}
