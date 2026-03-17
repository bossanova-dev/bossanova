# Plan: Chat History Picker for Sessions

## Context

Currently, pressing Enter on a session immediately launches a bare `claude` command in the worktree. There's no way to resume a previous Claude Code conversation — each attach starts a fresh chat. Claude Code stores conversation history as JSONL files in `~/.claude/projects/{project-key}/`, and supports `--resume {UUID}` to continue a previous conversation. We should surface this in the TUI.

## New Flow

```
Home (session list) → [enter] → Chat Picker → [enter] → Launch Claude
                                   ↑ [esc]        (new or --resume)
                                   Home
```

When a user selects a session, a new **ChatPickerModel** view shows:
1. **"New chat"** option at top (always present)
2. Previous Claude Code conversations for that worktree, sorted most-recent-first
3. Each chat shows: first user message (truncated) + relative time (e.g. "2h ago")

## Implementation

### 1. New package: `services/boss/internal/claude/chats.go`

Chat discovery logic (pure I/O, no TUI concerns):

- `DiscoverChats(worktreePath string) ([]Chat, error)` — scans `~/.claude/projects/{key}/*.jsonl`
- Convert path to project key: `/Users/dave/foo` → `-Users-dave-foo`
- For each JSONL file: read first ~50 lines to extract `slug` and first user message text
- Sort by file mtime descending, cap at 20 results
- Return `[]Chat` with `UUID`, `Slug`, `Summary`, `ModifiedAt`
- Non-existent directory returns `nil, nil` (no error)

### 2. New view: `services/boss/internal/views/chatpicker.go`

**ChatPickerModel** — Bubbletea model following existing patterns:

- Fields: `client`, `ctx`, `sessionID`, `session`, `chats []claude.Chat`, `cursor`, `loading`, `err`, `cancel`, `width`, `height`
- `Init()` → fetch session via RPC (to get worktree path)
- On `sessionFetchedMsg` → run `DiscoverChats(session.WorktreePath)` as a tea.Cmd
- On `chatsDiscoveredMsg` → store chats, stop loading
- Navigation: j/k/arrows to move cursor, Enter to select, esc/q to cancel
- Cursor 0 = "New chat", cursor 1+ = previous chats
- On Enter: emit `switchViewMsg{view: ViewAttach, sessionID, resumeID}`
- `Cancelled() bool` — checked by App to return to home
- View: session title header, "New chat" row, "Previous chats:" subheader, chat rows with summary + relative time
- Helper: `relativeTime(t time.Time) string` → "just now", "3m ago", "2h ago", "5d ago", "2w ago"

### 3. Modify `services/boss/internal/views/app.go`

- Add `ViewChatPicker` to `View` enum
- Add `resumeID string` field to `switchViewMsg`
- Add `chatPicker ChatPickerModel` field to `App`
- Home Enter → `ViewChatPicker` (was `ViewAttach`)
- Route `ViewChatPicker` in Update: on cancel → `switchToHome()`
- Route `ViewAttach` creation: pass `msg.resumeID` to `NewAttachModel`
- Update `SetAttachSession(sessionID, resumeID string)` signature
- Propagate width/height to chatPicker (same pattern as home)
- Add ViewChatPicker to View() switch

### 4. Modify `services/boss/internal/views/attach.go`

- Add `resumeID string` field to `AttachModel`
- Update `NewAttachModel` to accept `resumeID string`
- In `sessionFetchedMsg` handler: if `resumeID != ""`, use `exec.Command("claude", "--resume", m.resumeID)` instead of bare `claude`
- Update comment about `--resume`

### 5. Modify `services/boss/internal/views/home.go`

- Change Enter key: emit `ViewChatPicker` instead of `ViewAttach`
- Update action bar: `[enter] chats` instead of `[enter] open`

### 6. Modify `services/boss/cmd/handlers.go`

- Update `runAttach` to call `app.SetAttachSession(sessionID, "")` (empty resumeID)

### 7. Also update `newsession.go` auto-attach path in `app.go`

- Where a newly created session auto-attaches: `NewAttachModel(a.client, a.ctx, sess.Id, "")` — empty resumeID for fresh session

## Files

| File | Type | Description |
|------|------|-------------|
| `services/boss/internal/claude/chats.go` | NEW | Chat discovery: scan JSONL files, parse metadata |
| `services/boss/internal/claude/chats_test.go` | NEW | Unit tests for DiscoverChats and parseSessionMeta |
| `services/boss/internal/views/chatpicker.go` | NEW | ChatPickerModel view |
| `services/boss/internal/views/app.go` | MODIFY | Add ViewChatPicker routing, resumeID plumbing |
| `services/boss/internal/views/attach.go` | MODIFY | Accept resumeID, pass `--resume` to claude |
| `services/boss/internal/views/home.go` | MODIFY | Enter → ViewChatPicker, update action bar |
| `services/boss/cmd/handlers.go` | MODIFY | Update SetAttachSession call |

## Verification

1. `go build ./services/boss/...` compiles
2. `go test ./services/boss/internal/claude/...` passes
3. Launch `boss`, select a session → see chat picker with "New chat" + any previous chats
4. Select "New chat" → launches bare `claude` in worktree
5. Press esc → returns to home
6. Select a previous chat → launches `claude --resume {UUID}` in worktree
7. After exiting Claude, returns to home screen (not chat picker)
