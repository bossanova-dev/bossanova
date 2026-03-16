## Handoff: Flight Leg 5a — Cobra + Home Screen + List Mode

**Date:** 2026-03-16 21:30
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-k6b: Add Cobra dependency and root command with subcommands skeleton
- bossanova-nrn: Implement Bubbletea app shell with daemon client wiring
- bossanova-7wz: Implement home screen view with session table, state colors, and 2s polling
- bossanova-kyo: Implement boss ls non-interactive mode (list sessions as table to stdout)

### Files Changed

- `services/boss/cmd/main.go:1-139` — Full Cobra command tree: root (TUI), ls, new, attach, repo (add/ls/remove), archive, resurrect, trash empty. All with proper Args validation and flags.
- `services/boss/cmd/handlers.go:1-113` — Handler implementations: runTUI wires Bubbletea program, runLS queries daemon and outputs tabwriter table with --repo/--archived/--state filters. Other handlers are stubs.
- `services/boss/internal/views/app.go:1-90` — Root Bubbletea model: manages view routing (ViewHome, ViewNewSession, ViewAttach, ViewRepoAdd, ViewRepoList), daemon client context, alt screen via View.AltScreen, Ctrl+C quit.
- `services/boss/internal/views/home.go:1-242` — Home screen: session table with lipgloss-styled columns (ID, title, state, branch, PR#, CI), color-coded states (green/yellow/red/cyan/gray), 2s polling via tea.Tick, j/k arrow nav, action bar. Exports StateLabel and ChecksLabel for reuse.
- `services/boss/go.mod` — Added charm.land/bubbletea/v2, charm.land/lipgloss/v2, charm.land/bubbles/v2, github.com/spf13/cobra

### Learnings & Notes

- **Bubbletea v2 moved to charm.land**: Import paths are `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2` — NOT `github.com/charmbracelet/...`. The GitHub paths fail with module path mismatch errors.
- **Bubbletea v2 View() returns tea.View**: Not `string` like v1. Use `tea.NewView(s)` to wrap strings. Alt screen is now a field on `tea.View` (`v.AltScreen = true`), not a program option.
- **Bubbletea v2 no WithAltScreen**: `tea.WithAltScreen()` doesn't exist. Set `AltScreen = true` on the `tea.View` returned from `View()`.
- **lipgloss v2 Color() is a function**: `lipgloss.Color("#hex")` returns `color.Color` (from `image/color`), not a type. State color helpers must return `color.Color`.
- **Exported helpers for shared use**: `StateLabel()` and `ChecksLabel()` are exported from the views package so both TUI and non-interactive `boss ls` can share state display logic.

### Issues Encountered

- Bubbletea v2 API changes required several iterations (View return type, AltScreen pattern, lipgloss Color type). All resolved.
- golangci-lint caught errcheck violations in tabwriter fmt calls and unused `err` field. Fixed.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — golangci-lint 0 issues
- Tests: PASSED — 26 total (18 machine + 8 db)
- Vet: PASSED
- Format: PASSED

### Next Steps (Flight Leg 5b: Wizards + Attach + Archive)

- bossanova-rrb: Implement new session wizard (repo select, PR mode, plan input, confirm)
- bossanova-4ks: Implement repo management views (add repo wizard, repo list, remove)
- bossanova-9zq: Implement attach view with server-streaming output and Ctrl+C detach
- bossanova-nux: Implement archive, resurrect, and trash commands
- bossanova-uel: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/boss/cmd/main.go`, `services/boss/cmd/handlers.go`, `services/boss/internal/views/app.go`, `services/boss/internal/views/home.go`
