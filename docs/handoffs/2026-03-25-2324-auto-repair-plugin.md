# Auto-Repair Plugin Implementation Plan

**Flight ID:** fp-2026-03-25-2324-auto-repair-plugin

## Overview

When a PR's display status goes "red" (Failing, Conflict, Rejected), there's no automated recovery. The existing `FixLoop` in the free daemon has never successfully fixed anything and is being **removed as a premium feature**. This plan builds a **paid plugin** (`bossd-plugin-repair`) that watches for red statuses and triggers repair via a **free skill** (`boss-repair`). The skill does a full resolve cycle: fix the issue, force-push, and call `boss-finalize` to mark the PR ready for review.

## Architecture

```
DisplayPoller →2s→ PRTracker.Set() → onChange callback
  → pluginHost.NotifyStatusChange() → WorkflowService.NotifyStatusChange RPC
    → repair plugin detects red status → CreateAttempt(/boss-repair)
      → Claude fixes + force-pushes → FireSessionEvent(FixComplete) → AwaitingChecks
```

**Key decisions from eng review:**

1. **FixLoop removed** from free CLI — pass `nil` to Dispatcher (already supported)
2. **Daemon pushes** status changes to plugins via `NotifyStatusChange` RPC on `WorkflowService`
3. **`FireSessionEvent` RPC** added to HostService — generic, lets any plugin fire state machine events
4. **PRTracker gets `onChange` callback** — triggers when display status transitions
5. **Shared hostclient package** extracted from plugin boilerplate → `lib/bossalib/plugin/hostclient/`
6. **Skill source** is `.claude/skills/boss-repair/SKILL.md`, copied to `lib/bossalib/skilldata/skills/` by `make copy-skills`

## Affected Areas

### Modified

- `proto/bossanova/v1/host_service.proto` — Add `ListSessions`, `GetReviewComments`, `FireSessionEvent` RPCs
- `proto/bossanova/v1/plugin.proto` — Add `NotifyStatusChange` RPC to `WorkflowService`
- `services/bossd/internal/plugin/host_service.go` — Implement new RPCs + `SetSessionDeps`
- `services/bossd/internal/plugin/host.go` — Wire `SetSessionDeps`, add `NotifyStatusChange` broadcast
- `services/bossd/internal/plugin/grpc_plugins.go` — Add `NotifyStatusChange` to `WorkflowService` interface + client
- `services/bossd/internal/status/pr_tracker.go` — Add `onChange` callback
- `services/bossd/cmd/main.go` — Remove FixLoop, wire `SetSessionDeps`, register onChange, auto-start repair
- `plugins/bossd-plugin-autopilot/plugin.go` — Add no-op `NotifyStatusChange` to handler interface + descriptor
- `plugins/bossd-plugin-autopilot/server.go` — Implement no-op `NotifyStatusChange`
- `Makefile` — Add `build-repair` target, add to `plugins` target
- `TODOS.md` — Update "Expand HostService" TODO as partially addressed

### New

- `lib/bossalib/plugin/hostclient/` — Shared host client (extracted from autopilot)
- `plugins/bossd-plugin-repair/` — New plugin (main.go, plugin.go, server.go, go.mod)
- `.claude/skills/boss-repair/SKILL.md` — New skill

### Deleted

- `services/bossd/internal/session/fixloop.go` — Removed (premium feature moving to plugin)
- `services/bossd/internal/session/fixloop_test.go` — Removed with fixloop

## Design References

- Autopilot plugin pattern: `plugins/bossd-plugin-autopilot/` (main.go, plugin.go, host.go, server.go)
- HostService descriptors: `services/bossd/internal/plugin/host_service.go:51-105`
- WorkflowService descriptors: `plugins/bossd-plugin-autopilot/plugin.go:42-84`
- Host-side gRPC clients: `services/bossd/internal/plugin/grpc_plugins.go:213-273`
- Plugin-side host client: `plugins/bossd-plugin-autopilot/host.go` (entire file)
- PRTracker: `services/bossd/internal/status/pr_tracker.go`
- DisplayPoller: `services/bossd/internal/session/display_poller.go`
- Daemon wiring: `services/bossd/cmd/main.go:92-127`
- Skill source pattern: `.claude/skills/boss-finalize/SKILL.md`
- Skill embed: `lib/bossalib/skilldata/embed.go` (`//go:embed skills`)
- Config skill map: `lib/bossalib/config/config.go:40-46`
- Attempt creation from plugin: `plugins/bossd-plugin-autopilot/server.go:497-509`

---

## Flight Leg 1: Proto Changes + HostService Expansion

### Tasks

**Proto: host_service.proto**

- [ ] Add `ListSessions` RPC — Request: empty, Response: `repeated Session sessions`
- [ ] Add `GetReviewComments` RPC — Request: `repo_origin_url + pr_number`, Response: `repeated ReviewComment`
- [ ] Add `FireSessionEvent` RPC — Request: `session_id + SessionEvent`, Response: `new_state`
- [ ] Add proto messages: `ListSessionsRequest`, `ListSessionsResponse`, `GetReviewCommentsRequest`, `GetReviewCommentsResponse`, `FireSessionEventRequest`, `FireSessionEventResponse`

**Proto: plugin.proto**

- [ ] Add `NotifyStatusChange` RPC to `WorkflowService` — Request: `session_id + PRDisplayStatus + bool has_failures`, Response: empty

**Generate**

- [ ] Run `buf lint && buf generate`

**HostService implementation** (`services/bossd/internal/plugin/host_service.go`)

- [ ] Add fields: `repoStore db.RepoStore`, `sessionStore db.SessionStore`, `prTracker *status.PRTracker`
- [ ] Add `SetSessionDeps(repos db.RepoStore, sessions db.SessionStore, tracker *status.PRTracker)` method
- [ ] Implement `ListSessions` — iterate repos → list active sessions → hydrate with PRTracker status
- [ ] Implement `GetReviewComments` — delegate to `s.provider.GetReviewComments()`
- [ ] Implement `FireSessionEvent` — load session, create state machine, fire event, update DB
- [ ] Register all 3 new handlers in `hostServiceDesc.Methods` and `hostServiceHandler` interface

**Host-side WorkflowService client** (`services/bossd/internal/plugin/grpc_plugins.go`)

- [ ] Add `NotifyStatusChange` to `WorkflowService` interface
- [ ] Add `NotifyStatusChange` to `workflowServiceGRPCClient` (calls `/bossanova.v1.WorkflowService/NotifyStatusChange`)

**PRTracker onChange** (`services/bossd/internal/status/pr_tracker.go`)

- [ ] Add `OnChange func(sessionID string, oldStatus, newStatus vcs.PRDisplayStatus)` field
- [ ] In `Set()`, detect status transition and call `OnChange` if status changed

**Plugin host broadcast** (`services/bossd/internal/plugin/host.go`)

- [ ] Add `SetSessionDeps` that passes through to `HostServiceServer.SetSessionDeps`
- [ ] Add `NotifyStatusChange(ctx, sessionID, status, hasFailures)` method — iterates `GetWorkflowServices()`, calls `NotifyStatusChange` on each

**Daemon wiring** (`services/bossd/cmd/main.go`)

- [ ] Remove `fixLoop` creation and pass `nil` to `NewDispatcher`
- [ ] Call `pluginHost.SetSessionDeps(repos, sessions, prDisplayTracker)` after `SetWorkflowDeps`
- [ ] Register PRTracker onChange callback that calls `pluginHost.NotifyStatusChange()`

### Post-Flight Checks for Flight Leg 1

- [ ] **Proto:** `buf lint && buf generate` — no errors
- [ ] **Compiles:** `cd services/bossd && go build ./...`
- [ ] **Plugin tests:** `cd services/bossd && go test ./internal/plugin/... -v` — passes
- [ ] **Status tests:** `cd services/bossd && go test ./internal/status/... -v` — passes
- [ ] **Session tests:** `cd services/bossd && go test ./internal/session/... -v` — passes (dispatcher with nil fixloop)

### [HANDOFF] Review Flight Leg 1

Human reviews: Proto additions are backwards-compatible. HostService exposes session data to plugins. FixLoop cleanly removed.

---

## Flight Leg 2: Extract Shared Host Client + Autopilot Update

### Tasks

**Shared hostclient** (`lib/bossalib/plugin/hostclient/`)

- [ ] Create `hostclient.go` — extract from `plugins/bossd-plugin-autopilot/host.go`:
  - `Client` interface (was `hostClient`) — all HostService RPCs plugins can call
  - `DirectClient` struct (was `hostServiceClient`) — wraps `grpc.ClientConn`
  - `EagerClient` struct (was `eagerHostServiceClient`) — broker.Dial(1) in background
  - `AttemptOutputStream` interface + `attemptOutputStream` struct
  - `NewDirectClient`, `NewEagerClient` constructors
  - All method implementations (CreateWorkflow, UpdateWorkflow, etc.)
  - Add new methods: `ListSessions`, `GetReviewComments`, `GetCheckResults`, `GetPRStatus`, `FireSessionEvent`

**Update autopilot to use shared package**

- [ ] `plugins/bossd-plugin-autopilot/host.go` — replace with thin import of `hostclient` package
- [ ] `plugins/bossd-plugin-autopilot/plugin.go` — add `NotifyStatusChange` to `workflowServiceDesc.Methods` and `workflowServiceHandler`
- [ ] `plugins/bossd-plugin-autopilot/server.go` — add no-op `NotifyStatusChange` on `orchestrator`
- [ ] `plugins/bossd-plugin-autopilot/go.mod` — add dependency on `bossalib/plugin/hostclient`
- [ ] Verify autopilot still compiles and tests pass

### Post-Flight Checks for Flight Leg 2

- [ ] **Shared lib:** `cd lib/bossalib && go build ./plugin/hostclient/...` — compiles
- [ ] **Autopilot build:** `cd plugins/bossd-plugin-autopilot && go build .` — compiles
- [ ] **Autopilot tests:** `cd plugins/bossd-plugin-autopilot && go test ./...` — passes

### [HANDOFF] Review Flight Leg 2

Human reviews: Shared hostclient is a clean extraction. Autopilot uses it without behavior changes.

---

## Flight Leg 3: Repair Plugin Binary

### Tasks

**`plugins/bossd-plugin-repair/main.go`**

- [ ] Entry point: `goplugin.Serve` with `sharedplugin.PluginTypeWorkflow` (same as autopilot)

**`plugins/bossd-plugin-repair/plugin.go`**

- [ ] `repairPlugin` struct implementing `goplugin.GRPCPlugin`
- [ ] `GRPCServer`: create `hostclient.NewEagerClient(broker)` + `repairMonitor`, register service
- [ ] `workflowServiceDesc` gRPC descriptor: GetInfo, StartWorkflow, PauseWorkflow, ResumeWorkflow, CancelWorkflow, GetWorkflowStatus, NotifyStatusChange
- [ ] `workflowServiceHandler` interface (all 7 methods)
- [ ] Handler functions for all RPCs (follow autopilot pattern)

**`plugins/bossd-plugin-repair/server.go`** — the repair monitor

- [ ] `repairMonitor` struct: host `hostclient.Client`, logger, mu sync.Mutex, repairing map[string]bool, cooldowns map[string]time.Time, cancel context.CancelFunc
- [ ] `GetInfo` → name:"repair", capabilities:["workflow","repair"]
- [ ] `StartWorkflow` → creates workflow record, stores cancel func
- [ ] `NotifyStatusChange` → main entry point:
  - Skip if status is not Failing(3)/Conflict(4)/Rejected(5)
  - Skip if already repairing this session
  - Skip if cooldown not expired (5 min between retries per session)
  - Call `go m.repairSession(ctx, sessionID, status, hasFailures)`
- [ ] `repairSession`:
  - Set repairing[sessionID] = true, defer cleanup
  - Build prompt: `/boss-repair` (skill assesses state itself)
  - Call `host.CreateAttempt(ctx, &CreateAttemptRequest{WorkflowId, SkillName: "boss-repair", Input: prompt})`
  - Poll `host.GetAttemptStatus()` until complete (follow autopilot `pollAttempt` pattern)
  - On success: call `host.FireSessionEvent(ctx, sessionID, FixComplete)`
  - On failure: set cooldown, log error
- [ ] PauseWorkflow/ResumeWorkflow/CancelWorkflow: control via workflow status

**`plugins/bossd-plugin-repair/go.mod`**

- [ ] Dependencies: go-plugin, zerolog, bossalib/gen/bossanova/v1, bossalib/plugin, bossalib/plugin/hostclient

**Makefile**

- [ ] Add `$(BIN_DIR)/bossd-plugin-repair` target
- [ ] Add to `plugins:` target list
- [ ] Add `test-repair` and `lint-repair` targets

### Post-Flight Checks for Flight Leg 3

- [ ] **Compiles:** `cd plugins/bossd-plugin-repair && go build .` — builds cleanly
- [ ] **Interface check:** `var _ workflowServiceHandler = (*repairMonitor)(nil)` — compiles
- [ ] **Tests:** `cd plugins/bossd-plugin-repair && go test ./...` — passes

### [HANDOFF] Review Flight Leg 3

Human reviews: Plugin structure mirrors autopilot. Monitor has proper guards (cooldown, concurrency).

---

## Flight Leg 4: boss-repair Skill

### Tasks

**`.claude/skills/boss-repair/SKILL.md`**

- [ ] Create skill following `boss-finalize` pattern (YAML frontmatter + markdown body)
- [ ] Frontmatter: `name: boss-repair`, `description: Automated PR repair — fixes conflicts, failing checks, and review feedback`
- [ ] Instructions — the skill handles ALL three scenarios in one invocation:

  **Step 1: Assess state**

  ```
  gh pr view --json state,mergeable,reviewDecision,statusCheckRollup
  gh pr checks
  git status
  ```

  **Step 2: Fix based on assessment**
  - If merge conflict: `git fetch origin <base>` → `git rebase origin/<base>` → resolve → `git rebase --continue`
  - If failing checks: `gh pr checks` to ID failures → `gh run view <id> --log-failed` → fix root cause → `make test`
  - If review rejection: `gh pr view --json reviews,comments` → address each comment

  **Step 3: Quality gates** — `make`, `make lint`, `make test`

  **Step 4: Commit + force-push** — stage, commit with descriptive message, `git push --force-with-lease`

  **Step 5: Finalize** — run `/boss-finalize` to squash commits, add PR numbers, mark ready for review, verify checks

**Config integration** (`lib/bossalib/config/config.go`)

- [ ] Add `"repair": "boss-repair"` to `defaultSkillNames` map

### Post-Flight Checks for Flight Leg 4

- [ ] **Skill exists:** `ls .claude/skills/boss-repair/SKILL.md`
- [ ] **Copy:** `make copy-skills` — copies to `lib/bossalib/skilldata/skills/boss-repair/`
- [ ] **Embed:** `cd lib/bossalib && go build ./skilldata/...` — compiles

### [HANDOFF] Review Flight Leg 4

Human reviews: Skill instructions are comprehensive and handle all three repair scenarios.

---

## Flight Leg 5: Daemon Auto-Start + Final Verification

### Tasks

**Auto-start** (`services/bossd/cmd/main.go`)

- [ ] After `pluginHost.Start()`, iterate `pluginHost.GetWorkflowServices()`
- [ ] Probe each with `GetInfo()` — if name == "repair", call `StartWorkflow` with default config
- [ ] Run in `safego.Go` — non-fatal if fails

**Tests**

- [ ] Add `pr_tracker_test.go` test for onChange callback
- [ ] Add dispatcher test verifying nil fixLoop behavior (may already exist)
- [ ] Run full suite: `make test`
- [ ] Run linter: `make lint`
- [ ] Run `buf lint`

**TODOS.md**

- [ ] Update "Expand HostService Beyond VCS Reads" — partially addressed
- [ ] Add "Repair Plugin: Configurable Cooldown + Retry Limits" TODO

### Post-Flight Checks for Final Verification

- [ ] **Full build:** `make` — passes
- [ ] **Lint:** `make lint` — clean
- [ ] **Tests:** `make test` — all pass
- [ ] **Proto:** `buf lint` — clean
- [ ] **Binary:** `ls bin/bossd-plugin-repair` — exists

### [HANDOFF] Final Review

Human reviews: Complete feature before merge.

---

## What Already Exists (reuse, don't rebuild)

| Component                 | Location                                               | Reuse                                         |
| ------------------------- | ------------------------------------------------------ | --------------------------------------------- |
| PRDisplayStatus enum      | `proto/bossanova/v1/models.proto:280-292`              | Status values for Failing/Conflict/Rejected   |
| ComputeDisplayStatus      | `lib/bossalib/vcs/display.go`                          | Already computes red statuses                 |
| DisplayPoller             | `services/bossd/internal/session/display_poller.go`    | Already polls every 2s, calls PRTracker.Set() |
| PRTracker                 | `services/bossd/internal/status/pr_tracker.go`         | Just needs onChange callback                  |
| Autopilot plugin pattern  | `plugins/bossd-plugin-autopilot/*`                     | Template for repair plugin                    |
| Autopilot host.go         | `plugins/bossd-plugin-autopilot/host.go`               | Extract to shared package                     |
| HostServiceServer         | `services/bossd/internal/plugin/host_service.go`       | SetWorkflowDeps pattern for SetSessionDeps    |
| Dispatcher nil fixLoop    | `services/bossd/internal/session/dispatcher.go:190`    | Already checks `d.fixLoop != nil`             |
| Skill embed pipeline      | `Makefile:131-139` + `lib/bossalib/skilldata/embed.go` | `make copy-skills` auto-picks up new skills   |
| boss-finalize skill       | `.claude/skills/boss-finalize/SKILL.md`                | boss-repair calls this as final step          |
| CreateAttempt from plugin | `plugins/bossd-plugin-autopilot/server.go:497-509`     | Pattern for repair plugin's attempt creation  |

## NOT In Scope

- License gating for paid plugins (separate TODO in TODOS.md)
- TUI notifications for repair events
- Cost budgets / spend limits for repair attempts
- Repair history dashboard or reporting
- Configurable cooldown intervals (hardcoded 5 min is fine for MVP)

## Failure Modes

| Failure                            | Impact                              | Mitigation                                       |
| ---------------------------------- | ----------------------------------- | ------------------------------------------------ |
| Repair plugin crashes              | No auto-repair, PRs stay red        | Daemon continues fine, manual repair still works |
| boss-repair skill fails            | Attempt marked failed, cooldown set | 5-min cooldown prevents thrashing                |
| FireSessionEvent fails             | Session stuck in wrong state        | Log error, manual state correction via CLI       |
| PRTracker onChange panics          | Display polling stops               | onChange runs in goroutine, recovered by safego  |
| Plugin not configured              | Feature simply absent               | Auto-start gracefully skips, no error            |
| Concurrent repairs on same session | Wasted API calls                    | `repairing` map prevents concurrent repairs      |

## Rollback Plan

All changes are additive except FixLoop removal:

- Re-add FixLoop: restore `fixloop.go`, pass to `NewDispatcher` in main.go
- New proto RPCs: backwards-compatible, no breaking changes
- New plugin: remove from settings.json
- New skill: remove `.claude/skills/boss-repair/`
- Shared hostclient: autopilot continues working, just has shared dep

## Notes

- The repair plugin registers as `WorkflowService` — no new plugin type needed
- **Push model** (not polling): daemon pushes `NotifyStatusChange` to all workflow plugins on every PRTracker status transition — near-instant reaction
- Cooldown of 5 minutes between repair attempts per session prevents repair loops
- The `boss-repair` skill is a single comprehensive skill that assesses and fixes all three problem types
- Force-push uses `--force-with-lease` for safety
- The skill calls `/boss-finalize` as its final step — full squash, PR numbers, mark ready
- FixLoop removal is safe: Dispatcher already checks `if d.fixLoop != nil` at lines 190, 233, 282
