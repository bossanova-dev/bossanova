package rotation

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
)

// rotateTimeout bounds one stop→rebind→respawn orchestration.
const rotateTimeout = 2 * time.Minute

// ChatContext is the minimal session context a chat rotation needs.
type ChatContext struct {
	SessionID string
	RepoID    string
	Provider  string // Session.AgentName ("claude", "codex", ...)
}

// DecideRequest is the rotator-side view of a rotation decision request. The
// main.go adapter converts it into the BOS-173 engine's real rotation.Signal.
type DecideRequest struct {
	Provider       string
	SessionID      string
	AgentSessionID string
	ResetAt        time.Time // zero when the banner had no parseable reset
}

// DecisionKind classifies the rotator-side decision.
type DecisionKind int

const (
	// DecisionSwitch means AccountID is the account to rotate to.
	DecisionSwitch DecisionKind = iota + 1
	// DecisionAllExhausted means no account is available now (at least one is
	// cooling): the chat stays limited. ResumeAt is the earliest recovery time.
	DecisionAllExhausted
	// DecisionStatusOnly means the provider cannot rotate: do nothing.
	DecisionStatusOnly
)

// Decision is the rotator-side view of the engine's Outcome, produced by the
// main.go adapter from the real rotation.Outcome.
type Decision struct {
	Kind      DecisionKind
	AccountID string    // set for DecisionSwitch
	Label     string    // human label of the chosen account, for logs
	ResumeAt  time.Time // set for DecisionAllExhausted (min future cooldown)
}

// SwitchRequest is the rotator-side view of the BOS-171 SwitchAccount primitive
// (stop pane → rebind → respawn with resume → in-chat notice). Auto marks the
// automatic path: there is no operator to confirm a mid-turn interrupt, so a chat
// that recovered to WORKING before the switch executes aborts fail-safe instead of
// being interrupted; the notice uses session.AutoRotateNotice wording with
// PreviousResetAt.
type SwitchRequest struct {
	SessionID       string
	AgentSessionID  string
	AccountID       string
	Auto            bool
	PreviousResetAt time.Time
}

// SwitchResult reports the switch outcome.
type SwitchResult struct {
	SwitchedToLabel string
	Fresh           bool // restarted fresh (stale/unsupported resume)
}

// ChatRotatorDeps carries the injected seams. All are required.
type ChatRotatorDeps struct {
	Logger        zerolog.Logger
	LoadConfig    func() (config.RotationConfig, error)
	ChatContext   func(ctx context.Context, agentSessionID string) (ChatContext, error)
	CurrentStatus func(agentSessionID string) bossanovav1.ChatStatus
	Decide        func(ctx context.Context, req DecideRequest) (Decision, error)
	Switch        func(ctx context.Context, req SwitchRequest) (SwitchResult, error)
	Now           func() time.Time // nil = time.Now
	// Recorder audits each rotation decision outcome (BOS-176). Nil is safe: the
	// Recorder's methods are nil-receiver no-ops.
	Recorder *Recorder
}

// ChatRotator auto-rotates interactive chats on CHAT_STATUS_LIMITED transitions
// (Epic 4.3, D4). Trigger + policy glue only: pane lifecycle stays owned by the
// BOS-171 switch primitive / chat tracker (BOS-153). Fail-safe: any error ⇒ no
// rotation, the chat stays LIMITED.
type ChatRotator struct {
	deps ChatRotatorDeps

	mu          sync.Mutex
	inFlight    map[string]bool      // agentSessionID → rotation running
	lastAttempt map[string]time.Time // agentSessionID → last attempt (rate limit)
	active      sync.WaitGroup       // observed by idleForTest
}

// NewChatRotator builds a ChatRotator from its dependency seams.
func NewChatRotator(deps ChatRotatorDeps) *ChatRotator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &ChatRotator{
		deps:        deps,
		inFlight:    map[string]bool{},
		lastAttempt: map[string]time.Time{},
	}
}

// OnChatStatus is chained into the chat-status tracker's on-update hook
// (main.go), which only fires on real transitions. It must return fast: policy
// checks that need I/O run in a dispatched goroutine.
func (r *ChatRotator) OnChatStatus(agentSessionID string, st bossanovav1.ChatStatus, resetAt time.Time) {
	if st != bossanovav1.ChatStatus_CHAT_STATUS_LIMITED {
		return // WORKING/IDLE/QUESTION/STOPPED chats are NEVER rotated.
	}
	// Default to the canonical rate-limit window (single-sourced in config) when
	// the config load fails, so the fallback can never drift from the real default.
	minInterval := config.RotationConfig{}.ChatRotateMinInterval()
	if cfg, err := r.deps.LoadConfig(); err == nil {
		minInterval = cfg.ChatRotateMinInterval()
	}

	now := r.deps.Now()
	r.mu.Lock()
	// Evict stale rate-limit entries so lastAttempt does not grow unbounded over a
	// long-lived daemon: an entry older than the rate-limit window no longer
	// suppresses anything, so dropping it is behaviour-neutral. This bounds the map
	// to chats that hit LIMITED within the last minInterval. Evicting an entry whose
	// rotation is somehow still in flight (only reachable if minInterval is
	// configured below the 2m rotate timeout) is harmless: the separate inFlight
	// guard below, not lastAttempt, is what prevents a concurrent second rotation.
	for id, last := range r.lastAttempt {
		if now.Sub(last) >= minInterval {
			delete(r.lastAttempt, id)
		}
	}
	if r.inFlight[agentSessionID] {
		r.mu.Unlock()
		return
	}
	if last, ok := r.lastAttempt[agentSessionID]; ok && now.Sub(last) < minInterval {
		r.mu.Unlock()
		r.deps.Logger.Debug().Str("agent_session_id", agentSessionID).
			Msg("auto-rotate suppressed by per-chat rate limit")
		return
	}
	// Charge the limiter at attempt time (belt-and-braces vs banner flap), not at
	// success time.
	r.inFlight[agentSessionID] = true
	r.lastAttempt[agentSessionID] = now
	r.mu.Unlock()

	r.active.Add(1)
	safego.Go(r.deps.Logger, func() {
		defer r.active.Done()
		defer func() {
			r.mu.Lock()
			delete(r.inFlight, agentSessionID)
			r.mu.Unlock()
		}()
		r.rotate(agentSessionID, resetAt)
	})
}

func (r *ChatRotator) rotate(agentSessionID string, resetAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()

	cfg, err := r.deps.LoadConfig()
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate: config load failed; leaving chat limited")
		r.forgetAttempt(agentSessionID)
		return
	}
	cc, err := r.deps.ChatContext(ctx, agentSessionID)
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate: chat context lookup failed")
		r.forgetAttempt(agentSessionID)
		return
	}
	if !cfg.RotationEnabled() || !cfg.AutoRotateChatsEnabled(cc.RepoID) {
		log.Debug().Str("repo_id", cc.RepoID).Msg("auto-rotate: opted out; leaving chat limited")
		var disabledReset *time.Time
		if !resetAt.IsZero() {
			disabledReset = &resetAt
		}
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger: "ROTATION_TRIGGER_USAGE_LIMITED", ResetAt: disabledReset,
			Outcome: "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			Detail:  "automatic rotation disabled",
		})
		return
	}
	// Re-check: only act if the chat is STILL limited right now (the pane may have
	// redrawn/recovered between the transition and this dispatch).
	if st := r.deps.CurrentStatus(agentSessionID); st != bossanovav1.ChatStatus_CHAT_STATUS_LIMITED {
		log.Debug().Stringer("status", st).Msg("auto-rotate: chat no longer limited; aborting")
		return
	}

	decision, err := r.deps.Decide(ctx, DecideRequest{
		Provider:       cc.Provider,
		SessionID:      cc.SessionID,
		AgentSessionID: agentSessionID,
		ResetAt:        resetAt,
	})
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate: engine decide failed; leaving chat limited")
		return
	}

	var resetPtr *time.Time
	if !resetAt.IsZero() {
		resetPtr = &resetAt
	}
	auditBase := AuditEvent{
		SessionID: cc.SessionID,
		ChatID:    agentSessionID,
		Provider:  cc.Provider,
		Trigger:   "ROTATION_TRIGGER_USAGE_LIMITED",
		ResetAt:   resetPtr,
	}

	switch decision.Kind {
	case DecisionSwitch:
		res, err := r.deps.Switch(ctx, SwitchRequest{
			SessionID:       cc.SessionID,
			AgentSessionID:  agentSessionID,
			AccountID:       decision.AccountID,
			Auto:            true,
			PreviousResetAt: resetAt,
		})
		if err != nil {
			// A failed switch is fail-safe: leave the chat as-is. This also covers
			// the benign race where the chat recovered to WORKING before the switch
			// executed (SwitchAccount aborts with ErrChatMidTurn rather than kill a
			// live turn), so the message avoids asserting the chat is still limited.
			log.Warn().Err(err).Str("account_id", decision.AccountID).
				Msg("auto-rotate: switch did not complete; leaving chat as-is")
			failed := auditBase
			failed.ToAccount = decision.Label
			failed.Outcome = "ROTATION_OUTCOME_FAILED"
			failed.Detail = "switch did not complete"
			r.deps.Recorder.Record(ctx, failed)
			return
		}
		log.Info().Str("account", res.SwitchedToLabel).Bool("fresh", res.Fresh).
			Msg("auto-rotate: chat rotated to next account")
		rotated := auditBase
		rotated.ToAccount = res.SwitchedToLabel
		rotated.Outcome = "ROTATION_OUTCOME_ROTATED"
		if resetPtr != nil {
			rotated.Detail = "resets " + resetPtr.Format("15:04")
		}
		r.deps.Recorder.Record(ctx, rotated)
	case DecisionAllExhausted:
		// Terminal park: the chat keeps CHAT_STATUS_LIMITED; parked-timer UX +
		// single notification are BOS-176. One log line, no loop.
		log.Info().Time("resume_at", decision.ResumeAt).
			Msg("auto-rotate: all accounts cooling; chat stays limited")
		exhausted := auditBase
		exhausted.ResetAt = &decision.ResumeAt
		exhausted.Outcome = "ROTATION_OUTCOME_EXHAUSTED"
		exhausted.Detail = "all accounts cooling until " + decision.ResumeAt.Format("15:04")
		r.deps.Recorder.RecordExhausted(ctx, exhausted)
	case DecisionStatusOnly:
		log.Debug().Msg("auto-rotate: agent has no rotation capability; status only")
		statusOnly := auditBase
		statusOnly.Outcome = "ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY"
		statusOnly.Detail = "agent cannot rotate"
		r.deps.Recorder.Record(ctx, statusOnly)
	default:
		log.Warn().Int("kind", int(decision.Kind)).
			Msg("auto-rotate: unknown decision kind; leaving chat limited")
	}
}

func (r *ChatRotator) forgetAttempt(agentSessionID string) {
	r.mu.Lock()
	delete(r.lastAttempt, agentSessionID)
	r.mu.Unlock()
}

// idleForTest reports whether no rotation goroutine is active. Test-only.
func (r *ChatRotator) idleForTest() bool {
	done := make(chan struct{})
	go func() { r.active.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(time.Millisecond):
		return false
	}
}
