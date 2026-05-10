package main

// ignoredDirtyFiles enumerates files in a codex-managed worktree that
// bossd writes itself and that must NOT be treated as agent-authored
// changes. Codex has no `.claude/settings.local.json` equivalent — there
// is no Stop-hook config because ConfigureFinalizeHook returns
// IsSupported=false for codex (see server.go) — so this list is empty
// for now. Defined as a non-nil slice to match the AgentRunnerService
// contract: ListIgnoredDirtyFiles returns Paths:[], not Paths:nil.
var ignoredDirtyFiles = []string{}
