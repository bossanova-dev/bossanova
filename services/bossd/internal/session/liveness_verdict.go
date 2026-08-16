package session

// Liveness is the answer to "is this boss session's agent still running?".
//
// It is deliberately three-valued. Once panes can be reaped on purpose
// (BOS-883), "the pane is gone" stops being a synonym for "the agent exited":
// every deliberate teardown clears agent_chats.tmux_session_name as part of the
// kill, so a chat row that still exists with a cleared pointer is a session
// that was PARKED, not one that died. A boolean cannot carry that: reporting a
// parked session alive strands the orchestrator's stuck-task handling forever,
// and reporting it dead fails the task and releases the duplicate-PR/branch
// guard — the two failures this distinction exists to prevent.
//
// The type lives in this package rather than taskorchestrator because
// taskorchestrator already imports session (never the reverse), and because the
// meaning of a cleared pointer is defined here, by KillChatTmuxSession and
// loglessTmuxCompletionEvidence.
type Liveness int

const (
	// LivenessDead means the session's agent is gone and nothing deliberate
	// put it in that state. Callers may reap, fail, or finalize.
	//
	// It is the zero value on purpose: that is what the boolean this type
	// replaced returned by default (`false` = not alive), so a stub or a
	// zero-valued struct field keeps behaving exactly as it did before the
	// verdict was widened, rather than silently acquiring a new meaning.
	LivenessDead Liveness = iota

	// LivenessAlive means a live agent process or tmux pane was observed, or
	// the session has advanced past the states where the agent drives it.
	LivenessAlive

	// LivenessParked means the session's pane was torn down on purpose — a
	// chat row survives with its tmux pointer cleared. It is evidence of
	// neither progress nor death, so a caller must do NOTHING with it: not
	// fail the task, not release the duplicate-PR/branch guard, not finalize,
	// not orphan.
	LivenessParked
)

func (l Liveness) String() string {
	switch l {
	case LivenessAlive:
		return "alive"
	case LivenessParked:
		return "parked"
	default:
		return "dead"
	}
}
