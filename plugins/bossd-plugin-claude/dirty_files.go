package main

// ignoredDirtyFiles enumerates files in a Claude-managed worktree that
// bossd writes itself. They show up as untracked in `git status` but
// must NOT be treated as agent-authored changes.
//
//	.claude/settings.local.json — the Stop-hook config; contains a
//	bearer token, so misclassifying it as an agent change would risk
//	pushing credentials to the remote.
var ignoredDirtyFiles = []string{
	".claude/settings.local.json",
}
