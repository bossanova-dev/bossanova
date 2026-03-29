---
name: tui-qa
description: Systematic QA for the Boss TUI. Writes Go integration tests using the PTY-based TUI driver, runs them in a fix loop, and produces a structured health report.
---

# TUI QA: Automated Quality Assurance for Boss TUI

Drive the Boss TUI through a PTY + VT emulator, write Go integration tests, fix failures, and produce a health score. This skill is for agents performing QA on the bubbletea v2 terminal UI without human intervention.

---

## Prerequisites

Before starting, verify:

1. **Clean git state** — `git status` shows no uncommitted changes (or only expected WIP)
2. **Boss binary builds** — `cd services/boss && go build ./cmd` succeeds
3. **Existing tests pass** — `go test -v -run TestTUI ./internal/tuitest/ -timeout 120s` from `services/boss/`
4. **Skills copied** — `make copy-skills` has been run (required for boss binary embed)

If any prerequisite fails, fix it before proceeding.

---

## Workflow Phases

### Phase 1: Initialize

```bash
# Check git state
git status

# Build boss binary
cd services/boss && go build ./cmd

# Run existing tests as baseline
go test -v -run TestTUI ./internal/tuitest/ -timeout 120s
```

Record which tests pass. This is your baseline — no regressions allowed.

If running in **diff-aware mode**, check what changed:

```bash
git diff main --name-only
```

Focus testing on views/interactions affected by the diff.

### Phase 2: Orient

Read the TUI architecture to understand what to test:

1. **Views**: `services/boss/internal/views/` — every `*.go` file is a view
2. **Existing tests**: `services/boss/internal/tuitest/integration_test.go`
3. **Driver API**: `services/boss/internal/tuidriver/driver.go`
4. **Test harness**: `services/boss/internal/tuitest/harness.go`, `mock_daemon.go`

Build a mental map of views, keybindings, and navigation paths. See the [View Map](#view-map--navigation-reference) below.

### Phase 3: Plan Tests

Based on the orient phase, plan which tests to write. Choose a tier:

| Tier | Scope | When to Use |
|------|-------|-------------|
| **Quick** | Critical paths only: home view loads, navigation between views, quit | Smoke test after small change |
| **Standard** | + edge cases, empty states, confirmations, archive/restore/delete | Default for most QA runs |
| **Exhaustive** | + every view interaction, all keybindings, error states, data display | Major refactor or pre-release |

List planned tests before writing any code. Each test should have:
- Name: `TestTUI_ViewName_Behavior`
- What it verifies
- Which keys/interactions it exercises
- What text to assert on

### Phase 4: Write Tests

Write tests in `services/boss/internal/tuitest/integration_test.go` (or a new `*_test.go` file in the same package). Follow the patterns in [Test Patterns](#test-patterns) below.

Every test file in `tuitest/` must use the shared `TestMain`:

```go
// Already exists in main_test.go — do NOT duplicate
func TestMain(m *testing.M) {
    cleanup := tuitest.BuildBoss()
    code := m.Run()
    cleanup()
    os.Exit(code)
}
```

### Phase 5: Run & Fix Loop

```
┌─────────────────────────┐
│  go test -v -run TestTUI │
│  ./internal/tuitest/     │
│  -timeout 120s           │
└──────────┬──────────────┘
           │
     ┌─────▼─────┐
     │ All pass?  │──Yes──▶ Done
     └─────┬─────┘
           │ No
     ┌─────▼──────────┐
     │ Diagnose failure│
     │ Is it a TUI bug │
     │ or a test bug?  │
     └─────┬──────────┘
           │
     ┌─────▼─────┐
     │  Fix it    │
     │  One commit │
     │  per fix   │
     └─────┬─────┘
           │
     ┌─────▼──────────────┐
     │ Run full suite      │
     │ Check no regressions│
     └─────┬──────────────┘
           │
           └──────▶ Loop (max 3 retries per issue, 50 total fixes)
```

**Fix commit format**: `fix(boss/tui): description of what was fixed`

After each fix:
1. Run `make format` (from repo root)
2. Run `make test` to check for regressions across the full test suite
3. If a fix causes a regression, revert it and try a different approach

### Phase 6: Report

Produce a structured report:

```markdown
## TUI QA Report

**Date**: YYYY-MM-DD
**Tier**: Quick | Standard | Exhaustive
**Baseline**: X tests passing
**Final**: Y tests passing (Z new)

### Health Score: XX/100

| Category | Score | Max | Notes |
|----------|-------|-----|-------|
| Navigation | /20 | 20 | |
| Data Display | /20 | 20 | |
| Interactions | /20 | 20 | |
| Confirmations | /15 | 15 | |
| Empty States | /10 | 10 | |
| Error Handling | /10 | 10 | |
| Exit Behavior | /5 | 5 | |

### Issues Found
- [ ] ISSUE-1: Description (severity: critical|major|minor)

### Fixes Applied
- COMMIT-HASH: Description

### Tests Added
- TestTUI_ViewName_Behavior: What it verifies

### Known Gaps
- What wasn't tested and why
```

---

## TUI Driver API Reference

### `tuidriver.Driver`

Create via `tuidriver.New(opts)`. The test harness creates one for you.

```go
type Options struct {
    Command string   // path to boss binary
    Args    []string // CLI arguments
    Env     []string // environment variables (nil = inherit os.Environ())
    Dir     string   // working directory
    Width   int      // terminal width (default: 120)
    Height  int      // terminal height (default: 30)
}
```

#### Screen Reading

| Method | Signature | Description |
|--------|-----------|-------------|
| `Screen` | `() string` | Current terminal screen as plain text (thread-safe) |
| `ScreenContains` | `(text string) bool` | True if screen contains substring |

#### Input

| Method | Signature | Description |
|--------|-----------|-------------|
| `SendKey` | `(b byte) error` | Send single byte (e.g., `'j'`, `'q'`, `'a'`) |
| `SendString` | `(s string) error` | Send a string |
| `SendEnter` | `() error` | Send carriage return (`\r`) |
| `SendEscape` | `() error` | Send escape character (`0x1b`) |
| `SendCtrlC` | `() error` | Send ETX / Ctrl+C (`0x03`) |

#### Waiting

| Method | Signature | Description |
|--------|-----------|-------------|
| `WaitFor` | `(timeout time.Duration, pred func(string) bool) error` | Poll screen every 50ms until predicate is true |
| `WaitForText` | `(timeout time.Duration, text string) error` | Wait until screen contains text |
| `WaitForNoText` | `(timeout time.Duration, text string) error` | Wait until screen no longer contains text |

All wait methods return an error with the last screen content on timeout.

#### Lifecycle

| Method | Signature | Description |
|--------|-----------|-------------|
| `Done` | `() <-chan struct{}` | Channel closed when process exits |
| `Close` | `() error` | Send Ctrl+C, wait 3s, kill, cleanup |

### `tuitest.Harness`

```go
type Harness struct {
    Driver *tuidriver.Driver
    Daemon *MockDaemon
}
```

#### Setup

| Function | Description |
|----------|-------------|
| `BuildBoss() (cleanup func())` | Compile boss binary. Call from `TestMain` only. |
| `New(t *testing.T, opts ...Option) *Harness` | Create harness with mock daemon + driver. Auto-cleanup on test end. |

#### Options

| Option | Description |
|--------|-------------|
| `WithRepos(repos ...*pb.Repo)` | Seed mock daemon with repos |
| `WithSessions(sessions ...*pb.Session)` | Seed mock daemon with sessions |

### `MockDaemon`

#### Methods

| Method | Description |
|--------|-------------|
| `SocketPath() string` | Unix socket path |
| `AddRepo(repo *pb.Repo)` | Add repo at runtime |
| `AddSession(sess *pb.Session)` | Add session at runtime |
| `Sessions() []*pb.Session` | Get copy of all sessions (for verification) |

#### Implemented RPCs (12)

These return real responses with in-memory data:

| RPC | Behavior |
|-----|----------|
| `ListRepos` | Returns all repos |
| `ListSessions` | Filters by `IncludeArchived` flag |
| `GetSession` | Returns by ID (or `NotFound`) |
| `ArchiveSession` | Sets `ArchivedAt` to now |
| `ResurrectSession` | Clears `ArchivedAt` |
| `RemoveSession` | Deletes from memory |
| `EmptyTrash` | Removes all archived, returns `DeletedCount` |
| `ListChats` | Returns empty response |
| `ReportChatStatus` | Returns empty response |
| `GetChatStatuses` | Returns empty response |
| `GetSessionStatuses` | Returns empty response |
| `ListRepoPRs` | Returns empty response |

#### Unimplemented RPCs

All return `connect.CodeUnimplemented`: CreateSession, AttachSession, StopSession, PauseSession, ResumeSession, RetrySession, CloseSession, RegisterRepo, CloneAndRegisterRepo, RemoveRepo, UpdateRepo, ValidateRepoPath, ResolveContext, RecordChat, UpdateChatTitle, DeleteChat, DeliverVCSEvent, StartAutopilot, PauseAutopilot, ResumeAutopilot, CancelAutopilot, GetAutopilotStatus, ListAutopilotWorkflows, StreamAutopilotOutput.

---

## View Map & Navigation Reference

```
                         ┌──────────────┐
                    ┌────│   ViewHome   │────┐
                    │    │  (startup)   │    │
                    │    └──┬──┬──┬──┬──┘    │
                    │       │  │  │  │       │
               [n]  │  [r]  │  │  │  │ [t]   │  [s]
                    │       │  │  │  │       │
         ┌──────────▼┐  ┌──▼──┐│  │┌▼────┐ ┌▼────────┐
         │NewSession  │  │Repo ││  ││Trash│ │Settings  │
         │(wizard)    │  │List ││  ││     │ │          │
         └────────────┘  └──┬──┘│  │└─────┘ └──────────┘
                            │   │  │
                       [a]  │   │  │  [p]
                            │   │  │
                      ┌─────▼┐  │  ├────────┐
                      │Repo  │  │  │        │
                      │Add   │  │  │  ┌─────▼──────┐
                      └──────┘  │  │  │ Autopilot  │
                         [s/enter] │  └────────────┘
                            │   │
                      ┌─────▼──┐│
                      │Repo    ││
                      │Settings││
                      └────────┘│
                                │
                          [enter/h]
                                │
                         ┌──────▼──────┐
                         │ ChatPicker  │
                         └──────┬──────┘
                           [n/enter]
                                │
                         ┌──────▼──────┐
                         │  Attach     │
                         │ (Claude PTY)│
                         └─────────────┘
```

All views return to their parent via `esc` or `q`.

### View Details

#### ViewHome
- **Navigate to**: `n`=NewSession, `r`=RepoList, `s`=Settings, `t`=Trash, `p`=Autopilot, `h`/`enter`=ChatPicker/Attach
- **Actions**: `a`=archive selected session (confirmation), `q`=quit
- **Navigation**: `j`/`k`/`up`/`down` = move cursor
- **Assert text**: `"Bossanova"`, `"No active sessions."`, `"[n]ew session"`
- **Session data**: titles appear directly (e.g., `"Add dark mode"`)

#### ViewTrash
- **Navigate to**: from Home via `t`
- **Actions**: `r`=restore, `d`=delete (confirmation), `a`=empty trash (confirmation)
- **Assert text**: `"Archived Sessions"`, `"Trash is empty."`
- **Confirmations**: `"Permanently delete %q?"`, `"Permanently delete all %d archived sessions?"`

#### ViewSettings
- **Navigate to**: from Home via `s`
- **Actions**: `enter`/`space`=toggle checkbox or edit field
- **Assert text**: `"Settings"`, `"Enable Claude --dangerously-skip-permissions"`, `"Worktree base directory"`, `"Poll interval"`

#### ViewRepoList
- **Navigate to**: from Home via `r`
- **Actions**: `a`=add repo, `d`=remove (confirmation), `s`/`enter`=repo settings
- **Assert text**: `"No repositories registered."`, `"NAME"`, `"PATH"`
- **Confirmations**: `"Remove %q?"`

#### ViewRepoAdd
- **Navigate to**: from RepoList via `a`
- **Phases**: source select → input form → details form → done
- **Assert text**: `"Add Repository"`, `"Open project"`, `"Clone from URL"`, `"Repository registered!"`

#### ViewRepoSettings
- **Navigate to**: from RepoList via `s`/`enter`
- **Assert text**: `"Name:"`, `"Setup command:"`, `"Merge strategy:"`, `"Auto-merge PRs"`

#### ViewNewSession
- **Navigate to**: from Home via `n`
- **Phases**: loading → repo select → type select → PR select/form → creating → done
- **Assert text**: `"Select a repository"`, `"Create a new PR"`, `"Quick chat"`, `"Creating a new session..."`

#### ViewChatPicker
- **Navigate to**: from Home via `h` or `enter`
- **Actions**: `n`=new chat, `d`=delete chat (confirmation), `enter`=resume
- **Assert text**: `"Loading chats for"`, `"New chat"`

#### ViewAutopilot
- **Navigate to**: from Home via `p`
- **Actions**: `p`=pause, `r`=resume, `c`=cancel (confirmation)
- **Assert text**: `"Autopilot Workflows"`, `"No workflows."`

#### ViewAttach
- **Navigate to**: from NewSession (success), ChatPicker (new/resume), Home (auto-enter)
- **Detach**: `Ctrl+X` / `Ctrl+]`
- **Assert text**: `"Launching Claude Code for"`, `"Press Ctrl+X to detach"`

---

## Test Patterns

### Standard Wait Timeout

```go
const waitTimeout = 10 * time.Second
```

Use this for all `WaitFor`/`WaitForText`/`WaitForNoText` calls.

### Test Data Helpers

```go
func testRepos() []*pb.Repo {
    return []*pb.Repo{{
        Id:                "repo-1",
        DisplayName:       "my-app",
        LocalPath:         "/tmp/my-app",
        DefaultBaseBranch: "main",
    }}
}

func testSessions() []*pb.Session {
    return []*pb.Session{
        {
            Id:       "sess-aaa-111",
            Title:    "Add dark mode",
            RepoId:   "repo-1",
            Branch:   "boss/add-dark-mode",
            State:    pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
        },
        {
            Id:       "sess-bbb-222",
            Title:    "Fix login bug",
            RepoId:   "repo-1",
            Branch:   "boss/fix-login-bug",
            State:    pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
        },
    }
}
```

### Basic Test Structure

```go
func TestTUI_ViewName_Behavior(t *testing.T) {
    h := tuitest.New(t,
        tuitest.WithRepos(testRepos()...),
        tuitest.WithSessions(testSessions()...),
    )

    // Wait for the TUI to render
    if err := h.Driver.WaitForText(waitTimeout, "Bossanova"); err != nil {
        t.Fatal(err)
    }

    // Interact
    if err := h.Driver.SendKey('t'); err != nil {
        t.Fatal(err)
    }

    // Assert
    if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
        t.Fatal(err)
    }
}
```

### Navigation Pattern

```go
// Navigate to a view
h.Driver.SendKey('t')  // go to Trash
h.Driver.WaitForText(waitTimeout, "Archived Sessions")

// Navigate back
h.Driver.SendEscape()
h.Driver.WaitForText(waitTimeout, "Bossanova")
```

### Confirmation Dialog Pattern

```go
// Trigger destructive action
h.Driver.SendKey('a')
h.Driver.WaitForText(waitTimeout, "Archive")

// Confirm
h.Driver.SendKey('y')
h.Driver.WaitForNoText(waitTimeout, "Archive")

// --- OR cancel ---
h.Driver.SendKey('n')
// verify original state preserved
```

### Daemon State Verification

```go
// After a TUI action, verify the daemon state changed
for _, s := range h.Daemon.Sessions() {
    if s.Id == "sess-aaa-111" {
        if s.ArchivedAt == nil {
            t.Fatal("expected session to be archived")
        }
    }
}
```

### Complex Wait Predicate

```go
err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
    return strings.Contains(screen, "Trash is empty") ||
        !strings.Contains(screen, "Add dark mode")
})
if err != nil {
    t.Fatal(err)
}
```

### Process Exit Verification

```go
h.Driver.SendKey('q')
select {
case <-h.Driver.Done():
    // success — process exited
case <-time.After(5 * time.Second):
    t.Fatal("process did not exit")
}
```

### Empty State Pattern

```go
func TestTUI_ViewName_EmptyState(t *testing.T) {
    // Create harness with NO data
    h := tuitest.New(t,
        tuitest.WithRepos(testRepos()...),
        // no sessions
    )

    if err := h.Driver.WaitForText(waitTimeout, "No active sessions."); err != nil {
        t.Fatal(err)
    }
}
```

### Timing Between Key Presses

Use `time.Sleep` between rapid key presses when the TUI needs time to process:

```go
h.Driver.SendKey('j')
time.Sleep(200 * time.Millisecond)
h.Driver.SendKey('j')
time.Sleep(200 * time.Millisecond)
```

For most interactions, prefer `WaitForText` over `time.Sleep` — it's more reliable and self-documenting.

---

## Health Score Rubric

| Category | Weight | What to Test |
|----------|--------|-------------|
| **Navigation** | 20% | All views reachable from Home. Back navigation (`esc`) works from every view. `j`/`k`/`up`/`down` move cursor in tables. |
| **Data Display** | 20% | Sessions show title, repo, branch, state. Repos show name, path. Trash shows archived date. Tables render without visual corruption. |
| **Interactions** | 20% | Archive, restore, delete, empty trash all modify daemon state correctly. New session wizard flows work. Repo add/remove work. |
| **Confirmations** | 15% | All destructive actions (archive, delete, remove, empty trash, cancel workflow) show confirmation. Cancelling confirmation preserves state. |
| **Empty States** | 10% | `"No active sessions."` on Home with no sessions. `"Trash is empty."` with no archived. `"No repositories registered."` with no repos. `"No workflows."` with no autopilot workflows. |
| **Error Handling** | 10% | Graceful behavior when daemon returns errors. Error text is visible and actionable. |
| **Exit Behavior** | 5% | `q` quits from Home. `Ctrl+C` quits from any view. Process exits cleanly (Done channel closes). |

### Scoring

- **Full points**: Feature works correctly in all tested scenarios
- **Half points**: Feature works but has minor issues (cosmetic, non-blocking)
- **Zero points**: Feature is broken or untested

### Interpreting the Score

| Score | Status |
|-------|--------|
| 90-100 | Ship it |
| 75-89 | Minor issues — fix before release |
| 50-74 | Significant issues — needs work |
| < 50 | Major problems — do not release |

---

## Fix Loop Protocol

1. **One commit per fix** — keep fixes atomic and reviewable
2. **Commit format**: `fix(boss/tui): description` for TUI bugs, `fix(boss/tui-test): description` for test-only fixes
3. **After each fix**:
   - `make format` (from repo root)
   - `go test -v -run TestTUI ./internal/tuitest/ -timeout 120s` (from `services/boss/`)
   - `make test` if the fix touched non-test code
4. **Revert on regression** — if a fix breaks an existing test, `git revert` and try a different approach
5. **Retry limits** — max 3 attempts per issue, max 50 total fixes per QA run
6. **Escalate** — if you can't fix an issue after 3 attempts, log it in the report as a known issue

---

## Anti-Patterns

### Asserting on styled text
The VT emulator's `.String()` returns **plain text without ANSI codes**. Never assert on color codes or lipgloss-styled strings. Assert on the raw text content.

```go
// WRONG — style codes stripped by VT emulator
h.Driver.WaitForText(waitTimeout, "\033[1mBossanova\033[0m")

// CORRECT — plain text
h.Driver.WaitForText(waitTimeout, "Bossanova")
```

### Forgetting to wait for renders
The TUI renders asynchronously. Always `WaitForText` before interacting with a new view.

```go
// WRONG — view might not be rendered yet
h.Driver.SendKey('t')
h.Driver.SendKey('d')  // might hit Home's 'd' handler, not Trash's

// CORRECT
h.Driver.SendKey('t')
h.Driver.WaitForText(waitTimeout, "Archived Sessions")
h.Driver.SendKey('d')
```

### Using exact screen layout for assertions
Screen layout depends on terminal size. Assert on **content presence**, not position.

```go
// WRONG — fragile layout assertion
screen := h.Driver.Screen()
if screen[0:10] != "Bossanova" { ... }

// CORRECT
if !h.Driver.ScreenContains("Bossanova") { ... }
```

### Sleeping instead of waiting
`time.Sleep` is unreliable and slow. Use `WaitForText` or `WaitFor` with a predicate.

```go
// WRONG — slow and flaky
time.Sleep(2 * time.Second)
if !h.Driver.ScreenContains("Archived Sessions") { t.Fatal("...") }

// CORRECT
if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
    t.Fatal(err)
}
```

Exception: brief `time.Sleep(200 * time.Millisecond)` between rapid keypresses in the same view is acceptable when there's no intermediate text to wait for.

### Duplicating TestMain
`TestMain` with `BuildBoss()` must exist **exactly once** per package. It's already in `main_test.go`. Never add another.

### Hardcoding socket paths
The harness manages socket paths automatically. Never set `BOSS_SOCKET` manually in tests.

### Ignoring the Done() channel
When testing quit behavior, always verify the process actually exits via `Done()`. A test that sends `'q'` without checking `Done()` might pass even if quit is broken.

### Testing unimplemented RPCs
CreateSession, AttachSession, and streaming RPCs return `Unimplemented`. Don't write tests that depend on these — the mock daemon can't handle them. Tests for NewSession and Attach flows need the real daemon or an extended mock.

---

## Environment Variables

| Variable | Purpose | Set By |
|----------|---------|--------|
| `BOSS_SOCKET` | Override daemon socket path, skip auto-start | Harness |
| `BOSS_SKIP_SKILLS` | Skip skill prompt on startup (`"1"`) | Harness |
| `TERM` | Terminal type (`"xterm-256color"`) | Harness |

---

## Import Paths

```go
import (
    "testing"
    "time"
    "strings"

    pb "github.com/anthropics/boss/lib/bossalib/gen/boss/v1"
    "github.com/anthropics/boss/services/boss/internal/tuidriver"
    "github.com/anthropics/boss/services/boss/internal/tuitest"
)
```

---

## Running Tests

```bash
# From services/boss/

# Run all TUI tests
go test -v -run TestTUI ./internal/tuitest/ -timeout 120s

# Run a specific test
go test -v -run TestTUI_HomeView_ShowsSessions ./internal/tuitest/ -timeout 120s

# Run with race detector (slower but catches concurrency bugs)
go test -v -race -run TestTUI ./internal/tuitest/ -timeout 180s
```
