# Linear Integration Plugin Implementation Plan

**Flight ID:** fp-2026-04-07-linear-integration-plugin

## Overview

Add a Linear issue tracker plugin that lets users pick a Linear ticket when creating a new session. If a PR already exists for the ticket, the session attaches to it. If not, a new branch and draft PR are created using Linear's suggested branch name and `[ENG-123] Title` format.

The plugin ships as a separate binary (`bossd-plugin-linear`) excluded from the public repo via the copy-and-strip mirror.

## Affected Areas

- [ ] `proto/bossanova/v1/` — New messages (TrackerIssue), new RPCs (ListAvailableIssues, ListTrackerIssues), Repo/UpdateRepoRequest field additions
- [ ] `services/bossd/migrations/` — New migration for linear_api_key, linear_team_key columns
- [ ] `lib/bossalib/models/` — Repo struct additions
- [ ] `services/bossd/internal/db/` — RepoStore scan/update changes
- [ ] `services/bossd/internal/server/` — ListTrackerIssues handler, UpdateRepo handler changes
- [ ] `services/bossd/internal/plugin/` — TaskSource interface + gRPC client extension
- [ ] `services/boss/internal/client/` — BossClient interface + local client extension
- [ ] `services/boss/internal/views/` — Repo settings (2 new rows), new session wizard (Linear ticket type + issue picker phase)
- [ ] `plugins/bossd-plugin-linear/` — New plugin binary (GraphQL client, PR matching, ListAvailableIssues)

## Design References

- Reference plugin: `plugins/bossd-plugin-dependabot/` (server.go, plugin.go, github.go, main.go)
- Repo settings pattern: `services/boss/internal/views/repo_settings.go` (row constants, editingField, commitEdit)
- Session wizard pattern: `services/boss/internal/views/newsession.go` (phases, sessionTypeOptions, startCreating)
- Plugin interface: `services/bossd/internal/plugin/grpc_plugins.go` (TaskSource interface, gRPC client)
- Test pattern: `services/boss/internal/views/newsession_test.go` (stubClient, sendKey/sendMsg helpers)

---

> **IMPORTANT — Machine-Parsed Headers:** The `## Flight Leg N:` headings and
> `### [HANDOFF]` markers are parsed by the autopilot to count flight legs.
> Plans MUST use exactly this heading format. Without them, the autopilot cannot
> determine how many legs the plan has.

## Flight Leg 1: Proto Definitions + Code Generation

### Tasks

- [ ] Add `TrackerIssue` message to `proto/bossanova/v1/models.proto`
  - Files: `proto/bossanova/v1/models.proto`
  - Add after `PRInfo` message (after line 232):
    ```protobuf
    // TrackerIssue represents an issue from an external tracker (Linear, Jira, etc.).
    message TrackerIssue {
      string external_id = 1;      // eg. "ENG-123"
      string title = 2;
      string description = 3;
      string branch_name = 4;       // Suggested branch name from tracker
      string url = 5;               // Link to issue in tracker
      string state = 6;             // eg. "In Progress", "Todo"
      int32 pr_number = 7;          // Existing PR if found (0 = none)
      string existing_branch = 8;   // Existing branch if found
    }
    ```

- [ ] Add `linear_api_key` and `linear_team_key` to `Repo` message in `models.proto`
  - Files: `proto/bossanova/v1/models.proto`
  - Repo currently has fields 1-14. Add:
    ```protobuf
    string linear_api_key = 15;
    string linear_team_key = 16;
    ```

- [ ] Add `ListAvailableIssues` RPC to `TaskSourceService` in `plugin.proto`
  - Files: `proto/bossanova/v1/plugin.proto`
  - Add after `UpdateTaskStatus` RPC (line 20):
    ```protobuf
    // ListAvailableIssues returns browsable issues from the external tracker.
    rpc ListAvailableIssues(ListAvailableIssuesRequest) returns (ListAvailableIssuesResponse);
    ```
  - Add request/response messages after `TaskItemStatus` enum (after line 132):

    ```protobuf
    message ListAvailableIssuesRequest {
      string repo_origin_url = 1;
      map<string, string> config = 2;
    }

    message ListAvailableIssuesResponse {
      repeated TrackerIssue issues = 1;
    }
    ```

- [ ] Add `ListTrackerIssues` RPC and fields to `daemon.proto`
  - Files: `proto/bossanova/v1/daemon.proto`
  - Add RPC to DaemonService (after `ListRepoPRs`):
    ```protobuf
    rpc ListTrackerIssues(ListTrackerIssuesRequest) returns (ListTrackerIssuesResponse);
    ```
  - Add `linear_api_key` (field 9) and `linear_team_key` (field 10) to `UpdateRepoRequest`
  - Add `optional string branch_name = 8;` to `CreateSessionRequest` (for Linear's suggested branch name)
  - Add request/response messages:

    ```protobuf
    message ListTrackerIssuesRequest {
      string repo_id = 1;
    }

    message ListTrackerIssuesResponse {
      repeated TrackerIssue issues = 1;
    }
    ```

- [ ] Run `make generate` to regenerate Go and TypeScript code from protos
  - Command: `make generate`
  - Verify: `lib/bossalib/gen/bossanova/v1/` has updated `.pb.go` and `.connect.go` files

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make generate` succeeds without errors
- [ ] **Proto lint:** `buf lint` passes (if configured)
- [ ] **Verify generated code:** Generated Go files include `TrackerIssue`, `ListAvailableIssuesRequest/Response`, `ListTrackerIssuesRequest/Response`, and `Repo` has `LinearApiKey`/`LinearTeamKey` fields
- [ ] **Compile check:** `go build ./...` from `lib/bossalib/` succeeds

### [HANDOFF] Review Flight Leg 1

Human reviews: Proto field numbers, message naming, RPC placement in services.

---

## Flight Leg 2: Database, Store, Daemon Handler, Plugin Interface

### Tasks

- [ ] Add SQLite migration for `linear_api_key` and `linear_team_key` columns
  - Files: `services/bossd/migrations/20260407170000_linear_config.sql`
  - Pattern: Follow `20260318170000_repo_settings.sql`
  - Content:

    ```sql
    -- +goose Up
    ALTER TABLE repos ADD COLUMN linear_api_key TEXT NOT NULL DEFAULT '';
    ALTER TABLE repos ADD COLUMN linear_team_key TEXT NOT NULL DEFAULT '';

    -- +goose Down
    -- SQLite doesn't support DROP COLUMN; recreate table if needed.
    ```

- [ ] Add `LinearAPIKey` and `LinearTeamKey` to `models.Repo` struct
  - Files: `lib/bossalib/models/models.go`
  - Add fields after `MergeStrategy`:
    ```go
    LinearAPIKey  string
    LinearTeamKey string
    ```

- [ ] Update `repo_store.go`: scan, create, update for new columns
  - Files: `services/bossd/internal/db/repo_store.go`
  - Update `scanRepo()`: add `linear_api_key, linear_team_key` to SELECT and Scan
  - Update `Create()`: add columns to INSERT
  - Update `Update()`: add `LinearAPIKey *string` and `LinearTeamKey *string` to `UpdateRepoParams` in `store.go`, handle in Update()
  - Pattern: Follow existing nullable field pattern (`if params.LinearAPIKey != nil { sets = append(...) }`)

- [ ] Update daemon `UpdateRepo` handler to pass through new fields
  - Files: `services/bossd/internal/server/server.go`
  - In `UpdateRepo()` handler, map `req.Msg.LinearApiKey` and `req.Msg.LinearTeamKey` to `UpdateRepoParams`
  - In repo-to-proto conversion, populate `LinearApiKey` and `LinearTeamKey` on the returned `Repo` message

- [ ] Extend `TaskSource` interface with `ListAvailableIssues`
  - Files: `services/bossd/internal/plugin/grpc_plugins.go`
  - Add to `TaskSource` interface:
    ```go
    ListAvailableIssues(ctx context.Context, repoOriginURL string, config map[string]string) ([]*bossanovav1.TrackerIssue, error)
    ```
  - Add to `taskSourceGRPCClient`:
    ```go
    func (c *taskSourceGRPCClient) ListAvailableIssues(ctx context.Context, repoOriginURL string, config map[string]string) ([]*bossanovav1.TrackerIssue, error) {
        resp := &bossanovav1.ListAvailableIssuesResponse{}
        err := c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/ListAvailableIssues",
            &bossanovav1.ListAvailableIssuesRequest{RepoOriginUrl: repoOriginURL, Config: config}, resp)
        if err != nil { return nil, err }
        return resp.Issues, nil
    }
    ```

- [ ] Add `ListTrackerIssues` daemon handler
  - Files: `services/bossd/internal/server/server.go`
  - Handler flow:
    1. Validate `repo_id` is not empty
    2. Get repo from store (`s.repos.Get(ctx, repoID)`)
    3. Check `LinearAPIKey` is not empty → return `connect.CodeFailedPrecondition` if missing
    4. Find first TaskSource plugin from `s.pluginHost.GetTaskSources()`
    5. Call `source.ListAvailableIssues(ctx, repo.OriginURL, map[string]string{"linear_api_key": repo.LinearAPIKey, "linear_team_key": repo.LinearTeamKey})`
    6. Convert `[]*bossanovav1.TrackerIssue` to response
  - Pattern: Follow `ListRepoPRs` handler structure

- [ ] Add `ListTrackerIssues` to `BossClient` interface and `LocalClient`
  - Files: `services/boss/internal/client/client.go`, `services/boss/internal/client/local.go`
  - Interface:
    ```go
    ListTrackerIssues(ctx context.Context, repoID string) ([]*pb.TrackerIssue, error)
    ```
  - LocalClient: call `s.client.ListTrackerIssues(ctx, connect.NewRequest(&pb.ListTrackerIssuesRequest{RepoId: repoID}))`

### Post-Flight Checks for Flight Leg 2

- [ ] **Quality gates:** `make test-bossd` passes (existing tests still green)
- [ ] **Migration:** Verify migration file exists and has correct goose markers
- [ ] **Compile check:** `go build ./services/bossd/...` and `go build ./services/boss/...` both succeed
- [ ] **New handler registered:** `ListTrackerIssues` appears in generated connect handler code

### [HANDOFF] Review Flight Leg 2

Human reviews: Store update pattern, handler error codes, interface signature.

---

## Flight Leg 3: TUI — Repo Settings (Linear Config Rows)

### Tasks

- [ ] Add `repoSettingsRowLinearApiKey` and `repoSettingsRowLinearTeamKey` row constants
  - Files: `services/boss/internal/views/repo_settings.go`
  - Insert after `repoSettingsRowCanAutoResolveConflicts = 6`:
    ```go
    repoSettingsRowLinearApiKey            = 7
    repoSettingsRowLinearTeamKey           = 8
    repoSettingsRowCount                   = 9
    ```

- [ ] Add `linearApiKeyInput` and `linearTeamKeyInput` text inputs to `RepoSettingsModel`
  - Files: `services/boss/internal/views/repo_settings.go`
  - Add fields:
    ```go
    linearApiKeyInput  textinput.Model
    linearTeamKeyInput textinput.Model
    ```
  - Initialize in `NewRepoSettingsModel()`:

    ```go
    aki := textinput.New()
    aki.Placeholder = "lin_api_..."
    aki.SetWidth(60)
    // Note: do NOT pre-fill for API key (full replace, not edit)

    tki := textinput.New()
    tki.Placeholder = "e.g. ENG"
    tki.SetWidth(60)
    ```

  - On `repoSettingsLoadedMsg`: set `linearTeamKeyInput.SetValue(m.repo.LinearTeamKey)` (but NOT the API key — it's always full replace)

- [ ] Implement masked display for API key and activateRow/commitEdit for both rows
  - Files: `services/boss/internal/views/repo_settings.go`
  - **Masked display helper:**
    ```go
    func maskAPIKey(key string) string {
        if key == "" { return "(not set)" }
        if len(key) <= 4 { return key }
        return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
    }
    ```
  - **activateRow:** Add cases for new rows
    - `repoSettingsRowLinearApiKey`: set `editingField`, reset input to empty (full replace), focus
    - `repoSettingsRowLinearTeamKey`: set `editingField`, focus (pre-filled with current value)
  - **commitEdit:** Add cases for new rows
    - API key: `m.saveSettings(&pb.UpdateRepoRequest{Id: m.repoID, LinearApiKey: &val})`
    - Team key: `m.saveSettings(&pb.UpdateRepoRequest{Id: m.repoID, LinearTeamKey: &val})`
  - **updateEditing:** Add input forwarding for new rows
  - **cancelEdit:** Add input reset for new rows

- [ ] Render the two new rows in `View()`
  - Files: `services/boss/internal/views/repo_settings.go`
  - Add a separator (`b.WriteString("\n")`) before the Linear section
  - Render API key row: masked display when not editing, text input when editing
  - Render team key row: plain display when not editing, text input when editing
  - Pattern: Follow the Name/SetupScript row rendering pattern

- [ ] Add unit tests for repo settings Linear rows
  - Files: `services/boss/internal/views/repo_settings_test.go` (new file)
  - Tests:
    - `TestRepoSettings_MaskAPIKey` — verify masking logic (empty, short, normal)
    - `TestRepoSettings_LinearApiKeyEditIsFullReplace` — verify input starts empty, not pre-filled
    - `TestRepoSettings_LinearTeamKeyEditIsPrefilled` — verify input starts with current value
    - `TestRepoSettings_CursorNavigatesToLinearRows` — verify rows 7/8 are reachable

### Post-Flight Checks for Flight Leg 3

- [ ] **Quality gates:** `make test-boss` passes
- [ ] **Masking test:** `maskAPIKey("lin_api_abcdefghij1234")` returns `"****************1234"`
- [ ] **Masking test:** `maskAPIKey("")` returns `"(not set)"`
- [ ] **Compile check:** `go build ./services/boss/...` succeeds
- [ ] **Row count:** `repoSettingsRowCount` equals 9

### [HANDOFF] Review Flight Leg 3

Human reviews: API key masking renders correctly, row navigation works, edit/save pattern consistent.

---

## Flight Leg 4: TUI — New Session Wizard (Linear Ticket Flow)

### Tasks

- [ ] Add `sessionTypeLinearTicket` and `newSessionPhaseIssueSelect`
  - Files: `services/boss/internal/views/newsession.go`
  - Add `sessionTypeLinearTicket` to the `sessionType` iota (after `sessionTypeExecutePlan`)
  - Add `newSessionPhaseIssueSelect` to the `newSessionPhase` iota (after `newSessionPhasePRSelect`)
  - Add fields to `NewSessionModel`:
    ```go
    trackerIssues    []*pb.TrackerIssue
    issueTable       table.Model  // bubbles table for issue selection
    issueTableReady  bool
    issueErr         error
    selectedIssue    *pb.TrackerIssue
    ```

- [ ] Build dynamic `sessionTypeOptions` based on repo config
  - Files: `services/boss/internal/views/newsession.go`
  - Change `sessionTypeOptions` from a package-level `var` to a method on `NewSessionModel`:
    ```go
    func (m *NewSessionModel) buildSessionTypeOptions() []struct{ label string; desc string; typ sessionType } {
        opts := []struct{ label string; desc string; typ sessionType }{
            {"Create a new PR", "Start a fresh branch and pull request", sessionTypeNewPR},
            {"Work on an existing PR", "Attach to an open pull request", sessionTypeExistingPR},
            {"Quick chat", "Work directly in the repo's base folder", sessionTypeQuickChat},
        }
        // Add Linear ticket option if repo has Linear API key configured
        repo := m.selectedRepo()
        if repo != nil && repo.LinearApiKey != "" {
            // Insert before Quick chat
            opts = append(opts[:2], append([]struct{ ... }{
                {"Work on a Linear ticket", "Pick a ticket from your Linear board", sessionTypeLinearTicket},
            }, opts[2:]...)...)
        }
        return opts
    }
    ```
  - Update all references to `sessionTypeOptions` to use `m.buildSessionTypeOptions()`

- [ ] Implement `advanceFromTypeSelect` for Linear ticket flow
  - Files: `services/boss/internal/views/newsession.go`
  - Add case in `advanceFromTypeSelect()`:
    ```go
    case sessionTypeLinearTicket:
        m.phase = newSessionPhaseIssueSelect
        return m.fetchIssues()
    ```
  - Add `fetchIssues()` command:
    ```go
    func (m *NewSessionModel) fetchIssues() tea.Cmd {
        return func() tea.Msg {
            issues, err := m.client.ListTrackerIssues(m.ctx, m.selectedRepo().Id)
            return issuesMsg{issues: issues, err: err}
        }
    }
    ```
  - Add `issuesMsg` type and handle in `Update()`:
    ```go
    type issuesMsg struct {
        issues []*pb.TrackerIssue
        err    error
    }
    ```

- [ ] Implement issue selection and `startCreating` for Linear tickets
  - Files: `services/boss/internal/views/newsession.go`
  - Handle `issuesMsg`: build table rows from issues, set `issueTableReady`
  - Handle key navigation in `newSessionPhaseIssueSelect`: j/k/up/down + enter to select
  - In `startCreating()`, add `sessionTypeLinearTicket` case:
    ```go
    case sessionTypeLinearTicket:
        issue := m.selectedIssue
        req.Title = fmt.Sprintf("[%s] %s", issue.ExternalId, issue.Title)
        req.Plan = issue.Description
        if issue.PrNumber > 0 {
            prNum := issue.PrNumber
            req.PrNumber = &prNum
        } else {
            // New branch using Linear's suggested name
            if issue.BranchName != "" {
                req.BranchName = &issue.BranchName
            }
            // force_branch is already false by default
        }
    ```
  - Render issue table in `View()` for `newSessionPhaseIssueSelect`: loading spinner, table, or error

- [ ] Add `ListTrackerIssues` to `stubClient` and write tests
  - Files: `services/boss/internal/views/newsession_test.go`
  - Add to `stubClient`:
    ```go
    trackerIssues    []*pb.TrackerIssue
    trackerIssuesErr error
    ```
  - Tests:
    - `TestNewSession_LinearTicketOptionHiddenWithoutConfig` — no `LinearApiKey` → option not shown
    - `TestNewSession_LinearTicketOptionShownWithConfig` — with `LinearApiKey` → option visible
    - `TestNewSession_LinearTicketCreatesSessionWithBracketTitle` — selected issue → title is `[ENG-123] Title`
    - `TestNewSession_LinearTicketExistingPRAttaches` — issue with `PrNumber > 0` → `CreateSessionRequest.PrNumber` set
    - `TestNewSession_LinearTicketNewBranch` — issue without PR → no `PrNumber`, plan set to description

### Post-Flight Checks for Flight Leg 4

- [ ] **Quality gates:** `make test-boss` passes
- [ ] **Conditional visibility:** Test confirms Linear option hidden when `LinearApiKey` is empty
- [ ] **Session creation:** Test confirms `[ENG-123] Title` format in `CreateSessionRequest.Title`
- [ ] **Existing PR path:** Test confirms `PrNumber` set when `TrackerIssue.PrNumber > 0`
- [ ] **Compile check:** `go build ./services/boss/...` succeeds

### [HANDOFF] Review Flight Leg 4

Human reviews: Session wizard flow, issue table rendering, conditional option logic, title format.

---

## Flight Leg 5: Plugin Binary (bossd-plugin-linear)

### Tasks

- [ ] Create plugin directory structure and `main.go`
  - Files: `plugins/bossd-plugin-linear/main.go`
  - Pattern: Copy structure from `plugins/bossd-plugin-dependabot/main.go`
  - Register `sharedplugin.PluginTypeTaskSource` with `&taskSourcePlugin{}`

- [ ] Create `plugin.go` with manually-built gRPC ServiceDesc
  - Files: `plugins/bossd-plugin-linear/plugin.go`
  - Pattern: Mirror `plugins/bossd-plugin-dependabot/plugin.go`
  - ServiceDesc includes 4 methods: `GetInfo`, `PollTasks`, `UpdateTaskStatus`, `ListAvailableIssues`
  - `GRPCServer()`: create eager host client, register server

- [ ] Create `linear.go` with GraphQL client
  - Files: `plugins/bossd-plugin-linear/linear.go`
  - Struct:
    ```go
    type linearClient struct {
        apiKey   string
        teamKey  string
        endpoint string // default "https://api.linear.app/graphql"
    }
    ```
  - Method `FetchIssues(ctx) ([]linearIssue, error)`:
    - Build GraphQL query for team issues filtered by state ("Todo", "In Progress")
    - HTTP POST with `Authorization: <apiKey>` header (no "Bearer" prefix)
    - Parse JSON response into `linearIssue` structs
    - Return mapped results

- [ ] Create `server.go` implementing TaskSourceService
  - Files: `plugins/bossd-plugin-linear/server.go`
  - Pattern: Mirror `plugins/bossd-plugin-dependabot/server.go`
  - `GetInfo()`: returns name="linear", capabilities=["task_source"]
  - `PollTasks()`: return empty (Linear plugin is user-initiated, not polled)
  - `UpdateTaskStatus()`: log and return empty
  - `ListAvailableIssues(req)`:
    1. Extract `linear_api_key` and `linear_team_key` from `req.Config`
    2. Create `linearClient`, call `FetchIssues()`
    3. Call `s.host.ListOpenPRs(req.RepoOriginUrl)` for PR matching
    4. For each issue, call `matchPR(issue, prs)` to find existing PRs
    5. Return `[]*bossanovav1.TrackerIssue`

- [ ] Create `github.go` with host client (eager dial pattern) and PR matching
  - Files: `plugins/bossd-plugin-linear/github.go`
  - Pattern: Mirror `plugins/bossd-plugin-dependabot/github.go` (eagerHostServiceClient)
  - Add `matchPR(issue linearIssue, prs []*bossanovav1.PRSummary) (int32, string)`:
    ```go
    func matchPR(issue linearIssue, prs []*bossanovav1.PRSummary) (prNumber int32, branch string) {
        // Primary: branch name match
        for _, pr := range prs {
            if pr.HeadBranch == issue.BranchName {
                return pr.Number, pr.HeadBranch
            }
        }
        // Fallback: title contains [ENG-123]
        tag := "[" + issue.Identifier + "]"
        for _, pr := range prs {
            if strings.Contains(pr.Title, tag) {
                return pr.Number, pr.HeadBranch
            }
        }
        return 0, ""
    }
    ```

- [ ] Create `go.mod` and add Makefile target
  - Files: `plugins/bossd-plugin-linear/go.mod`, `Makefile`
  - go.mod: module `github.com/recurser/bossd-plugin-linear`, require `bossalib`, `go-plugin`, `zerolog`, `grpc`
  - Makefile: add `build-linear` target mirroring `build-dependabot`

- [ ] Write unit tests for GraphQL response parsing and PR matching
  - Files: `plugins/bossd-plugin-linear/linear_test.go`, `plugins/bossd-plugin-linear/github_test.go`
  - **GraphQL tests** (~8):
    - `TestParseIssuesResponse_Success` — valid JSON → correct issue structs
    - `TestParseIssuesResponse_Empty` — empty nodes → empty slice
    - `TestParseIssuesResponse_MalformedJSON` — invalid JSON → error
    - `TestParseIssuesResponse_MissingFields` — partial data → zero values
    - `TestParseIssuesResponse_AuthError` — 401 response → meaningful error
    - `TestParseIssuesResponse_NetworkError` — connection refused → error
    - `TestLinearClient_SetsCorrectHeaders` — verify Authorization header
    - `TestLinearClient_UsesCorrectEndpoint` — verify URL
  - **PR matching tests** (~6):
    - `TestMatchPR_BranchMatch` — branch matches → returns PR number + branch
    - `TestMatchPR_TitleMatch` — title contains `[ENG-123]` → returns PR number + branch
    - `TestMatchPR_BranchPreferredOverTitle` — both match different PRs → branch wins
    - `TestMatchPR_NoMatch` — no branch or title match → returns 0, ""
    - `TestMatchPR_EmptyPRs` — no open PRs → returns 0, ""
    - `TestMatchPR_CaseSensitiveIdentifier` — `[eng-123]` vs `[ENG-123]` → only exact match

### Post-Flight Checks for Flight Leg 5

- [ ] **Quality gates:** `go test ./plugins/bossd-plugin-linear/...` passes
- [ ] **Build:** `go build ./plugins/bossd-plugin-linear/` produces binary
- [ ] **PR matching:** Branch match preferred over title match in tests
- [ ] **Auth header:** Test verifies `Authorization: <key>` (no "Bearer" prefix)
- [ ] **Empty PollTasks:** `PollTasks()` returns empty response (user-initiated only)

### [HANDOFF] Review Flight Leg 5

Human reviews: GraphQL query correctness, PR matching logic, host client dial pattern, plugin registration.

---

## Flight Leg 6: Final Verification + Integration

### Tasks

- [ ] Run full test suite across all modules
  - Command: `make test`
  - All existing tests must still pass (no regressions)

- [ ] Run linter
  - Command: `make lint` (if available) or `golangci-lint run ./...`
  - No new lint errors

- [ ] Verify proto generation is clean
  - Command: `make generate && git diff --exit-code lib/bossalib/gen/`
  - Generated code matches committed code

- [ ] Write integration test for daemon ListTrackerIssues handler
  - Files: `services/bossd/internal/server/server_tracker_test.go` (new file)
  - Test:
    - Set up test DB with a repo that has `linear_api_key` and `linear_team_key`
    - Mock plugin host returning a stub TaskSource
    - Call `ListTrackerIssues` handler
    - Verify stub TaskSource received correct config map
    - Verify response contains mapped issues

- [ ] Verify build targets for all binaries
  - Command: `make build && make build-linear` (or equivalent)
  - All binaries compile: boss, bossd, bossd-plugin-dependabot, bossd-plugin-linear

### Post-Flight Checks for Final Verification

- [ ] **End-to-end flow:** Integration test passes — daemon correctly proxies ListTrackerIssues through plugin interface
- [ ] **Full test suite:** `make test` passes with 0 failures
- [ ] **All binaries build:** boss, bossd, bossd-plugin-linear compile cleanly
- [ ] **No dead code:** New proto messages and RPCs are used by handler, client, and tests
- [ ] **Proto clean:** `make generate` produces no diff

### [HANDOFF] Final Review

Human reviews: Complete feature before merge. Verify:

1. Repo settings: Linear API key masked (last 4 chars), team key editable
2. New session: "Work on a Linear ticket" appears only when configured
3. Issue picker: loading state, table, selection
4. Session creation: `[ENG-123] Title` format, existing PR attachment
5. Plugin: correct GraphQL query, PR matching, host client integration

---

## Rollback Plan

All changes are additive:

- Proto: new fields/messages only (no breaking changes to existing fields)
- DB: new columns with DEFAULT '' (no data migration needed)
- TUI: new rows/phases appended (existing rows unchanged)
- Plugin: new binary (no changes to existing plugins)

To rollback: revert the branch. No data migration needed since new columns default to empty strings.

## Notes

- **Field numbers locked:** Repo fields 15-16, UpdateRepoRequest fields 9-10, TrackerIssue fields 1-8
- **Plugin is private:** `plugins/bossd-plugin-linear/` must be added to the copy-and-strip exclusion list
- **No official Linear Go SDK:** Using raw `net/http` with GraphQL POST
- **Linear auth format:** `Authorization: <api_key>` (no "Bearer" prefix for personal API keys)
- **Encryption deferred:** API key encryption at rest tracked in TODOS.md
