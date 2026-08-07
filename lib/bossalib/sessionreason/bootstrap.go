package sessionreason

// BootstrapStranded is the persisted blocked reason for a session reclaimed by
// the stranded-bootstrap reaper (BOS-717): it never got past worktree creation
// or agent start, and the goroutine that was creating it is gone — either the
// daemon restarted under it, or its bootstrap outlived the bootstrap deadline
// and no longer has an owner.
func BootstrapStranded() string {
	return "session bootstrap never completed; worktree and branch were cleaned up"
}
