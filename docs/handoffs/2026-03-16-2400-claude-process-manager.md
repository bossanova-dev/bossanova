## Handoff: Flight Leg 6b — Claude Process Manager

**Date:** 2026-03-16 24:00
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-r16: Define ClaudeRunner interface and implement process manager
- bossanova-6kb: Implement Claude process stdout/stderr capture with log file + ring buffer
- bossanova-97r: Write unit tests for ClaudeRunner (mock process, ring buffer, subscriber)

### Files Changed

- `services/bossd/internal/claude/runner.go:1-439` — ClaudeRunner interface (Start, Stop, IsRunning, Subscribe, History). Runner implementation using exec.Command with CommandFactory injection for testing. OutputLine type with text + timestamp. Ring buffer (1000 entries, circular) with thread-safe read/write. Subscriber broadcast system with context-aware cleanup. Process lifecycle: stdin pipe for plan, stdout/stderr capture via buffered scanner (1MB line limit), log file writing to `<workDir>/.boss/claude.log` (or custom logDir). Stop with 10s graceful timeout + force kill. Resume support via `--resume` flag. RunnerOption pattern (WithCommandFactory, WithLogDir).
- `services/bossd/internal/claude/runner_test.go:1-364` — 15 test functions. testRunner helper with injected shell echo script. Tests: StartStop lifecycle, IsRunning state transitions, History retrieval (10 lines), LogFileWritten (custom logDir), DefaultLogPath (workDir/.boss/claude.log), Subscriber broadcast + channel close, ring buffer overflow (5-entry), underflow, empty, exact capacity, 1000-entry overflow (eviction at entry-200), StartWithResume (--resume flag capture), unknown session errors (Stop, Subscribe, History).

### Learnings & Notes

- **CommandFactory injection pattern**: The `Runner.cmdFunc` field defaults to `exec.CommandContext` but can be overridden via `WithCommandFactory` for tests. Tests use shell echo scripts (`sh -c "cat > /dev/null; for i in $(seq 1 10); do echo line $i; done"`) instead of the real `claude` CLI.
- **Ring buffer wrap-around**: When `count > size`, the `head` index points to the oldest entry. Reading starts at `head` and wraps around, giving chronological order.
- **Subscriber slow consumer handling**: `broadcast()` uses non-blocking sends (`select/default`) — slow consumers get lines dropped rather than blocking the capture goroutine.
- **Log file path convention**: Default is `<workDir>/.boss/claude.log`. The `WithLogDir` option changes it to `<logDir>/<sessionID>.log` (used in tests to avoid collisions).
- **Process exit cleanup**: The wait goroutine closes the log file, closes all subscriber channels, and closes the `done` channel (signaling IsRunning).
- **Scanner buffer size**: Set to 1MB max per line (`scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)`) to handle large JSON output from `--output-format=stream-json`.

### Issues Encountered

- None — implementation was straightforward. All 15 tests pass on first run.

### Current Status

- Build: PASSED — 3 binaries (bossd, boss, bosso)
- Lint: PASSED — golangci-lint 0 issues
- Tests: PASSED — all packages (claude: 15 tests, db, git)
- Vet: PASSED
- Format: PASSED

### Next Steps (Flight Leg 6c: Session Lifecycle Wiring)

- bossanova-iwc: Create SessionLifecycle orchestrator wiring worktree + claude + state machine
- bossanova-m9x: Wire SessionLifecycle into Server and daemon entry point
- bossanova-ph1: Update AttachSession RPC to stream from Claude ring buffer
- bossanova-wbq: Update RegisterRepo to detect origin URL from git config
- bossanova-nou: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 6 section)
3. Key files: `services/bossd/internal/claude/runner.go`, `services/bossd/internal/git/worktree.go`, `services/bossd/internal/server/server.go`
