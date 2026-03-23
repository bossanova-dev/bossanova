# Autopilot Plugin Implementation Plan

**Flight ID:** fp-2026-03-23-1514-autopilot-plugin

## Overview

Implement the autopilot plugin for bossd — a go-plugin that orchestrates end-to-end plan execution by sequentially invoking boss- skills (plan → implement → handoff/resume loop → verify → land). The plugin calls back to the daemon via HostService to create Claude attempts, manage workflow state, and stream output. Includes CLI commands, smart retry, plan validation, autopilot history, and diff preview.

## Affected Areas

- [ ] `proto/bossanova/v1/` — New WorkflowService + HostService additions (workflow/attempt RPCs)
- [ ] `lib/bossalib/plugin/shared.go` — New `PluginTypeWorkflow` constant
- [ ] `lib/bossalib/config/config.go` — `AutopilotConfig` typed struct
- [ ] `services/bossd/migrations/` — New `workflows` table migration
- [ ] `services/bossd/internal/db/` — New `WorkflowStore` implementation
- [ ] `services/bossd/internal/plugin/` — WorkflowService dispensing, HostService workflow/attempt RPCs
- [ ] `services/bossd/internal/server/` — New daemon RPCs for autopilot CLI commands + streaming
- [ ] `services/bossd/cmd/main.go` — Wire new stores and pass to plugin host + server
- [ ] `plugins/bossd-plugin-autopilot/` — New plugin binary (orchestration loop, host callbacks)
- [ ] `services/boss/cmd/` — New `autopilot` CLI subcommand group
- [ ] `go.work` — Add autopilot plugin module

## Design References

- Design doc: `docs/designs/autopilot-plugin.md`
- Dependabot plugin pattern: `plugins/bossd-plugin-dependabot/` (main.go, plugin.go, server.go, github.go)
- Host service pattern: `services/bossd/internal/plugin/host_service.go` (manual gRPC descriptors)
- Streaming RPC pattern: `services/bossd/internal/server/server.go:AttachSession` (ConnectRPC server-stream)
- Session creator pattern: `services/bossd/internal/taskorchestrator/session_creator.go`
- DB store pattern: `services/bossd/internal/db/attempt_store.go`
- Claude runner: `services/bossd/internal/claude/runner.go` (Start, Subscribe, History, IsRunning)
- Config pattern: `lib/bossalib/config/config.go` (Settings struct, Load, Save)
- Migration pattern: `services/bossd/migrations/20260321170000_task_mappings.sql` (Goose format)
- CLI pattern: `services/boss/cmd/main.go` (Cobra subcommands, `newClient(cmd)` for RPC)

---

## Flight Leg 1: Proto Definitions + Code Generation

### Tasks

- [ ] Add `PluginTypeWorkflow = "workflow"` to `lib/bossalib/plugin/shared.go`
  - Pattern: Follow existing `PluginTypeTaskSource`, `PluginTypeEventSource`, `PluginTypeScheduler`
- [ ] Add `WorkflowService` to `proto/bossanova/v1/plugin.proto`
  - RPCs: `GetInfo`, `StartWorkflow`, `PauseWorkflow`, `ResumeWorkflow`, `CancelWorkflow`, `GetWorkflowStatus`
  - Messages: `StartWorkflowRequest` (plan_path, session_id, repo_id, config overrides), `WorkflowStatus` (id, status, current_step, flight_leg, last_error, started_at)
  - Follow existing service patterns (TaskSourceService, EventSourceService)
- [ ] Add workflow/attempt RPCs to `proto/bossanova/v1/host_service.proto`
  - RPCs: `CreateWorkflow`, `UpdateWorkflow`, `GetWorkflow`, `ListWorkflows`, `CreateAttempt`, `GetAttemptStatus`, `StreamAttemptOutput`
  - `StreamAttemptOutput` is a server-streaming RPC (like `StreamEvents` in plugin.proto)
  - Messages: `Workflow` (matches DB schema), `AttemptStatus` (id, status, error, output_lines), `CreateAttemptRequest` (workflow_id, skill_name, input, work_dir)
- [ ] Add autopilot daemon RPCs to `proto/bossanova/v1/daemon.proto`
  - RPCs: `StartAutopilot`, `PauseAutopilot`, `ResumeAutopilot`, `CancelAutopilot`, `GetAutopilotStatus`, `ListAutopilotWorkflows`, `StreamAutopilotOutput`
  - `StreamAutopilotOutput` is server-streaming (like AttachSession)
  - Messages: `AutopilotWorkflow` (id, status, current_step, flight_leg, plan_path, elapsed, started_at)
- [ ] Add `WorkflowStatus` enum and `AutopilotWorkflow` model to `proto/bossanova/v1/models.proto`
  - Enum values: UNSPECIFIED, PENDING, RUNNING, PAUSED, COMPLETED, FAILED, CANCELLED
  - Enum: `WorkflowStep` — PLAN, IMPLEMENT, HANDOFF, RESUME, VERIFY, LAND
- [ ] Run `make generate` to regenerate Go + TypeScript code from protos
  - Verify generation succeeds with `make lint-proto`

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make generate && make lint-proto` — both pass with zero errors
- [ ] **Proto validation:** `buf lint` passes on all proto files
- [ ] **Generated code exists:** Verify `lib/bossalib/gen/bossanova/v1/` contains new workflow types
- [ ] **No regressions:** `make test` passes (existing tests still work with updated protos)

### [HANDOFF] Review Flight Leg 1

Human reviews: Proto API surface design — are the RPC signatures right? Are the message fields sufficient for the orchestration loop?

---

## Flight Leg 2: Database Layer (Migration + WorkflowStore)

### Tasks

- [ ] Create migration `services/bossd/migrations/20260323170000_workflows.sql`
  - Goose format (`-- +goose Up` / `-- +goose Down`)
  - Create `workflows` table:
    ```sql
    CREATE TABLE workflows (
      id               TEXT PRIMARY KEY,
      session_id       TEXT NOT NULL REFERENCES sessions(id),
      repo_id          TEXT NOT NULL REFERENCES repos(id),
      plan_path        TEXT NOT NULL,
      status           TEXT NOT NULL DEFAULT 'pending',
      current_step     TEXT NOT NULL DEFAULT 'plan',
      flight_leg       INTEGER NOT NULL DEFAULT 0,
      max_legs         INTEGER NOT NULL DEFAULT 20,
      last_error       TEXT,
      start_commit_sha TEXT,
      config_json      TEXT,
      created_at       TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
    );
    CREATE INDEX idx_workflows_session_id ON workflows(session_id);
    CREATE INDEX idx_workflows_status ON workflows(status);
    ```
  - Down migration: `DROP TABLE IF EXISTS workflows;`
- [ ] Add `Workflow` model to `lib/bossalib/models/`
  - New file `lib/bossalib/models/workflow.go` with `Workflow` struct
  - Fields match DB schema: ID, SessionID, RepoID, PlanPath, Status, CurrentStep, FlightLeg, MaxLegs, LastError, StartCommitSHA, ConfigJSON, CreatedAt, UpdatedAt
  - Status/Step as string types (not int enums — matches existing pattern for new models)
- [ ] Create `services/bossd/internal/db/workflow_store.go`
  - Interface: `WorkflowStore` with Create, Get, Update, List, ListByStatus methods
  - Implementation: `SQLiteWorkflowStore` following `AttemptStore` pattern
  - `CreateWorkflowParams`: SessionID, RepoID, PlanPath, MaxLegs, StartCommitSHA, ConfigJSON
  - `UpdateWorkflowParams`: Status, CurrentStep, FlightLeg, LastError (all pointer-optional)
  - `List(ctx, opts)` with optional status filter for `boss autopilot list`
  - Scanner helper: `scanWorkflow(s sqlutil.Scanner) (*models.Workflow, error)`
- [ ] Create `services/bossd/internal/db/workflow_store_test.go`
  - Table-driven tests: Create, Get, Update, List, ListByStatus, Get nonexistent (ErrNoRows)
  - Follow `attempt_store.go` test patterns

### Post-Flight Checks for Flight Leg 2

- [ ] **Quality gates:** `cd services/bossd && go build ./...` — compiles
- [ ] **Tests pass:** `cd services/bossd && go test ./internal/db/... -run TestWorkflow -v` — all pass
- [ ] **Migration applies:** Test migration by running daemon startup (migration runs on boot)
- [ ] **Schema verification:** Verify table exists with correct columns via `sqlite3` query in tests

### [HANDOFF] Review Flight Leg 2

Human reviews: DB schema matches design doc. Store methods cover all needed operations.

---

## Flight Leg 3: Config + Shared Constants

### Tasks

- [ ] Add `AutopilotConfig` to `lib/bossalib/config/config.go`
  - Add to `Settings` struct: `Autopilot AutopilotConfig `json:"autopilot,omitempty"``
  - `AutopilotConfig` struct:
    ```go
    type AutopilotConfig struct {
        Skills              AutopilotSkills `json:"skills,omitempty"`
        HandoffDir          string          `json:"handoff_dir,omitempty"`
        PollIntervalSeconds int             `json:"poll_interval_seconds,omitempty"`
        MaxFlightLegs       int             `json:"max_flight_legs,omitempty"`
        ConfirmLand         bool            `json:"confirm_land,omitempty"`
    }
    type AutopilotSkills struct {
        Plan      string `json:"plan,omitempty"`
        Implement string `json:"implement,omitempty"`
        Handoff   string `json:"handoff,omitempty"`
        Resume    string `json:"resume,omitempty"`
        Verify    string `json:"verify,omitempty"`
        Land      string `json:"land,omitempty"`
    }
    ```
  - Default methods: `HandoffDirectory() string` (default `docs/handoffs`), `PollInterval() time.Duration` (default 5s), `MaxLegs() int` (default 20)
  - `SkillName(step string) string` method that returns configured or default skill for each step
  - `ValidateSkills() error` that checks all configured skill names exist as files in `.claude/skills/`
- [ ] Add config round-trip tests to `lib/bossalib/config/config_test.go`
  - Test: marshal/unmarshal AutopilotConfig with all fields
  - Test: defaults applied when fields are empty/zero
  - Test: partial overrides merge with defaults

### Post-Flight Checks for Flight Leg 3

- [ ] **Quality gates:** `cd lib/bossalib && go test ./config/... -v` — all pass
- [ ] **Backwards compatible:** Existing settings.json without `autopilot` key still loads correctly
- [ ] **Round-trip:** Config with AutopilotConfig serializes and deserializes identically

### [HANDOFF] Review Flight Leg 3

Human reviews: Config field names and defaults match design doc. Skill validation logic.

---

## Flight Leg 4: Host-Side Plugin Infrastructure

### Tasks

- [ ] Add `WorkflowService` interface to `services/bossd/internal/plugin/grpc_plugins.go`
  - Interface: `WorkflowService` with `GetInfo`, `StartWorkflow`, `PauseWorkflow`, `ResumeWorkflow`, `CancelWorkflow`, `GetWorkflowStatus`
  - Follow existing `TaskSource`, `EventSource`, `Scheduler` interface pattern
- [ ] Add `WorkflowServiceGRPCPlugin` to `services/bossd/internal/plugin/grpc_plugins.go`
  - `GRPCClient` method: Register HostService on broker ID 1 (same as TaskSourceGRPCPlugin), return `workflowServiceGRPCClient`
  - `workflowServiceGRPCClient` struct wrapping `*grpc.ClientConn` with methods matching `WorkflowService` interface
  - Each method uses `conn.Invoke()` with path `/bossanova.v1.WorkflowService/{Method}`
- [ ] Add `PluginTypeWorkflow` to `NewPluginMap()` in `services/bossd/internal/plugin/shared.go`
  - Register `&WorkflowServiceGRPCPlugin{HostService: hostService}` alongside existing types
- [ ] Extend `managedPlugin` in `services/bossd/internal/plugin/host.go`
  - Add `workflowService WorkflowService` field (cached dispensed interface, nil if not a workflow service)
  - In `Start()`: after TaskSource dispensing, try to dispense WorkflowService with `sharedplugin.PluginTypeWorkflow`
  - Add `GetWorkflowServices() []WorkflowService` method (mirrors `GetTaskSources()`)
- [ ] Extend `HostServiceServer` in `services/bossd/internal/plugin/host_service.go`
  - Constructor: `NewHostServiceServer(provider, workflowStore, claude, logger)` — add dependencies
  - Add workflow RPCs to the manual `hostServiceDesc`:
    - `CreateWorkflow` — creates row in workflows table via store
    - `UpdateWorkflow` — updates status/step/leg/error via store
    - `GetWorkflow` — reads single workflow via store
    - `ListWorkflows` — lists workflows (for `boss autopilot list`) via store
  - Add attempt RPCs:
    - `CreateAttempt` — starts Claude process via `claude.Start(ctx, workDir, prompt, resume)`, returns attempt ID (which is the Claude session ID)
    - `GetAttemptStatus` — checks `claude.IsRunning(sessionID)`, returns status (running/completed/failed) + last output lines from `claude.History(sessionID)`
  - Add `StreamAttemptOutput` — streaming RPC that calls `claude.Subscribe(ctx, sessionID)` and streams OutputLines (mirrors AttachSession pattern)
  - Add handlers to `hostServiceDesc.Methods` and `hostServiceDesc.Streams` for new RPCs
- [ ] Update `hostServiceHandler` interface with the new methods

### Post-Flight Checks for Flight Leg 4

- [ ] **Quality gates:** `cd services/bossd && go build ./...` — compiles
- [ ] **Interface satisfaction:** Compile-time checks `var _ WorkflowService = (*workflowServiceGRPCClient)(nil)`
- [ ] **Plugin map:** `NewPluginMap()` returns map with 4 entries (task_source, event_source, scheduler, workflow)
- [ ] **Tests pass:** `cd services/bossd && go test ./internal/plugin/... -v` — existing tests still pass

### [HANDOFF] Review Flight Leg 4

Human reviews: gRPC client/server wiring follows dependabot pattern. Broker ID 1 reuse is correct. HostService expansion is backwards-compatible.

---

## Flight Leg 5: Autopilot Plugin Binary (Core Orchestration)

### Tasks

- [ ] Create `plugins/bossd-plugin-autopilot/` directory structure
  - `go.mod` — module `github.com/recurser/bossd-plugin-autopilot`, same deps as dependabot plugin
  - Add to `go.work`: `./plugins/bossd-plugin-autopilot`
- [ ] Create `plugins/bossd-plugin-autopilot/main.go`
  - Follow dependabot `main.go` pattern exactly
  - `goplugin.Serve` with Handshake, PluginSet using `sharedplugin.PluginTypeWorkflow`, `DefaultGRPCServer`
- [ ] Create `plugins/bossd-plugin-autopilot/plugin.go`
  - `workflowPlugin` struct implementing `goplugin.GRPCPlugin`
  - `GRPCServer`: create lazy host client, create orchestrator server, register `WorkflowService` descriptor
  - Manual `grpc.ServiceDesc` for `bossanova.v1.WorkflowService` with all 6 methods
  - Handler interface matching the WorkflowService RPCs
- [ ] Create `plugins/bossd-plugin-autopilot/host.go`
  - `hostServiceClient` struct wrapping `*grpc.ClientConn`
  - `lazyHostServiceClient` with `sync.Once` broker.Dial(1) pattern (copy from dependabot github.go)
  - Methods: `CreateWorkflow`, `UpdateWorkflow`, `GetWorkflow`, `CreateAttempt`, `GetAttemptStatus`, `StreamAttemptOutput`
  - Each method uses `conn.Invoke()` with path `/bossanova.v1.HostService/{Method}`
  - `StreamAttemptOutput` uses `grpc.NewStream()` for server-streaming from host
- [ ] Create `plugins/bossd-plugin-autopilot/server.go` — the core orchestration loop
  - `orchestrator` struct with `host hostClient`, `logger zerolog.Logger`
  - `StartWorkflow(ctx, req)` method:
    1. Validate plan file path (check it's relative, no `..`, file exists check delegated to Claude)
    2. Validate skill names via checking configured skill overrides exist
    3. Call `host.CreateWorkflow(pending)`
    4. Call `host.UpdateWorkflow(running)`
    5. Call `runFlightLeg(ctx, workflow, "plan", planPath)`
    6. Enter handoff/resume loop:
       - Scan handoff directory for new files (mtime > last attempt start)
       - If no new handoff file → `runFlightLeg(ctx, workflow, "verify", ...)`
       - If new handoff file → `runFlightLeg(ctx, workflow, "resume", handoffPath)`
       - Increment flight_leg, check max_legs
    7. After verify: run diff preview (call host to get git diff stat)
    8. If confirm_land: pause workflow, return
    9. Run `runFlightLeg(ctx, workflow, "land", ...)`
    10. Call `host.UpdateWorkflow(completed)`
  - `runFlightLeg(ctx, workflow, step, input)` method:
    1. Update workflow: current_step=step, increment flight_leg
    2. Build prompt: `/{skill_name} {input}`
    3. Call `host.CreateAttempt(workflow_id, skill_name, prompt, work_dir)`
    4. Poll `host.GetAttemptStatus(attempt_id)` every 5 seconds
    5. On completion: check for error, return result
    6. On failure: capture error, attempt smart retry (once), then return error
  - `smartRetry(ctx, workflow, step, input, lastError)` method:
    1. Build retry prompt: `/{skill_name} {input}\n\nPrevious attempt failed with: {lastError}. Please address this and continue.`
    2. If lastError is empty/non-actionable (timeout, crash): use generic prompt
    3. Call `host.CreateAttempt` with retry prompt
    4. Poll for completion
  - `PauseWorkflow`, `ResumeWorkflow`, `CancelWorkflow` — update workflow status via host
  - `GetWorkflowStatus` — read workflow from host
- [ ] Create `plugins/bossd-plugin-autopilot/handoff.go`
  - `scanHandoffDir(dir string, since time.Time) (string, error)` — reads directory, filters by mtime, returns newest file path
  - Validates dir is relative and exists
  - Returns empty string if no new files (signals workflow should proceed to verify)

### Post-Flight Checks for Flight Leg 5

- [ ] **Quality gates:** `cd plugins/bossd-plugin-autopilot && go build ./...` — compiles
- [ ] **Binary builds:** `go build -o /tmp/bossd-plugin-autopilot ./plugins/bossd-plugin-autopilot/`
- [ ] **go.work valid:** `go work sync` succeeds
- [ ] **No import cycles:** Build succeeds without circular dependency errors

### [HANDOFF] Review Flight Leg 5

Human reviews: Orchestration loop logic is correct. Smart retry prompt construction. Handoff directory scanning. Error handling at each step.

---

## Flight Leg 6: Plugin Tests

### Tasks

- [ ] Create `plugins/bossd-plugin-autopilot/server_test.go`
  - `mockHostClient` implementing the `hostClient` interface with preconfigured responses
  - Table-driven tests for the orchestration loop:
    - **Happy path:** plan → implement → no handoff → verify → land → completed
    - **Handoff loop:** plan → implement → handoff found → resume → no handoff → verify → land
    - **Multiple handoffs:** plan → implement → handoff → resume → handoff → resume → verify → land
    - **Max legs exceeded:** Loop until max_legs, verify workflow completes without land
    - **Plan file validation failure:** Invalid skill → error returned, no workflow created
    - **Flight leg failure + retry success:** First attempt fails, smart retry succeeds → continues
    - **Flight leg failure + retry failure:** Both attempts fail → workflow paused
    - **Non-retryable error:** Plugin crash → immediate pause, no retry
    - **Pause mid-workflow:** Pause flag set → workflow pauses after current leg
    - **Resume from paused:** Workflow resumes from correct step
    - **Cancel running workflow:** Workflow marked cancelled
    - **Confirm-land pause:** After verify, workflow pauses for confirmation
    - **Diff preview:** After verify, git diff stat returned
    - **Empty plan path:** Error returned
    - **Handoff dir missing:** Error, workflow pauses
    - **Multiple handoff files:** Newest file selected
  - Compile-time interface checks: `var _ hostClient = (*mockHostClient)(nil)`
- [ ] Create `plugins/bossd-plugin-autopilot/handoff_test.go`
  - Tests for `scanHandoffDir`:
    - No files → empty string
    - One new file → returns path
    - Multiple files, pick newest by mtime
    - Old files only (before `since`) → empty string
    - Directory doesn't exist → error
    - Mixed old and new files → only new file returned

### Post-Flight Checks for Flight Leg 6

- [ ] **Quality gates:** `cd plugins/bossd-plugin-autopilot && go test ./... -v` — all tests pass
- [ ] **Coverage:** At least 80% coverage on server.go and handoff.go
- [ ] **Table-driven:** All test functions use `tests := []struct{...}` pattern

### [HANDOFF] Review Flight Leg 6

Human reviews: Test coverage adequacy. Mock fidelity. Edge case coverage matches design doc failure modes.

---

## Flight Leg 7: Daemon Server RPCs + Wiring

### Tasks

- [ ] Add autopilot RPCs to `services/bossd/internal/server/server.go`
  - `StartAutopilot(ctx, req)` — find WorkflowService plugin, call StartWorkflow, return workflow ID
  - `PauseAutopilot(ctx, req)` — find plugin, call PauseWorkflow
  - `ResumeAutopilot(ctx, req)` — find plugin, call ResumeWorkflow
  - `CancelAutopilot(ctx, req)` — find plugin, call CancelWorkflow
  - `GetAutopilotStatus(ctx, req)` — read workflow from WorkflowStore (or call plugin)
  - `ListAutopilotWorkflows(ctx, req)` — read from WorkflowStore
  - `StreamAutopilotOutput(ctx, req, stream)` — streaming RPC, follows AttachSession pattern:
    1. Get workflow from store
    2. Get current Claude session ID from attempt
    3. Send history burst from `claude.History()`
    4. Subscribe to `claude.Subscribe(ctx, sessionID)`
    5. Stream output lines until process exits or client disconnects
    6. Send final workflow status
- [ ] Add `Server.Config` fields for new dependencies:
  - `WorkflowStore db.WorkflowStore`
  - `PluginHost *plugin.Host` (to access `GetWorkflowServices()`)
- [ ] Register new RPCs in the ConnectRPC handler setup (mux.Handle)
  - Add autopilot RPCs to the `DaemonServiceHandler`
- [ ] Wire new components in `services/bossd/cmd/main.go`:
  - Create `WorkflowStore` from database: `workflows := db.NewWorkflowStore(database)`
  - Pass `WorkflowStore` and `ClaudeRunner` to `HostServiceServer` constructor
  - Pass `WorkflowStore` and `PluginHost` to `Server.Config`
  - Update `plugin.New()` call if constructor signature changed

### Post-Flight Checks for Flight Leg 7

- [ ] **Quality gates:** `cd services/bossd && go build ./cmd/...` — daemon compiles
- [ ] **Server starts:** `./bin/bossd` starts without errors, listens on socket
- [ ] **RPC registered:** New RPCs appear in ConnectRPC handler (verified by build + manual inspection)
- [ ] **Existing functionality:** `make test` — all existing tests pass

### [HANDOFF] Review Flight Leg 7

Human reviews: Server wiring correctness. Dependency injection. Streaming RPC follows AttachSession pattern exactly.

---

## Flight Leg 8: CLI Commands

### Tasks

- [ ] Create `services/boss/cmd/autopilot.go` — Cobra subcommand group
  - `autopilotCmd()` returns parent command with subcommands:
    - `autopilotStartCmd()` — `boss autopilot start <plan-file> [--max-legs N] [--confirm-land]`
      - Reads plan-file arg, calls `StartAutopilot` RPC
      - Prints workflow ID and status
    - `autopilotStatusCmd()` — `boss autopilot status [workflow-id] [--follow]`
      - Without --follow: calls `GetAutopilotStatus`, prints table
      - With --follow: calls `StreamAutopilotOutput`, streams lines to terminal
    - `autopilotListCmd()` — `boss autopilot list [--all]`
      - Calls `ListAutopilotWorkflows`, prints table (ID, status, step, legs, duration)
      - Without --all: filters to active/paused only
    - `autopilotPauseCmd()` — `boss autopilot pause [workflow-id]`
      - Calls `PauseAutopilot`, prints confirmation
    - `autopilotResumeCmd()` — `boss autopilot resume [workflow-id]`
      - Calls `ResumeAutopilot`, prints confirmation
    - `autopilotCancelCmd()` — `boss autopilot cancel [workflow-id]`
      - Calls `CancelAutopilot`, prints confirmation
  - Default workflow-id resolution: if no ID provided, find most recent active/paused workflow; error if multiple
  - Register with `root.AddCommand(autopilotCmd())` in `rootCmd()`
- [ ] Add table formatting for `autopilot list` output
  - Follow existing `boss ls` output formatting patterns
  - Columns: ID (short), Status, Step, Leg, Plan, Duration, Created

### Post-Flight Checks for Flight Leg 8

- [ ] **Quality gates:** `cd services/boss && go build ./cmd/...` — CLI compiles
- [ ] **Command registration:** `boss autopilot --help` shows all subcommands
- [ ] **Subcommand help:** `boss autopilot start --help` shows flags (--max-legs, --confirm-land)
- [ ] **No regressions:** `boss --help` still shows all existing commands plus new `autopilot`

### [HANDOFF] Review Flight Leg 8

Human reviews: CLI UX — command names, flag names, output formatting, error messages.

---

## Flight Leg 9: Integration Testing + Skill Renaming

### Tasks

- [ ] Add integration test for autopilot plugin lifecycle in `services/bossd/internal/plugin/`
  - Test: HostService workflow RPCs (CreateWorkflow, UpdateWorkflow, GetWorkflow, ListWorkflows)
  - Test: HostService attempt RPCs (CreateAttempt, GetAttemptStatus) with mock Claude runner
  - Follow existing `integration_test.go` patterns
- [ ] Add WorkflowStore integration tests in `services/bossd/internal/db/`
  - Test full lifecycle: create → update status → update step → list by status
  - Test concurrent access patterns
- [ ] Rename existing skills to `boss-` prefix:
  - `pre-flight-checks` → `boss-plan`
  - `take-off` → `boss-implement`
  - `handoff-task` → `boss-handoff`
  - `resume-handoff` → `boss-resume`
  - `post-flight-checks` → `boss-verify`
  - `land-the-plane` → `boss-land`
  - `file-a-flight-plan` → `boss-flight-plan`
  - Keep old skill directories as symlinks for backwards compatibility (or just rename)
  - Update any internal references to old skill names
- [ ] Add Makefile target for building autopilot plugin binary
  - `$(BIN_DIR)/bossd-plugin-autopilot: go build -o $(BIN_DIR)/bossd-plugin-autopilot ./plugins/bossd-plugin-autopilot/`

### Post-Flight Checks for Flight Leg 9

- [ ] **Quality gates:** `make test` — ALL tests pass across all modules
- [ ] **Integration tests:** `cd services/bossd && go test ./internal/plugin/... -run TestIntegration -v`
- [ ] **Plugin builds:** `make build` includes autopilot plugin binary
- [ ] **Skills renamed:** `ls .claude/skills/` shows boss- prefixed directories

### [HANDOFF] Review Flight Leg 9

Human reviews: Integration test coverage. Skill renaming completeness. Build system changes.

---

## Flight Leg 10: Final Verification + End-to-End

### Tasks

- [ ] Run full test suite: `make test`
- [ ] Run linter: `make lint`
- [ ] Run proto linter: `make lint-proto`
- [ ] Build all binaries: `make build`
- [ ] Verify no unused exports or dead code
- [ ] Manual end-to-end smoke test:
  1. Start daemon with autopilot plugin enabled in settings.json
  2. `boss autopilot list` — empty list, no errors
  3. `boss autopilot start docs/plans/some-test-plan.md` — workflow starts (or fails gracefully if no Claude API key)
  4. `boss autopilot status` — shows workflow state
  5. `boss autopilot pause` — pauses workflow
  6. `boss autopilot resume` — resumes workflow
  7. `boss autopilot cancel` — cancels workflow

### Post-Flight Checks for Final Verification

- [ ] **End-to-end test:** All `make` targets pass (generate, lint, test, build)
- [ ] **Binary sizes:** Autopilot plugin binary is reasonable size (< 20MB)
- [ ] **No panics:** Daemon starts and stops cleanly with autopilot plugin loaded
- [ ] **Backwards compatible:** Daemon works without autopilot plugin configured

### [HANDOFF] Final Review

Human reviews: Complete feature before merge. All tests pass. All binaries build. Design doc requirements met.

---

## Rollback Plan

Git revert the PR. The `workflows` migration creates a new table — reverting the code doesn't drop it (harmless). The plugin binary stops loading. CLI `autopilot` subcommand disappears. No data loss, no breaking changes to existing functionality.

## Notes

- **Broker ID 1 reuse:** Both TaskSource and WorkflowService register HostService on broker ID 1. This works because go-plugin creates a separate broker per plugin process — each plugin gets its own broker namespace.
- **Manual gRPC descriptors:** The project uses ConnectRPC for codegen (not protoc-gen-go-grpc), so plugin-side gRPC service descriptors must be written manually. Follow the exact pattern from `plugins/bossd-plugin-dependabot/plugin.go`.
- **Lazy host client pattern:** Plugin's `GRPCServer` runs before the host calls `AcceptAndServe` on the broker. The lazy client pattern (sync.Once on first RPC call) is mandatory.
- **Streaming over go-plugin:** The `StreamAttemptOutput` RPC from host to plugin uses gRPC server-streaming. This is different from ConnectRPC streaming (used for CLI-to-daemon). The plugin calls `grpc.NewStream()` to consume the host's stream.
- **start_commit_sha:** Store the git HEAD at workflow start for accurate diff preview (CEO review decision).
- **Handoff dir validation:** Validate `handoff_dir` is relative and doesn't contain `..` (security requirement from CEO review Section 3).
