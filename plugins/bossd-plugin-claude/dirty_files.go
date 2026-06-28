package main

// ignoredDirtyFiles enumerates files in a Claude-managed worktree that
// bossd writes itself. They show up as untracked in `git status` but
// must NOT be treated as agent-authored changes.
//
//	.claude/settings.local.json — the Stop-hook config; contains a
//	bearer token, so misclassifying it as an agent change would risk
//	pushing credentials to the remote.
//
//	.claude/scheduled_tasks.lock — Claude/bossd-owned scheduler lock;
//	misclassifying it as agent output would create noisy dirty worktrees
//	and unnecessary finalize chats.
//
//	.superpowers/ — superpowers-framework scratch dir. Skills create it
//	while they run (including during the finalize/stop skills), so the
//	agent cannot reliably clean it before finalize; treat it as ignorable.
var ignoredDirtyFiles = []string{
	".claude/settings.local.json",
	".claude/scheduled_tasks.lock",
	".superpowers/",
}
