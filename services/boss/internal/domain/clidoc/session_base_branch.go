package clidoc

const newSessionBaseBranchLong = "Base branch: --base-branch names the origin branch to cut the worktree from " +
	"and target with its draft PR. Omit it to use the repo's default base. " +
	"It requires --repo and --prompt and cannot be combined with --pr or --quick-chat. " +
	"The daemon refuses a base missing from origin before creating a session or worktree.\n\n"
