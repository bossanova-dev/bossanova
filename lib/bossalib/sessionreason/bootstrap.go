package sessionreason

const bootstrapStranded = "session bootstrap never completed; worktree and branch were cleaned up"

// BootstrapStranded is the persisted blocked reason for a session reclaimed by
// the stranded-bootstrap reaper (BOS-717): it never got past worktree creation
// or agent start, and the goroutine that was creating it is gone — either the
// daemon restarted under it, or its bootstrap outlived the bootstrap deadline
// and no longer has an owner.
func BootstrapStranded() string {
	return bootstrapStranded
}

// IsBootstrapStranded reports whether a blocked reason was produced by
// BootstrapStranded. Such a session has no worktree and no agent chat left, so
// nothing about it can be opened or resumed.
func IsBootstrapStranded(reason *string) bool {
	return reason != nil && *reason == bootstrapStranded
}
