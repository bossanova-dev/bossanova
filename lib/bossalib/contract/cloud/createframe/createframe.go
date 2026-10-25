// Package createframe classifies session creation stream frames.
package createframe

// IsAccepted reports whether a created frame represents acceptance before
// bootstrap finishes. Attached sessions and quick chats emit a single terminal
// frame. Older daemons also emit only the settled frame, whose worktree path is
// populated; checking it preserves termination across independently upgraded
// daemons and clients. Callers must track whether acceptance was already seen,
// since the next created frame is terminal even if its worktree path is empty.
func IsAccepted(attachedExisting, isQuickChat bool, worktreePath string) bool {
	return !attachedExisting && !isQuickChat && worktreePath == ""
}
