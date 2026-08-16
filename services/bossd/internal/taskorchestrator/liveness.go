package taskorchestrator

import (
	"context"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
)

// SessionLivenessChecker reports whether a task-orchestrated session
// is still making progress. Used by the recovery sweep to detect stuck tasks.
//
// The verdict is three-valued on purpose (see session.Liveness). The older
// boolean IsSessionAlive was deliberately removed rather than kept as a
// wrapper: any call site still consuming a boolean would have to fold
// session.LivenessParked into either "alive" or "dead", and folding it into
// "dead" is exactly the bug BOS-884 exists to prevent. Forcing every caller to
// name what it does with a parked session keeps that from creeping back in.
type SessionLivenessChecker interface {
	SessionLiveness(ctx context.Context, sessionID string) session.Liveness
}

// defaultLivenessChecker checks liveness by looking at the session state
// and whether the agent process or tmux session is still running.
type defaultLivenessChecker struct {
	sessions        db.SessionStore
	chats           db.AgentChatStore
	agentForSession func(*models.Session) agent.AgentRunner
	tmux            *tmux.Client
}

// NewLivenessChecker creates a SessionLivenessChecker backed by the
// session store, chat store, a per-session agent runner resolver, and
// tmux client. agentForSession returns the AgentRunner that should be
// queried for a given session — typically a Dispatcher that does its
// own internal routing — and may return nil for sessions whose agent
// plugin isn't loaded; SessionLiveness treats nil as "skip the runner
// check, fall through to tmux/chat liveness signals" rather than as
// a fatal error.
func NewLivenessChecker(sessions db.SessionStore, chats db.AgentChatStore, agentForSession func(*models.Session) agent.AgentRunner, tmux *tmux.Client) SessionLivenessChecker {
	return &defaultLivenessChecker{sessions: sessions, chats: chats, agentForSession: agentForSession, tmux: tmux}
}

// tmuxHostedSession reports whether the session runs inside a tmux pane rather
// than as a bare headless subprocess: a cron job, a tmux_unattended session, or
// a detach run.
//
// This is the twin of session.isUnattendedSession — same three fields, same
// meaning — duplicated because that one is unexported and this package may not
// reach into session's internals. Keep the two in step: a fourth field added
// there and not here silently reclassifies runs at every call site below.
//
// Detach is read straight off the persisted row on purpose. It is not "the user
// asked for --detach": session.Lifecycle only records it when tmux was actually
// available at start, and a detach run that fell back to a paneless subprocess
// leaves it false and stays in the headless class. So the row already carries
// the tmux-hosted distinction this function needs, with no tmux probe.
func tmuxHostedSession(sess *models.Session) bool {
	if sess == nil {
		return false
	}
	return (sess.CronJobID != nil && *sess.CronJobID != "") || sess.IsTmuxUnattended || sess.Detach
}

func (c *defaultLivenessChecker) SessionLiveness(ctx context.Context, sessionID string) session.Liveness {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.LivenessDead
	}

	// If the session has advanced past ImplementingPlan, VCS events
	// (checks passed/failed, PR merged/closed) handle completion.
	// The task is not stuck.
	switch sess.State {
	case machine.CreatingWorktree, machine.StartingAgent, machine.ImplementingPlan:
		// Early states -- fall through to agent liveness check below.
	default:
		// All post-ImplementingPlan states (PushingBranch, AwaitingChecks, etc.)
		// are driven by VCS events, not the agent. The task is not stuck.
		return session.LivenessAlive
	}

	hasAgentID := sess.AgentSessionID != nil && *sess.AgentSessionID != ""

	// Check headless agent runner. agentForSession may return nil for
	// sessions whose agent plugin isn't loaded — fall through to the
	// tmux/chat checks rather than treating a missing runner as fatal.
	if hasAgentID {
		runner := c.agentForSession(sess)
		if runner != nil && runner.IsRunning(*sess.AgentSessionID) {
			return session.LivenessAlive
		}
	}

	// Check per-chat tmux sessions (interactive agent).
	//
	// A chat row whose tmux pointer is cleared is the durable trace of a
	// DELIBERATE teardown: KillChatTmuxSession kills the pane and only then
	// nulls agent_chats.tmux_session_name, so the row outliving its pointer
	// means somebody reaped the pane on purpose (BOS-884).
	//
	// "Cleared" and "never set" are indistinguishable within a single row, so
	// the rows that never had a pane must be excluded explicitly — counting one
	// as parked makes its session permanently un-reapable, silently disabling
	// sweepOrphanedHeadlessRuns, the cron/stranded completion evidence,
	// recoverStaleTasks and the duplicate-PR/branch guard release. That is the
	// mirror image of the bug this verdict exists to prevent, so it is guarded
	// with the same care. Two kinds of row can never have had a pane:
	//
	//   - a failed-start row (recordFailedStartChat, session/tmux_chat.go): the
	//     launch errored, so no pane was ever created for it;
	//   - the headless primary row (session/lifecycle.go): codex exec /
	//     claude --print runs never pass through StartTmuxChat, which is the
	//     only place a pointer is ever stamped, so their pointer is NULL by
	//     construction and stays NULL for the life of the row.
	//
	// A headless run is identified the same way sweepOrphanedHeadlessRuns
	// identifies one: a session carrying an agent run id whose provenance is
	// not tmux-hosted (cron / tmux_unattended / detach). An interactive session
	// whose chats came from StartTmuxChat has no agent run id of its own — none
	// of the three writers of sessions.agent_session_id run for it — so a reaped
	// interactive pane still reads parked, which is the case BOS-884 is about.
	tmuxHosted := tmuxHostedSession(sess)
	headlessRun := hasAgentID && !tmuxHosted

	// A session can hold several chats, and they are torn down independently:
	// KillChatTmuxSession clears one chat's pointer while the others keep
	// running. So "some chat was reaped" is not a claim about the session until
	// no OTHER chat has died on its own — a chat that still holds its pointer
	// and has lost its pane is positive evidence of death, and it outranks a
	// sibling's reap, which says nothing about it. Tracking it separately keeps
	// one stale reaped row from masking a genuinely dead session forever.
	//
	// Deliberately scoped to the chat rows: the legacy session-level tmux name
	// below is written by exactly one site, and only ever to clear it, so
	// treating its absent pane as death would add risk without adding evidence.
	parked := false
	deadPointed := false
	if c.tmux != nil && c.chats != nil {
		chats, chatErr := c.chats.ListBySession(ctx, sessionID)
		if chatErr == nil {
			for _, chat := range chats {
				if chat == nil {
					continue
				}
				if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
					if headlessRun || (chat.StartError != nil && *chat.StartError != "") {
						continue
					}
					parked = true
					continue
				}
				if c.tmux.HasSession(ctx, *chat.TmuxSessionName) {
					return session.LivenessAlive
				}
				deadPointed = true
			}
		}
	}

	// Legacy: check session-level tmux name.
	hasTmuxName := sess.TmuxSessionName != nil && *sess.TmuxSessionName != ""
	if hasTmuxName && c.tmux != nil && c.tmux.HasSession(ctx, *sess.TmuxSessionName) {
		return session.LivenessAlive
	}

	// A surviving chat row with no pane outranks every remaining branch: having
	// excluded the rows that never had one, it proves a pane once existed (so
	// the session is not "still initializing") and that its disappearance was
	// deliberate (so it is not death).
	if parked && !deadPointed {
		return session.LivenessParked
	}

	// If neither process identifier is set, the session is still initializing
	// (e.g. quick chat waiting for first user attach). Unattended sessions are
	// the exception: a daemon restart cannot leave their pre-agent work running,
	// so recovery must be able to reap them rather than treating them as live.
	if !hasAgentID && !hasTmuxName {
		if tmuxHosted {
			return session.LivenessDead
		}
		if c.chats != nil {
			// Check if any chats exist with tmux names.
			chats, chatErr := c.chats.ListBySession(ctx, sessionID)
			if chatErr == nil && len(chats) == 0 {
				return session.LivenessAlive
			}
		} else {
			return session.LivenessAlive
		}
	}

	return session.LivenessDead
}
