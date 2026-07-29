package main

// ignoredDirtyFiles enumerates files in an opencode-managed worktree that bossd
// writes itself and that must NOT be treated as agent-authored changes.
//
// It holds exactly one entry, the BOS-486 question-signal hook that StartRun
// injects (see questionhook.go). bossd's finalize path calls
// ListIgnoredDirtyFiles and exact-matches the results against `git status
// --porcelain` lines (stripBossdManagedFilesWith), so that a "changed nothing"
// run is not misclassified as having produced an untracked file — the
// pr_failed → Blocked shape the claude twin's .claude/settings.local.json
// entry exists to prevent.
//
// This list is the SECONDARY filter, not the primary one: it only matches in a
// repo that already tracks something under `.opencode/plugins/`. The
// `.git/info/exclude` pattern bossd maintains is what covers the common case.
// Do not delete that entry on the strength of this list — full reasoning and
// the measurement in
// docs/solutions/logic-errors/spike-opencode-question-signal-events-unreachable.md
// ("Canonical: which dirty-file filter actually keeps the injected asset out of
// finalize").
//
// Everything ELSE about opencode leaves the worktree clean, and that remains
// the steady state — not a stub:
//
//   - opencode persists session state under the opencode data dir
//     (~/.local/share/opencode/, resolved via `opencode db path`), NOT inside
//     the worktree, so a run leaves no other bossd-written worktree file.
//   - opencode's default snapshot feature uses a SHADOW git repo (a separate
//     object store), which normally does not dirty the real worktree.
//   - a project-local `.opencode/` may also hold USER-authored config or
//     plugins; those are deliberately NOT listed — they are the repo's own
//     files, not bossd's, so only bossd's own filename is ignored here.
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
var ignoredDirtyFiles = []string{
	".opencode/plugins/bossd-question.js",
}
