package main

import (
	"context"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog/log"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/domain"
)

func (w *daemonWiring) wireStreamObservers() {
	// --- Server ---

	// --- Upstream (optional, cloud mode) ---
	//
	// The legacy upstream.Manager (heartbeat + SyncSessions loops) was
	// replaced in T3.7 by upstream.StreamClient, which opens a single
	// long-lived DaemonStream and receives commands the orchestrator
	// pushes. Bootstrap sequence:
	//   1. Build the Connect client against BOSSD_ORCHESTRATOR_URL.
	//   2. Call RegisterDaemon with the WorkOS JWT to obtain a
	//      session_token (bosso persists the daemon's identity).
	//   3. Construct StreamClient with adapters that wrap the existing
	//      stores/lifecycle/tmux reader — no new subsystems needed.
	//   4. Launch Run(ctx) in a tracked goroutine. It owns reconnects,
	//      token refresh, snapshot, delta forwarding, and command
	//      dispatch on its own.
	//
	// streamBus is created earlier (alongside the chat-status tracker
	// wiring) so the chat-status hook can publish per-chat ChatStatusDelta
	// events. We reuse that bus here.

	// Wire the display-status computer's post-write hook into the stream
	// bus so every Recompute that actually writes a new (label, intent,
	// spinner) trio fans out a SessionDelta{UPDATED} on the reverse
	// stream. Without this, bosso only ever sees the initial
	// DaemonSnapshot — labels recomputed after startup (PR check
	// results, chat status, workflow transitions) never reach the web UI
	// and every session shows whatever it computed to before the gh
	// poller had run, which is uniformly "idle" for sessions whose chat
	// status is IDLE and whose PR check state is UNSPECIFIED.
	publishSessionUpdated := func(ctx context.Context, sessionID string) {
		row, err := w.rawSessions.Get(ctx, sessionID)
		if err != nil {
			log.Debug().Err(err).Str("session_id", sessionID).Msg("display update: session lookup failed")
			return
		}
		pbSess := server.SessionToProto(row)
		// Populate the joined repo display name. bosso applies session
		// deltas as full replacements (state.go applySessionDelta), so
		// omitting this would clobber the populated value the initial
		// DaemonSnapshot set and the web UI would lose the Repo column.
		if row.RepoID != "" {
			if r, err := w.repos.Get(ctx, row.RepoID); err == nil && r != nil {
				pbSess.RepoDisplayName = r.DisplayName
				pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
			}
		}
		// Full-replacement semantics (above) also mean this recompute-driven
		// delta must re-assert the observability overlay, or it would clobber a
		// live AGENT_AUTH_FAILED that the auth-change hook published.
		w.hydrateSessionForStream(ctx, pbSess)
		w.streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
				Session: pbSess,
			},
		})
	}
	w.displayComputer.SetOnUpdate(publishSessionUpdated)
	// Marker-only PR edits change attention even when the display trio stays
	// identical. The tracker calls this after recomputing that trio.
	w.displayTracker.SetOnAttentionChange(func(sessionID string) {
		publishSessionUpdated(context.Background(), sessionID)
	})
	// A restart-proof activity read that outlasted its caller's wait landed
	// after the delta or snapshot that started it went out with the older
	// value; republish so bosso and the web see it.
	w.wedgeActivityCache.SetOnChange(func(sessionID string) {
		publishSessionUpdated(context.Background(), sessionID)
	})

	// Evidence and resume ledgers can change attention without changing the
	// display trio. Publish explicitly even when Recompute has no row to write.
	refreshAttention := func(ctx context.Context, sessionID string) {
		_ = w.displayComputer.Recompute(ctx, sessionID)
		publishSessionUpdated(ctx, sessionID)
	}
	w.evidenceReader.SetOnChange(refreshAttention)
	w.resumeAttempts.SetOnChange(func(sessionID string) {
		refreshAttention(context.Background(), sessionID)
	})

	// BOS-1535: the resolved-alert sweep clears a latched repair alert (and its
	// stall record) once the head moved, CI went green, or the PR left the
	// repair-eligible states. A cleared row rarely changes the display trio, so
	// Recompute alone would not reach the stream — publish the UPDATED delta
	// explicitly after recomputing.
	w.lifecycle.SetAlertClearDeps(w.displayTracker, w.repairLease, func(ctx context.Context, sessionID string) {
		_ = w.displayComputer.Recompute(ctx, sessionID)
		publishSessionUpdated(ctx, sessionID)
	})

	// A Blocked run whose own chat works again, or parks on a callback it
	// armed in this block, reads in progress while it does (the display
	// tracker carries the mark Session.attention reads); the mark changes no
	// display trio, so publish the delta explicitly.
	w.lifecycle.SetBlockedRunDeps(w.chatWorking, w.progressWait, w.displayTracker, publishSessionUpdated)

	// Wire the auth-failed change hook: when a chat's login-required state flips
	// — which need not coincide with any chat STATUS change — resolve it to its
	// session and emit a hydrated SessionDelta{UPDATED}. Without this the
	// AGENT_AUTH_FAILED attention only ever appears on local daemon reads
	// (GetSession/ListSessions); the cloud/web read model, fed solely by the
	// reverse stream, would never see it for a session whose status stays
	// WORKING. SetAuthFailed gates the hook on an effective-state transition, so
	// the poller's per-tick calls don't storm the stream.
	w.chatStatusTracker.SetOnAuthChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishAgentMarkerSessionDelta(ctx, agentSessionID, w.streamHydrator, w.streamBus, log.Logger)
		// BOS-316: on an auth-failed SET transition, dispatch the auto-rotator so a
		// typed 401 pane rotates to a healthy account with zero LLM turns. This hook
		// also fires on the CLEAR (recovery) transition, on which there is nothing to
		// rotate — gate on the post-set AuthFailed state (the hook runs after the
		// tracker's map update, so AuthFailed reflects the new state) so a normal
		// re-login does not dispatch a no-op rotation that records a spurious
		// "recovered" audit row and burns the per-chat rate-limit slot. Non-blocking
		// and fail-safe (the rotator confirms via probe and self-throttles); nil-guarded
		// because a daemon without a rotation-capable plugin leaves chatRotator unset.
		if w.chatRotator != nil && w.chatStatusTracker.AuthFailed(agentSessionID) {
			w.chatRotator.OnAuthFailed(agentSessionID)
		}
	})

	// BOS-667: republish the session on every agent-stalled transition, for the
	// same reason as the auth hook above — the cloud/web read model is fed solely
	// by the reverse stream, and a stalled chat's status stays WORKING, so nothing
	// else would ever emit a delta carrying the AGENT_STALLED attention. The
	// tracker's SetStalled debounce means only real edges reach here. Unlike auth
	// there is no automated remedy to dispatch: a stall has no single known cause,
	// so the daemon surfaces it and leaves the decision to a human.
	w.chatStatusTracker.SetOnStalledChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishAgentMarkerSessionDelta(ctx, agentSessionID, w.streamHydrator, w.streamBus, log.Logger)
	})

	// BOS-518: dispatch the bounded auto-resume consumer on every transient-API
	// -error transition. Unlike the auth hook above this deliberately does NOT
	// gate on the post-set marker state: OnTransientAPIError needs BOTH edges.
	// The SET edge arms the settle timer, and the CLEAR edge is the only positive
	// evidence the chat recovered — it cancels the armed cycle and zeroes the
	// attempt budget. Gating on TransientAPIError(...) would swallow the clear,
	// leaving a chat that recovered and later failed again with a partially spent
	// budget. The resumer re-checks the marker itself on entry, so passing both
	// edges through costs nothing. Nil-guarded: the resumer is constructed later
	// (it needs srv), and the poller cannot fire this hook before then anyway.
	w.chatStatusTracker.SetOnTransientAPIErrorChange(func(agentSessionID string) {
		if w.transientResumer != nil {
			w.transientResumer.OnTransientAPIError(agentSessionID)
		}
	})
	w.transientResumeHookInstalled = true

}

// progressWait reports whether a live progress callback armed at or after
// armedSince (zero means any) holds chat. The blocked-run sweep and the tmux
// reaper share it.
func (w *daemonWiring) progressWait(ctx context.Context, chat *domain.AgentChat, sess *models.Session, armedSince time.Time) (bool, error) {
	repo, err := w.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return false, err
	}
	return callback.HoldsProgressWait(ctx, w.githubCallbacks, chat.AgentSessionID, sess, vcs.GitHubNWO(repo.OriginURL), armedSince, time.Now())
}
