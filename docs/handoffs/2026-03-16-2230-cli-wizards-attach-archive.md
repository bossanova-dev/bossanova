## Handoff: Flight Leg 5b — Wizards + Attach + Archive

**Date:** 2026-03-16 22:30
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-rrb: Implement new session wizard (repo select, PR mode, plan input, confirm)
- bossanova-4ks: Implement repo management views (add repo wizard, repo list, remove)
- bossanova-9zq: Implement attach view with server-streaming output and Ctrl+C detach
- bossanova-nux: Implement archive, resurrect, and trash commands

### Files Changed

- `services/boss/internal/views/newsession.go:1-368` — Multi-step new session wizard: repo picker (auto-select single repo), PR mode choice (new vs existing), PR picker from ListRepoPRs, title textinput, plan textarea (Ctrl+D to finish), confirmation screen, CreateSession RPC call. 6 wizard steps with esc cancel.
- `services/boss/internal/views/repo.go:1-339` — Repo add wizard: path input (cwd default), display name (basename default), base branch ("main" default), worktree dir, optional setup script, confirmation. Repo list: table with cursor nav, 'd' to delete with y/n confirmation. Wired to RegisterRepo, ListRepos, RemoveRepo RPCs.
- `services/boss/internal/views/attach.go:1-229` — Attach view: streams AttachSession RPC via connect.ServerStreamForClient. Session header (title, state, branch). Viewport scrollable output from OutputLine events. State change annotations. Session ended banner. Ctrl+C/esc detaches (cancels stream context). Continues reading after each event.
- `services/boss/internal/views/app.go:1-157` — Updated App model: added repoAdd, repoList, attach fields. switchViewMsg carries optional sessionID. SetInitialView/SetAttachSession for direct CLI launch. switchToHome helper. View routing for all 5 view types.
- `services/boss/internal/views/home.go` — Added 'n' (new session), 'r' (repo list), 'enter' (attach) key bindings.
- `services/boss/cmd/handlers.go:1-225` — Replaced stubs: runNew/runAttach launch TUI with specific views. runRepoAdd launches add wizard. runRepoLS non-interactive tabwriter table. runRepoRemove direct RPC + stdout. runArchive/runResurrect direct RPCs. runTrashEmpty with --older-than duration parser (h/d/w units) + timestamppb conversion.

### Learnings & Notes

- **Bubbletea v2 bubbles textinput/textarea**: `Placeholder` is a direct field (not a setter). `SetWidth`, `SetHeight`, `Focus()`, `Blur()` are methods. `Value()` returns string.
- **gofmt alignment strictness**: Go's formatter requires consistent tab-aligned fields in structs. Mixed-width field names cause gofmt violations — run `gofmt -w` to auto-fix.
- **Stream continuation pattern**: After receiving a stream event (OutputLine, StateChange), must return `readFromStream(m.stream)` as the next Cmd to continue reading. One-shot reads without continuation will appear to hang.
- **App view switching**: Used `switchViewMsg` with a `view` field + optional `sessionID` for attach. The App model's Update dispatches to the correct sub-model and checks for Cancelled/Done/Detached to return to home.
- **Duration parsing for --older-than**: Custom `parseDuration` supports "30d", "2w", "1h" format, converts to `time.Duration`, then to `timestamppb.Timestamp` for the RPC.

### Issues Encountered

- Bubbletea v2 bubbles API differs from v1 (Placeholder as field vs method). Quickly resolved.
- gofmt alignment issues on mixed-width struct fields. Auto-fixed with `gofmt -w`.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — golangci-lint 0 issues
- Tests: PASSED — all existing tests pass
- Vet: PASSED
- Format: PASSED

### CLI Feature Completeness (Leg 5 overall)

Flight Leg 5 is now complete. All CLI features implemented:

| Feature                       | Status | Command / Key                            |
| ----------------------------- | ------ | ---------------------------------------- |
| Home screen                   | Done   | `boss` (default)                         |
| Session table + polling       | Done   | 2s auto-refresh                          |
| Non-interactive list          | Done   | `boss ls` with --repo/--archived/--state |
| New session wizard            | Done   | `boss new` or 'n' key                    |
| Attach to session             | Done   | `boss attach <id>` or enter key          |
| Repo add wizard               | Done   | `boss repo add`                          |
| Repo list (TUI)               | Done   | 'r' key from home                        |
| Repo list (non-interactive)   | Done   | `boss repo ls`                           |
| Repo remove (non-interactive) | Done   | `boss repo remove <id>`                  |
| Repo remove (TUI)             | Done   | 'd' key in repo list                     |
| Archive session               | Done   | `boss archive <id>`                      |
| Resurrect session             | Done   | `boss resurrect <id>`                    |
| Empty trash                   | Done   | `boss trash empty [--older-than 30d]`    |

### Next Steps (Flight Leg 6: Git Worktree + Claude Process)

Leg 6 implements the daemon-side process management:

- WorktreeManager interface in bossd: Create, Remove, Archive, Resurrect, EmptyTrash
- Claude subprocess manager: Start, Stop, Resume via `claude` CLI
- Wire into session lifecycle
- Setup script execution with 5-minute timeout

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 6 section)
3. Key daemon files: `services/bossd/internal/db/`, `services/bossd/internal/server/`
