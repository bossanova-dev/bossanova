package main

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossd/internal/domain"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

func (w *daemonWiring) wireRotation() {
	// Auto-rotate interactive chats (BOS-175): on a CHAT_STATUS_LIMITED transition
	// the rotator asks the BOS-173 engine for the next account and executes the
	// swap through the BOS-171 SwitchAgentAccount primitive (Auto path). All seams are
	// fail-safe — any error leaves the chat LIMITED. Config is re-read live so the
	// opt-out applies without a daemon restart.
	rateLimitProbe := func(ctx context.Context, accountID string) (domain.UsageSnapshot, error) {
		snap, ok := probeUsageSnapshotForRotation(ctx, log.Logger, w.accountMaterializer, w.accounts, accountID, nil)
		if !ok {
			return domain.UsageSnapshot{}, nil
		}
		return snap, nil
	}
	w.chatRotator = rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:   log.Logger,
		Recorder: w.rotationRecorder,
		OnAuthDecisionComplete: func(agentSessionID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			publishAgentMarkerSessionDelta(ctx, agentSessionID, w.streamHydrator, w.streamBus, log.Logger)
		},
		LoadConfig: func() (config.ManagedAgentAccountsConfig, error) {
			loaded, err := config.Load()
			if err != nil {
				return config.ManagedAgentAccountsConfig{}, err
			}
			return loaded.ManagedAccounts, nil
		},
		ChatContext: func(ctx context.Context, agentSessionID string) (rotation.ChatContext, error) {
			chat, err := w.agentChats.GetByAgentSessionID(ctx, agentSessionID)
			if err != nil {
				return rotation.ChatContext{}, err
			}
			if chat == nil {
				return rotation.ChatContext{}, fmt.Errorf("agent chat not found for agent_session_id %q", agentSessionID)
			}
			sess, err := w.sessions.Get(ctx, chat.SessionID)
			if err != nil {
				return rotation.ChatContext{}, err
			}
			// BOS-381: provider + account authority lives on the CHAT, not the
			// session. Read them from the freshly-fetched chat; a chat that never
			// bound its own account (nil) inherits the session's mirrored binding.
			accountID := ""
			if chat.AccountID != nil {
				accountID = *chat.AccountID
			} else if sess.AccountID != nil {
				accountID = *sess.AccountID
			}
			provider := chat.AgentName
			if provider == "" {
				provider = sess.AgentName
			}
			// Resolve the bound account to its human label here (never empty:
			// "" -> "Unmanaged local credentials", unknown -> short id) so every
			// rotation audit records a readable from-side. Keeps the rotation
			// package free of the account/db packages — the label is injected,
			// mirroring how the to-side decision.Label is injected.
			fromLabel, _ := w.accountResolver.Label(ctx, accountID)
			return rotation.ChatContext{SessionID: sess.ID, RepoID: sess.RepoID, Provider: provider, Model: chat.Model, AccountID: accountID, FromLabel: fromLabel}, nil
		},
		CurrentStatus: func(agentSessionID string) bossanovav1.ChatStatus {
			if e := w.chatStatusTracker.Get(agentSessionID); e != nil {
				return e.Status
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED
		},
		RateLimitProbe: rateLimitProbe,
		// Auth-invalidation path (BOS-316): CurrentAuthFailed re-checks the pane is
		// still login-required at dispatch time; AuthProbe confirms a typed 401 on the
		// bound account before any rotation. AuthProbe reuses the same
		// ProbeUsageSnapshot seam as the LIMITED path but classifies its error rather
		// than its snapshot: only a codes.Unauthenticated failure confirms the 401. A
		// healthy/limited snapshot, an unsupported probe, or any non-auth error yields
		// confirmed=false (non-auth errors are logged, never surfaced as rotation), so
		// the path proceeds only on a real auth invalidation.
		CurrentAuthFailed: w.chatStatusTracker.AuthFailed,
		// The stable per-episode anchor behind the BOS-980 latch: it lets the rotator tell
		// a pane still inside the same auth-failed episode from one that recovered and
		// wedged again. (AuthFailed alone cannot, because the tracker clears its marker on
		// the first clean poll by design — see the BOS-808 note on status/tracker.go.)
		AuthFailedSince: w.chatStatusTracker.AuthFailedSince,
		AuthProbe: func(ctx context.Context, accountID string) rotation.AuthProbeResult {
			prober, ok := w.accountMaterializer.(usageSnapshotProber)
			if !ok || accountID == "" {
				// No probe capability: inconclusive, never rotate on the loose trigger.
				return rotation.AuthProbeUnknown
			}
			_, err := prober.ProbeUsageSnapshot(ctx, accountID)
			if err == nil {
				// The bound account itself probes healthy — the pane is auth-failed for a
				// plumbing reason (a stale in-proxy token after a daemon restart), not an
				// invalid credential. Signals the respawn-in-place path (BOS-482).
				return rotation.AuthProbeHealthy
			}
			if authProbeConfirmsInvalidation(err) {
				return rotation.AuthProbeConfirmed401
			}
			log.Warn().Err(err).Str("account_id", accountID).
				Msg("auto-rotate(auth): probe returned non-auth error; inconclusive")
			return rotation.AuthProbeUnknown
		},
		// Decide adapter: build the BOS-173 engine's real Signal (the currently
		// bound account is the "capped" account; capability is probed live via the
		// provider plugin) and map its Outcome onto rotation.Decision.
		Decide: func(ctx context.Context, req rotation.DecideRequest) (rotation.Decision, error) {
			capable, err := w.accountMaterializer.SupportsRotation(ctx, req.Provider)
			if err != nil {
				return rotation.Decision{}, err
			}
			sig := cacheUsageProbeForRotationSignal(ctx, log.Logger, w.accountMaterializer, w.accounts, rotation.Signal{
				Provider:        req.Provider,
				Model:           req.Model,
				CappedAccountID: req.AccountID,
				Kind:            req.Kind,
				ResetAt: func() *time.Time {
					if req.ResetAt.IsZero() {
						return nil
					}
					r := req.ResetAt
					return &r
				}(),
				RotationCapable:    capable,
				SuppressHealthFail: req.SuppressHealthFail,
			})
			out, err := w.rotationEngine.Decide(ctx, sig)
			if err != nil {
				return rotation.Decision{}, err
			}
			switch out.Kind {
			case rotation.OutcomeRotate:
				if out.NextAccount == nil {
					// Defensive: OutcomeRotate should always carry a target, but a nil
					// NextAccount IS "no eligible account" — route it to the no-eligible
					// decision (not the capability short-circuit's status-only) so the
					// operator is steered to enable an account (BOS-327).
					return rotation.Decision{Kind: rotation.DecisionNoEligibleAgentAccount}, nil
				}
				return rotation.Decision{
					Kind:      rotation.DecisionSwitch,
					AccountID: out.NextAccount.ID,
					Label:     out.NextAccount.Label,
				}, nil
			case rotation.OutcomeAllExhausted:
				return rotation.Decision{Kind: rotation.DecisionAllExhausted, ResumeAt: out.ResumeAt}, nil
			case rotation.OutcomeNoEligibleAgentAccount:
				return rotation.Decision{Kind: rotation.DecisionNoEligibleAgentAccount}, nil
			default:
				// OutcomeStatusOnly (capability short-circuit) and any unknown kind
				// map to status-only: the agent/provider cannot rotate.
				return rotation.Decision{Kind: rotation.DecisionStatusOnly}, nil
			}
		},
		// Switch adapter: the BOS-171 manual-switch primitive on its Auto path.
		Switch: func(ctx context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			res, err := w.lifecycle.SwitchAgentAccount(ctx, session.SwitchAgentAccountParams{
				SessionID:          req.SessionID,
				AgentSessionID:     req.AgentSessionID,
				TargetAccountID:    req.AccountID,
				Auto:               true,
				RespawnSameAccount: req.RespawnSameAccount,
				PreviousResetAt:    req.PreviousResetAt,
			})
			if err != nil {
				return rotation.SwitchResult{}, mapRotationSwitchError(err)
			}
			return rotation.SwitchResult{SwitchedToLabel: res.TargetLabel, Fresh: !res.Resumed}, nil
		},
		// Proactive pre-cap sweep seams (BOS-318). LiveChatStatuses surfaces the
		// non-stale live panes; ProactiveCandidate probes candidate utilization and
		// picks the banded consume-first candidate — soonest in-band weekly reset,
		// else the idlest (BOS-830) — WITHOUT cooling the bound account.
		LiveChatStatuses: func() map[string]bossanovav1.ChatStatus {
			snap := w.chatStatusTracker.Snapshot()
			out := make(map[string]bossanovav1.ChatStatus, len(snap))
			for id, e := range snap {
				out[id] = e.Status
			}
			return out
		},
		ProactiveCandidate: func(ctx context.Context, req rotation.ProactiveDecideRequest) (rotation.ProactiveDecision, error) {
			capable, err := w.accountMaterializer.SupportsRotation(ctx, req.Provider)
			if err != nil {
				return rotation.ProactiveDecision{}, err
			}
			if !capable {
				return rotation.ProactiveDecision{Kind: rotation.ProactiveNone}, nil
			}
			usage := probeCandidateUsageForRotationSignal(ctx, log.Logger, w.accountMaterializer, w.accounts, rotation.Signal{
				Provider:        req.Provider,
				Model:           req.Model,
				CappedAccountID: req.BoundAccountID, // excluded from candidates
				Kind:            rotation.UsageLimited,
			})
			cand, err := w.rotationEngine.SelectProactiveCandidate(ctx, req.Provider, req.Model, req.BoundAccountID, usage)
			if err != nil {
				return rotation.ProactiveDecision{}, err
			}
			if cand == nil {
				return rotation.ProactiveDecision{Kind: rotation.ProactiveNone}, nil
			}
			return rotation.ProactiveDecision{
				Kind:          rotation.ProactiveSwitch,
				AccountID:     cand.ID,
				Label:         cand.Label,
				CandidateUtil: rotation.UsageUtil(usage[cand.ID]),
			}, nil
		},
		CaptureProactiveRotation: w.rotationEngine.CaptureProactiveRotation,
		CaptureReactiveRotation:  w.rotationEngine.CaptureReactiveRotation,
	})
	w.cronGate = session.NewCronCompletionGate(session.CronCompletionGateDeps{
		Sessions:  w.sessions,
		Finalizer: w.lifecycle,
		Logger:    log.Logger,
		// Gate finalize on the run actually being over. The Stop hook fires every
		// turn (including mid-run pauses awaiting a background subagent), so without
		// this a paused run would be finalized — opening a junk PR and Blocking a
		// still-working session. Same criterion the stranded-cron sweep uses.
		RunCompletionEvidence: w.lifecycle.CronRunCompletionEvidence,
		// Defer an auth-failed chat instead of finalizing it; the stranded sweep
		// blocks it (BOS-1530).
		ChatAuthFailed:  w.lifecycle.SessionChatAuthFailed,
		DeferDroppedRun: w.lifecycle.DeferDroppedRunFinalization,
	})
	w.lifecycle.SetCronCompletionNotifier(w.cronGate)
	// Wire the lifecycle into HostServiceServer so plugin-side StartChatRun
	// (Task 4) can spawn tmux-hosted runs through the same path the cron
	// scheduler uses. SetLifecycle accepts the narrow ChatLifecycle
	// interface — *session.Lifecycle satisfies it — and is a no-op when
	// no plugins are loaded (HostService is nil in that branch).
	if hs := w.hostService; hs != nil {
		hs.SetLifecycle(w.lifecycle)
		// Wire the poll-fallback armer so StartSession / StartTmuxChat
		// can drive completion for hookless agents (e.g. codex). Plugins
		// that own a finalize hook (claude) report IsSupported=true and
		// never reach the armer. Cadence + jitter chosen to balance
		// responsiveness with idle CPU cost on a daemon hosting many
		// concurrent runs.
		w.lifecycle.SetPollCompleter(hs)
		// Same object also owns the headless question-hook token registry
		// (BOS-486), which authenticates a headless run's POSTs to
		// /hooks/question/{agent_session_id}. Wired separately from the
		// completer so the two concerns stay independent.
		w.lifecycle.SetQuestionHookRegistrar(hs)
		pollFallback := agent.NewPollFallback(log.Logger, 2*time.Second, 200*time.Millisecond, w.lifecycle)
		w.lifecycle.SetPollArmer(pollFallback)
	}

	// Liveness checker resolves whether a session's agent is still running
	// (headless subprocess or tmux chat). The Dispatcher already routes
	// per-session internally, so agentForSession can return it unconditionally;
	// the closure shape lets non-dispatcher wirings (e.g. unit tests) resolve
	// per session. Built here — before the startup cron-recovery pass below — so
	// the stranded-cron sweep can reap logless / agent-dead runs immediately on
	// boot instead of waiting out the durable log-idle threshold. Reused for the
	// session creator, orchestrator, and cron overlap checker.
	agentForSession := func(_ *models.Session) agent.Runner {
		return w.agentRunner
	}
	w.livenessChecker = taskorchestrator.NewLivenessChecker(w.sessions, w.agentChats, agentForSession, w.tmuxClient, w.lifecycle.StartedAt())
	w.lifecycle.SetSessionLiveness(w.livenessChecker)

	// Auto-rotation wiring. Install policy knobs, the real rotation.Engine, and
	// the live session→account binding/materializer adapters. Existing gates
	// still apply: rotation.enabled, ImplementingPlan, per-repo CanAutoRotate,
	// account-0/unbound sessions, and Block-on-error degradation.
	// Live re-read of the rotation kill-switch, shared by the lifecycle
	// auto-rotation loader and the task/cron session creator. Each caller sees
	// the current on-disk value rather than the boot-time snapshot.
	rotationConfigLoader := func() (config.ManagedAgentAccountsConfig, error) {
		loaded, err := config.Load()
		if err != nil {
			return config.ManagedAgentAccountsConfig{}, err
		}
		return loaded.ManagedAccounts, nil
	}
	w.lifecycle.SetRotationConfig(w.settings.ManagedAccounts)
	w.lifecycle.SetRotationConfigLoader(rotationConfigLoader)
	w.lifecycle.SetRateLimitProbe(rateLimitProbe)
	w.lifecycle.SetRotationDecider(func(ctx context.Context, sig rotation.Signal) (rotation.Outcome, error) {
		return w.rotationEngine.Decide(ctx, cacheUsageProbeForRotationSignal(ctx, log.Logger, w.accountMaterializer, w.accounts, sig))
	})
	w.lifecycle.SetRotationRecorder(w.rotationRecorder)
	w.lifecycle.SetAgentAccountMaterializer(accountwiring.NewLifecycleMaterializer(w.accountMaterializer))
	w.lifecycle.SetRotationBinding(accountwiring.NewRotationBindingResolver(accountwiring.NewRegistry(w.accounts), w.accountMaterializer))
	// Account-by-id getter for CurrentBearer's first-leg sentinel→bearer
	// translation (BOS-326): resolves the bound account the materializer needs.
	w.lifecycle.SetAgentAccountGetter(w.accounts.Get)
	// Managed-default account resolver for the startup pane-token adoption sweep
	// (BOS-481): mirrors the spawn's DefaultAgentAccountID for a surviving pane whose
	// chat and session both lack a persisted binding.
	w.lifecycle.SetDefaultAgentAccountResolver(w.accountResolver.DefaultAgentAccountID)
	// Probe-skipping pane repair for a 401 the failover proxy minted itself
	// (BOS-982). The proxy attributes its own unknown-token rejection to a live
	// pane and lands here; the account was never consulted for that 401, so the
	// rotator respawns in place rather than probing a credential that is not the
	// problem. Nil-guarded: chatRotator is only constructed when the rotation
	// lane is wired, and without it the proxy's 401 behaves exactly as before.
	w.lifecycle.SetPaneRepairDispatcher(func(agentSessionID string) {
		if w.chatRotator == nil {
			return
		}
		w.chatRotator.OnProxyTokenUnresolved(agentSessionID)
	})
	if w.opts.onRotationSeamsWired != nil {
		w.opts.onRotationSeamsWired(w.lifecycle.HasLiveRotationSeams())
	}
	if w.opts.onRelabelShutdownProbe != nil {
		w.opts.onRelabelShutdownProbe(w.lifecycle.BackgroundRelabelsStopped)
	}
	// Operator-facing startup diagnostic (BOS-315): report whether the
	// auto-rotation lane can actually fire — the wired seams, the boot-time
	// kill-switch snapshot (re-read live per decision), and the per-repo
	// CanAutoRotate opt-in counts. Fail-soft: a repo-list error never blocks boot.
	allRepos, repoListErr := w.repos.List(context.Background())
	logRotationLaneAvailability(log.Logger, w.lifecycle.HasLiveRotationSeams(), w.settings.ManagedAccounts.ManagedAgentAccountsEnabled(), allRepos, repoListErr)

	// Recover sessions left in Finalizing from a previous daemon crash.
	// They can't be safely re-driven (we don't know whether EnsurePR ran
	// or whether the finalize chat was spawned), so we record
	// failed_recovered on their cron_job and transition them to Blocked
	// for the operator to investigate. Worktrees are preserved.
	if n, err := w.lifecycle.RecoverFinalizingSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to recover Finalizing sessions")
	} else if n > 0 {
		log.Info().Int("count", n).Msg("recovered sessions stuck in Finalizing from previous run")
	}

}
