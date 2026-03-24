# Expose Autopilot Claude Chats in CLI Session View

**Flight ID:** fp-2026-03-24-1846-autopilot-chats-in-session-view

## Overview

When the autopilot runs, each Claude subprocess it spawns should be registered as a chat in the boss database, linked to the workflow's session. This makes autopilot Claude conversations visible in the chat picker and attachable from the boss CLI — just like manually created chats.

## The Gap

Currently, `HostServiceServer.CreateAttempt()` calls `s.claude.Start()` which spawns a Claude process and returns a daemon-internal session ID (e.g., `claude-1711234567890`). This ID is never registered via `RecordChat()`, so autopilot chats are invisible in the session's chat list.

In contrast, the interactive attach view (`attach.go:99-107`) generates a UUID, calls `RecordChat()`, then passes `--session-id` to Claude — making the chat trackable.

## Design Decision: Session ID Strategy

The autopilot uses `claude --print --output-format stream-json` (headless mode), not interactive mode. The daemon assigns an internal ID (`claude-<timestamp>`) rather than a Claude Code session UUID. Two approaches:

**Option A: Record with daemon session ID as claude_id** — Use the `claude-<timestamp>` ID returned by `Start()` as the `claude_id` in `claude_chats`. The chat picker already supports showing daemon-reported statuses for chats. Resuming these chats with `--resume` won't work (since there's no real Claude Code session file), but that's acceptable for headless autopilot runs. The chat would show status (working/idle/stopped) and output can be streamed via `boss autopilot status --follow`.

**Option B: Pre-generate a UUID and pass `--session-id`** — Change `ClaudeRunner.Start()` to accept an optional `--session-id` argument, generate a UUID before starting, record it via `RecordChat()`, and use that UUID. This would make autopilot chats resumable and produce real Claude Code JSONL session files that the title backfill can read.

**Chosen: Option B** — This is the cleaner approach. It makes autopilot chats first-class citizens: they have real Claude Code session IDs, their JSONL files exist for title backfill, and they could theoretically be resumed. The change to `ClaudeRunner.Start()` is minimal (add an optional session ID parameter).

## Affected Areas

- [ ] `services/bossd/internal/claude/runner.go` — Add optional session ID parameter to `Start()`
- [ ] `services/bossd/internal/plugin/host_service.go` — Record chats when creating attempts
- [ ] `services/bossd/internal/plugin/host.go` — Thread chat store through `SetWorkflowDeps()`
- [ ] `services/bossd/cmd/main.go` — Pass `claudeChats` to `SetWorkflowDeps()`
- [ ] `services/bossd/internal/claude/runner_test.go` — Update tests for new Start signature
- [ ] `services/bossd/internal/plugin/host_service_test.go` — Add chat registration tests

## Eng Review Decisions

- **Proto change:** REMOVED — `CreateAttemptResponse` does NOT need `claude_id`. The orchestrator never uses it.
- **API design:** Simple parameter addition to `Start()`, not a `StartOptions` struct.
- **Retry behavior:** Each retry creates a separate chat record (separate Claude processes, separate JSONL files).
- **Edge case:** Only register chat when `workflowID` is present. Bare `CreateAttempt` calls skip registration.
- **Error policy:** Chat registration is best-effort. If `claudeChats.Create()` fails, log the error and continue starting Claude.

## Design References

- Existing chat creation pattern: `services/boss/internal/views/attach.go:99-107`
- ClaudeRunner interface: `services/bossd/internal/claude/runner.go:31-54`
- HostServiceServer.CreateAttempt: `services/bossd/internal/plugin/host_service.go:331-357`
- Chat picker display: `services/boss/internal/views/chatpicker.go`

---

## Flight Leg 1: Extend ClaudeRunner to Accept a Session ID

### Tasks

- [ ] Add `sessionID` parameter to `ClaudeRunner.Start()` interface
  - Files: `services/bossd/internal/claude/runner.go:31-35`
  - Change signature to: `Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error)`
  - When `sessionID` is non-empty, pass `--session-id <sessionID>` to `claude` CLI args
  - When `sessionID` is non-empty, use it as the process tracking key instead of generating `claude-<timestamp>`
  - When `sessionID` is empty, preserve existing behavior (generate `claude-<timestamp>`)
  - Pattern: Follow existing `resume` parameter handling at lines 131-134
- [ ] Update `Runner.Start()` implementation with session ID support
  - Files: `services/bossd/internal/claude/runner.go:119-227`
  - Accept the new parameter, add `--session-id` to args when provided
  - Use `sessionID` as the map key (replacing `claude-<timestamp>`) when provided
- [ ] Update all callers of `ClaudeRunner.Start()` to pass the new parameter
  - Files: `services/bossd/internal/plugin/host_service.go:349`, any test files
  - Pass `""` for existing callers that don't need a specific session ID
- [ ] Update tests for the new signature
  - Files: `services/bossd/internal/claude/runner_test.go`
  - Ensure existing tests pass with `""` session ID
  - Add a test that verifies `--session-id` is passed when a session ID is provided

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make format && make test` — all pass
- [ ] **Interface compliance:** Verify `Runner` still satisfies `ClaudeRunner` interface (compilation check)
  - How to test: `cd services/bossd && go build ./...`
  - Fail if: compilation errors
- [ ] **Behavioral check:** Verify `--session-id` appears in command args when session ID is provided
  - How to test: Run tests in `services/bossd/internal/claude/`
  - Expected: New test passes, existing tests unchanged

### [HANDOFF] Review Flight Leg 1

Human reviews: ClaudeRunner interface change, backward compatibility with existing callers

---

## Flight Leg 2: Record Autopilot Chats in CreateAttempt

### Tasks

- [ ] Wire `ClaudeChatStore` into `HostServiceServer`
  - Files: `services/bossd/internal/plugin/host_service.go:20-41`, `host.go:215-221`, `services/bossd/cmd/main.go:114`
  - Add `claudeChats db.ClaudeChatStore` field to `HostServiceServer` struct
  - Update `SetWorkflowDeps()` to accept and store the chat store (both on HostServiceServer and Host)
  - Update `main.go:114` to pass `claudeChats` to `pluginHost.SetWorkflowDeps()`
- [ ] Generate UUID and record chat in `CreateAttempt` (best-effort)
  - Files: `services/bossd/internal/plugin/host_service.go:331-357`
  - Only when `req.GetWorkflowId() != ""` and stores are available:
    1. Resolve workflow: `wf, err := s.workflowStore.Get(ctx, req.GetWorkflowId())`
    2. Generate a UUID: `chatID := uuid.New().String()`
    3. Call `s.claudeChats.Create()` with `SessionID: wf.SessionID, ClaudeID: chatID, Title: fmt.Sprintf("autopilot: %s", req.GetSkillName())`
    4. If chat creation fails, log the error and continue (best-effort)
    5. Pass `chatID` as the `sessionID` parameter to `s.claude.Start()`
  - When `workflowID` is empty, skip chat registration entirely (pass `""` to Start)
  - Add inline ASCII comment documenting the data flow above `CreateAttempt`
- [ ] Add 3 tests for chat registration in `host_service_test.go`
  - Test 1: `TestCreateAttemptRegistersChat` — CreateAttempt with workflowID creates a claude_chats record with correct session_id, claude_id, and "autopilot:" title prefix
  - Test 2: `TestCreateAttemptNoChatWithoutWorkflow` — CreateAttempt without workflowID does NOT create a chat record
  - Test 3: `TestCreateAttemptChatErrorBestEffort` — When claudeChats.Create() fails, CreateAttempt still succeeds (Claude is still started)

### Post-Flight Checks for Flight Leg 2

- [ ] **Quality gates:** `make format && make test` — all pass
- [ ] **Compilation:** `cd services/bossd && go build ./...` — no errors
- [ ] **Chat creation:** All 3 new tests pass
  - Expected: Chat record created with correct fields when workflow present, skipped otherwise, best-effort on error
  - Fail if: Any test fails

### [HANDOFF] Review Flight Leg 2

Human reviews: Chat registration logic, UUID generation, best-effort error handling, title format

---

## Flight Leg 3: Final Verification

### Tasks

- [ ] Run full test suite: `make test`
- [ ] Run linter: `make lint`
- [ ] Verify no unused exports or dead code
- [ ] Verify chat visibility: the chat picker (`chatpicker.go`) already calls `ListChats(sessionID)` which queries `claude_chats` by session ID — autopilot chats will appear automatically

### Post-Flight Checks for Final Verification

- [ ] **Full test suite:** `make test` — all pass
- [ ] **Lint:** `make lint` — no new errors
- [ ] **Build:** `cd services/bossd && go build ./...` and `cd services/boss && go build ./...` — all binaries compile

### [HANDOFF] Final Review

Human reviews: Complete feature before merge

---

## Rollback Plan

All changes are additive:

- The `sessionID` parameter to `Start()` is backward-compatible (empty string = old behavior)
- Chat records can be deleted from `claude_chats` table
- No schema migration needed (uses existing `claude_chats` table)

## Notes

- **Title strategy:** Autopilot chats are titled `autopilot: <step>` (e.g., "autopilot: implement"). The title backfill will overwrite this with the actual first user prompt from the JSONL file once Claude writes it.
- **Resumability:** Autopilot chats use `--session-id`, so they create real Claude Code session files. However, the chat picker's resume feature passes `--resume` which enters interactive mode — this is a different mode than `--print`. Resuming an autopilot chat would switch from headless to interactive, which is actually a useful feature (inspect/continue work interactively).
- **Status display:** The chat picker already has daemon status support. Since autopilot chats run via `ClaudeRunner` (in the daemon), their working/stopped status will be visible if chat status reporting is wired up. This is an existing feature and doesn't need changes.
- **Existing `claude_session_id` on Session:** The `sessions` table has a `claude_session_id` field, but this is a 1:1 legacy field. The `claude_chats` table is the correct many-to-one relationship for this feature.

## GSTACK REVIEW REPORT

| Review        | Trigger               | Why                             | Runs | Status       | Findings                  |
| ------------- | --------------------- | ------------------------------- | ---- | ------------ | ------------------------- |
| CEO Review    | `/plan-ceo-review`    | Scope & strategy                | 0    | —            | —                         |
| Codex Review  | `/codex review`       | Independent 2nd opinion         | 0    | —            | —                         |
| Eng Review    | `/plan-eng-review`    | Architecture & tests (required) | 1    | CLEAR (PLAN) | 3 issues, 0 critical gaps |
| Design Review | `/plan-design-review` | UI/UX gaps                      | 0    | —            | —                         |

- **UNRESOLVED:** 0
- **VERDICT:** ENG CLEARED — ready to implement
