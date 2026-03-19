# PR Display Status Polling System

## Context

The bossd daemon currently has a CI check poller that only monitors sessions in `AwaitingChecks` state. The TUI shows two separate columns: CI (pass/fail/pending) and STATUS (working/idle/stopped). We need a richer, unified PR status display that covers ALL active sessions with PRs, showing meaningful health statuses (Reviewed, Failing, Conflict, Checking, Passing, Merged) with appropriate colors and spinners. The system must be provider-agnostic (for future GitLab support) and webhook-ready (same types/interfaces for both polling and future live updates).

## Design Decisions

- **Single STATUS column** replaces both CI and STATUS columns
- **Entire row strike-through** for merged sessions
- **Separate display poller** from existing lifecycle poller (different concern, different scope, different interval)
- **In-memory tracker** (like existing chat status tracker) -- display status is derived, not persisted
- **Pure `ComputeDisplayStatus` function** in `bossalib/vcs` -- reusable by both poller and future webhooks
- **Hydrate in ListSessions** response -- no new RPC, TUI's existing 2s poll picks it up

## Steps

### Step 1: Define PRDisplayStatus type and computation function

**New file: `lib/bossalib/vcs/display.go`**

- `PRDisplayStatus` enum: Idle, Checking, Failing, Conflict, Reviewed, Passing, Merged, Closed
- `PRDisplayInfo` struct: `{ Status PRDisplayStatus, HasFailures bool }`
- `ComputeDisplayStatus(pr *PRStatus, checks []CheckResult, reviews []ReviewComment) PRDisplayInfo`
  - Priority: Merged > Closed > Conflict > Failing > Checking (if checks running) > Reviewed (changes_requested) > Passing (all green, mergeable, no outstanding reviews) > Idle
  - `HasFailures` = true when some checks failed but others still running (for Checking + error color)

**New file: `lib/bossalib/vcs/display_test.go`** -- table-driven tests for all status combinations.

Reuses existing types from `lib/bossalib/vcs/types.go`: `PRStatus`, `CheckResult`, `ReviewComment`, `PRState`, `CheckStatus`, `CheckConclusion`, `ReviewState`.

### Step 2: Add PRDisplayStatus to protobuf

**Modify: `proto/bossanova/v1/models.proto`**

Add enum:

```protobuf
enum PRDisplayStatus {
  PR_DISPLAY_STATUS_UNSPECIFIED = 0;
  PR_DISPLAY_STATUS_IDLE = 1;
  PR_DISPLAY_STATUS_CHECKING = 2;
  PR_DISPLAY_STATUS_FAILING = 3;
  PR_DISPLAY_STATUS_CONFLICT = 4;
  PR_DISPLAY_STATUS_REVIEWED = 5;
  PR_DISPLAY_STATUS_COMPLETE = 6;
  PR_DISPLAY_STATUS_MERGED = 7;
  PR_DISPLAY_STATUS_CLOSED = 8;
}
```

Add fields to `Session` message:

```protobuf
  PRDisplayStatus pr_display_status = 20;
  bool pr_display_has_failures = 21;
```

Run `buf generate` to regenerate Go code.

### Step 3: Create in-memory PR display tracker

**New file: `services/bossd/internal/status/pr_tracker.go`**

Follows the same pattern as `services/bossd/internal/status/tracker.go`:

- `PRDisplayEntry { Status vcs.PRDisplayStatus, HasFailures bool, UpdatedAt time.Time }`
- `PRTracker` with `sync.RWMutex` and `map[string]*PRDisplayEntry` (session ID -> entry)
- Methods: `Set(sessionID, vcs.PRDisplayInfo)`, `Get(sessionID) *PRDisplayEntry`, `GetBatch([]string) map[string]*PRDisplayEntry`, `Remove(sessionID)`

### Step 4: Create the display poller

**New file: `services/bossd/internal/session/display_poller.go`**

- `DefaultDisplayPollInterval = 30 * time.Second`
- `DisplayPoller` struct with: `sessions db.SessionStore`, `repos db.RepoStore`, `provider vcs.Provider`, `tracker *status.PRTracker`, `interval time.Duration`, `logger zerolog.Logger`
- `Run(ctx context.Context)` -- polls immediately on start (for initial state), then on ticker. Runs in goroutine via `safego.Go`.
- `poll(ctx)` -- iterates ALL active sessions across all repos; skips sessions without PRs
- `pollSession(ctx, repoPath, sessionID, prNumber)` -- calls `provider.GetPRStatus`, `provider.GetCheckResults`, `provider.GetReviewComments`; passes results to `vcs.ComputeDisplayStatus`; calls `tracker.Set`
- Graceful degradation: if GetCheckResults or GetReviewComments fails, continues with nil (ComputeDisplayStatus handles nil slices)

### Step 5: Add PollIntervalSeconds to config

**Modify: `lib/bossalib/config/config.go`**

Add field to `Settings`:

```go
PollIntervalSeconds int `json:"poll_interval_seconds,omitempty"`
```

Add helper:

```go
func (s Settings) DisplayPollInterval() time.Duration {
    if s.PollIntervalSeconds > 0 {
        return time.Duration(s.PollIntervalSeconds) * time.Second
    }
    return 30 * time.Second
}
```

### Step 6: Wire display poller into daemon

**Modify: `services/bossd/cmd/main.go`**

After chat status tracker creation:

```go
prDisplayTracker := status.NewPRTracker()
settings, _ := config.Load()
displayPoller := session.NewDisplayPoller(
    sessions, repos, ghProvider, prDisplayTracker,
    settings.DisplayPollInterval(), log.Logger,
)
displayPoller.Run(pollerCtx)
```

Add `PRDisplay: prDisplayTracker` to `server.Config`.

**Modify: `services/bossd/internal/server/server.go`**

- Add `prDisplay *status.PRTracker` to `Server` struct
- Add `PRDisplay *status.PRTracker` to `Config` struct
- Wire in `New()` constructor

### Step 7: Hydrate pr_display_status in ListSessions

**Modify: `services/bossd/internal/server/server.go` -- `ListSessions` method**

After building `pbSessions`, before returning:

```go
if s.prDisplay != nil {
    sessionIDs := make([]string, len(sessions))
    for i, sess := range sessions { sessionIDs[i] = sess.ID }
    entries := s.prDisplay.GetBatch(sessionIDs)
    for i, sess := range sessions {
        if e, ok := entries[sess.ID]; ok {
            pbSessions[i].PrDisplayStatus = pb.PRDisplayStatus(e.Status)
            pbSessions[i].PrDisplayHasFailures = e.HasFailures
        }
    }
}
```

### Step 8: Update TUI display

**Modify: `services/boss/internal/views/home.go`**

- Remove the `CI` column from the table
- Columns become: cursor | REPO | BRANCH | PR | STATUS
- In `buildTableRows`: replace `checksLabelAndColor` + `renderStatus` with `renderPRDisplayStatus`
- For merged sessions: apply `Strikethrough(true)` to ALL cells in the row
- Remove `checksLabelAndColor` and `ChecksLabel` functions (no longer needed)

**Modify: `services/boss/internal/views/status.go`**

Replace `renderStatus` with `renderPRDisplayStatus(sess *pb.Session, claudeStatus string, sp spinner.Model) string`:

- If `claudeStatus == StatusWorking` -> green spinner + "working" (overrides all)
- `PR_DISPLAY_STATUS_MERGED` -> info color + "merged" (row already strike-through)
- `PR_DISPLAY_STATUS_CLOSED` -> muted color + "closed"
- `PR_DISPLAY_STATUS_COMPLETE` -> success color + "✓ passing"
- `PR_DISPLAY_STATUS_FAILING` -> danger color + "failing"
- `PR_DISPLAY_STATUS_CONFLICT` -> danger color + "conflict"
- `PR_DISPLAY_STATUS_REVIEWED` -> info color + "reviewed"
- `PR_DISPLAY_STATUS_CHECKING` -> info color (or danger if HasFailures) + spinner + "checking"
- Default/Idle -> warning "idle" if Claude idle, muted "stopped" otherwise

### Step 9: Add poll interval to Settings TUI

**Modify: `services/boss/internal/views/settings.go`**

Add a third row (`settingsRowPollInterval = 2`, `settingsRowCount = 3`) with a text input for poll interval seconds. Follow the worktree dir editing pattern. Validate as integer > 0.

## Files to create

- `lib/bossalib/vcs/display.go`
- `lib/bossalib/vcs/display_test.go`
- `services/bossd/internal/status/pr_tracker.go`
- `services/bossd/internal/session/display_poller.go`

## Files to modify

- `proto/bossanova/v1/models.proto` (add PRDisplayStatus enum + Session fields)
- `lib/bossalib/config/config.go` (add PollIntervalSeconds)
- `services/bossd/cmd/main.go` (wire display poller + tracker)
- `services/bossd/internal/server/server.go` (add PRTracker to Config, hydrate in ListSessions)
- `services/boss/internal/views/home.go` (remove CI column, add strikethrough for merged rows)
- `services/boss/internal/views/status.go` (replace renderStatus with renderPRDisplayStatus)
- `services/boss/internal/views/settings.go` (add poll interval setting row)

## Verification

1. **Unit tests**: Run `go test ./lib/bossalib/vcs/...` for ComputeDisplayStatus tests
2. **Build**: `go build ./services/bossd/...` and `go build ./services/boss/...`
3. **Proto gen**: `buf generate` succeeds without errors
4. **Manual test**: Start bossd, create a session with a PR, observe STATUS column updates every 30s
5. **Settings**: Open settings TUI, verify poll interval is editable and persists to settings.json
6. **Existing tests**: `go test ./...` passes -- existing lifecycle poller/dispatcher unchanged
