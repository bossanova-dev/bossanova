package tmux

// NewSessionOpts configures a new tmux session.
type NewSessionOpts struct {
	Name    string   // Session name (required)
	WorkDir string   // Working directory (required)
	Command []string // Command to run in session (required)
	Width   int      // Initial width (defaults to 200)
	Height  int      // Initial height (defaults to 50)
	// Env sets session-environment variables (tmux `new-session -e KEY=VALUE`)
	// so the launched command — and any later window opened in the session —
	// inherits them. Used to mark cron-spawned sessions with BOSS_CRON=true.
	// The values reach tmux on stdin, never on the process argv, because they
	// include credentials and argv is world-readable through ps (BOS-1511).
	Env map[string]string
	// RemainOnExit arms `remain-on-exit on` on the session after creation so a
	// pane whose process exits stays present (marked pane_dead) with its final
	// screen intact rather than collapsing the session. bossd uses this for chat
	// panes so a fast-exiting agent's output can be captured at death before the
	// pane is reaped (BOS-477).
	RemainOnExit bool
	// ExitOnCompletion overrides inherited remain-on-exit for service panes.
	// It leaves the existing default for other callers unchanged.
	ExitOnCompletion bool
}
