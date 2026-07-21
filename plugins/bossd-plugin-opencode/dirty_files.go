package main

// ignoredDirtyFiles enumerates files in an opencode-managed worktree that bossd
// writes itself and that must NOT be treated as agent-authored changes.
//
// It is EMPTY for opencode, and that is the correct steady state — not a stub:
//
//   - opencode persists session state under the opencode data dir
//     (~/.local/share/opencode/, resolved via `opencode db path`), NOT inside
//     the worktree, so a normal run leaves no bossd-written worktree file.
//   - opencode's default snapshot feature uses a SHADOW git repo (a separate
//     object store), which normally does not dirty the real worktree.
//   - a project-local `.opencode/` directory MAY appear if a plugin writes to
//     it; if that becomes common, add it here.
//
// Defensive caveat (coordinated with the spawn path, part 2 / BOS-434): if
// opencode is ever spawned inside a git-hook context, a leaked GIT_INDEX_FILE
// (an upstream opencode bug) can corrupt the real .git/index. The durable fix
// is to sanitize GIT_* from the child environment on the SPAWN path (the shared
// agentruntime runner), not to enumerate a dirty file here — this list is the
// wrong layer for it. That sanitization is intentionally left to a dedicated
// spawn-path change rather than bundled into this introspection slice, because
// the spawn helper is shared across every agent runner (codex, claude) and the
// scrub belongs to their common review, not opencode's SQLite reader.
//
// Defined as a non-nil slice to match the AgentRunnerService contract:
// ListIgnoredDirtyFiles returns Paths:[], not Paths:nil.
var ignoredDirtyFiles = []string{}
