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
}

// CronCompletionGate debounces cron completion signals before finalization.
type CronCompletionGate struct {
	sessions        cronSessionStore
	finalizer       cronFinalizer
	logger          zerolog.Logger
	quietDelay      time.Duration
	finalizeTimeout time.Duration

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
	if session.CronJobID == nil || *session.CronJobID == "" {
		return cronCompletionGateCheckDone
	}

	return cronCompletionGateCheckFinalize
}
