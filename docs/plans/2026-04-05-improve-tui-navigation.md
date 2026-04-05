# Improve TUI Navigation

**Branch:** `improve-the-tui-navigation`
**Date:** 2026-04-05

## Problem

The TUI has 11 views with inconsistent navigation patterns. Action bars mix item-specific actions, screen navigation, and app-level commands in a flat list with no visual grouping. The `q` key means "quit" on Home but "back" everywhere else. Action label formats and destructive action naming vary between screens.

## Decisions Made

### 1. Grouped action bars with `·` separators

Action bars will visually separate three groups:

- **Item actions** (left): things you do to the selected row
- **Screen navigation** (middle): places you can go
- **Exit** (right): back/quit

Example for Home:

```
[enter] select  [h]istory  [a]rchive  ·  [n]ew  [p]ilot  [r]epos  [s]ettings  [t]rash  ·  [q]uit
```

Example for ChatPicker:

```
[enter] select  [d]elete  ·  [n]ew chat  [s]ettings  ·  [esc] back
```

### 2. Consistent q/esc behavior

- **Home screen only:** `q` quits the application
- **All sub-screens:** `esc` goes back. `q` is removed (no longer a back key on sub-screens)
- **Ctrl+C:** continues to force-quit from anywhere (unchanged)

This prevents accidental quits from deep screens.

### 3. Embedded label format as standard

Use `[letter]word` format when the key matches the first letter: `[n]ew`, `[a]rchive`, `[d]elete`, `[h]istory`, `[p]ilot`, `[r]epos`, `[s]ettings`, `[t]rash`, `[c]ancel`, `[p]ause`, `[r]esume`.

Fall back to `[key] word` for multi-char keys: `[enter] select`, `[esc] back`, `[y/enter] confirm`.

### 4. Standardized destructive action naming

- `[a]rchive` for recoverable soft-delete (Home screen)
- `[d]elete` for permanent removal (ChatPicker, RepoList, Trash)

### 5. Context-sensitive Autopilot actions

Action bar only shows valid actions for the selected workflow's current status:

| Workflow Status            | Actions Shown        |
| -------------------------- | -------------------- |
| running/pending            | `[p]ause  [c]ancel`  |
| paused                     | `[r]esume  [c]ancel` |
| completed/failed/cancelled | (no item actions)    |

### 6. Banner as single source of "where am I?"

Remove explicit `styleTitle` renders from Autopilot and Trash views. Instead, update the banner to show screen-specific context for all views:

| View            | Banner line1             | Banner line2            |
| --------------- | ------------------------ | ----------------------- |
| Home            | "Bossanova"              | "v{version}"            |
| ChatPicker      | PR title + link + status | worktree path           |
| SessionSettings | PR title + link + status | worktree path           |
| RepoSettings    | repo display name        | repo local path         |
| RepoList        | "Repositories"           | (subtle count or empty) |
| Autopilot       | "Autopilot"              | (subtle count or empty) |
| Trash           | "Archived Sessions"      | (subtle count or empty) |
| Settings        | "Settings"               | (empty)                 |
| NewSession      | "New Session"            | (empty)                 |
| RepoAdd         | "Add Repository"         | (empty)                 |

## Revised Action Bars (complete spec)

### Home (with sessions)

```
[enter] select  [h]istory  [a]rchive  ·  [n]ew  [p]ilot  [r]epos  [s]ettings  [t]rash  ·  [q]uit
```

### Home (empty, repos exist)

```
[n]ew session  [p]ilot  [r]epos  [s]ettings  [t]rash  ·  [q]uit
```

### Home (empty, no repos)

```
[n]ew session  [r]epos  [s]ettings  ·  [q]uit
```

### ChatPicker (with chats)

```
[enter] select  [d]elete  ·  [n]ew chat  [s]ettings  ·  [esc] back
```

### ChatPicker (empty)

```
[n]ew chat  [s]ettings  ·  [esc] back
```

### Autopilot (running/pending selected)

```
[p]ause  [c]ancel  ·  [esc] back
```

### Autopilot (paused selected)

```
[r]esume  [c]ancel  ·  [esc] back
```

### Autopilot (terminal state selected, or empty)

```
[esc] back
```

### RepoList (with repos)

```
[enter] settings  [d]elete  ·  [a]dd  ·  [esc] back
```

### RepoList (empty)

```
[a]dd  ·  [esc] back
```

### Trash (with sessions)

```
[d]elete  [a] delete all  [r]estore  ·  [esc] back
```

### Trash (empty)

```
[esc] back
```

### Settings / RepoSettings / SessionSettings (navigation mode)

```
[enter/space] toggle/edit  ·  [esc] back
```

### Settings / RepoSettings / SessionSettings (editing mode)

```
[enter] save  [esc] cancel
```

### Confirmation dialogs (all screens, unchanged)

```
[y/enter] confirm  [n/esc] cancel
```

### NewSession / RepoAdd (table phases)

```
[enter] select  ·  [esc] back
```

## Implementation Plan

### Step 1: Add shared action bar helper to theme.go

Add `actionBarSeparator = " · "` constant and a shared `actionBar(groups ...[]string) string` helper function to `theme.go`. The helper takes groups of action strings, joins items within each group with double-space, then joins groups with `" · "`, and wraps in `styleActionBar.Render()`. All views will use this helper instead of hand-building action bar strings.

Also move `renderBanner()`, `bannerOpts`, `bannerGradient`, and `bannerHeight` from `home.go` to `theme.go` so the banner system lives alongside other shared rendering infrastructure.

### Step 2: Update banner to show screen context

In `app.go`, update the banner opts in `App.View()` to pass screen-specific line1/line2 for all views:

| View       | Banner line1            | Banner line2            |
| ---------- | ----------------------- | ----------------------- |
| RepoList   | "Repositories"          | (subtle count or empty) |
| Autopilot  | "Autopilot"             | (subtle count or empty) |
| Trash      | "Archived Sessions"     | (subtle count or empty) |
| Settings   | "Settings"              | (empty)                 |
| NewSession | "New Session"           | (empty)                 |
| RepoAdd    | "Add Repository"        | (empty)                 |
| Attach     | session title or branch | worktree path           |

Home, ChatPicker, SessionSettings, and RepoSettings already have banner opts.

### Step 3: Remove explicit titles from all views

Remove `styleTitle.Render(...)` calls from:

- `autopilot_view.go` ("Autopilot Workflows")
- `trash.go` ("Archived Sessions")
- `settings.go` ("Settings")

The banner now handles "where am I?" for all views.

Also fix `tableHeight()` overhead in `autopilot_view.go` and `trash.go`: change `bannerOverhead + 4` to `bannerOverhead + 2` since the title line and its trailing blank are removed (only blank + action bar remain).

### Step 4: Update Home action bars

- Use the new `actionBar()` helper to group actions with `·` separator in `home.go` View()
- Keep `q` as quit (Home only)

### Step 5: Update sub-screen action bars and key handlers

For each sub-screen view file:

- Use the new `actionBar()` helper for grouped format
- Remove `q` from key handlers (only `esc` goes back)
- Standardize label format to embedded style
- Change "remove" to "delete" in both action bar labels AND confirmation dialog text

Files to modify (all files with `case "esc", "q":`):

- `chatpicker.go` (also change confirmation "Remove %q?" to "Delete %q?")
- `autopilot_view.go`
- `repo_list.go` (also change confirmation "Remove %q?" to "Delete %q?", remove `s` key handler)
- `trash.go`
- `settings.go`
- `repo_settings.go`
- `session_settings.go`
- `newsession.go` (line 416, repoSelect phase only)
- `attach.go`

Note: `repo_add.go` does NOT have a `q` handler, so no key handler change needed there, but its action bar should still use the new helper.

### Step 6: Make Autopilot actions context-sensitive

In `autopilot_view.go`:

- Update `View()` to build the action bar based on `selectedWorkflow().Status`
- Show only valid actions for the current workflow state
- Use the `actionBar()` helper for all states

### Step 7: Update RepoList

- Change `[s/enter] settings` to `[enter] settings`
- Remove `s` key handler from Update() (only `enter` opens settings)

### Step 8: Tests (unit + integration)

**Unit tests:**

- Update `autopilot_view_test.go` for context-sensitive action bar changes
- Add tests for the `actionBar()` helper in a new test or in `theme_test.go`
- Add q-quit regression test to `home_test.go` (verify q only quits from Home)

**Integration tests (tuitest/):**

- `navigation_test.go`: Change `SendKey('q')` to `SendEscape()` for RepoList back-navigation (lines 56, 109)
- `navigation_test.go`: Update `WaitForText` strings that check for removed title text ("Archived Sessions" at line 39, "Autopilot Workflows" at line 122)
- Review all tuitest files for any other q-back or title text assertions that need updating:
  - `trash_test.go`, `settings_test.go`, `repolist_test.go`, `chatpicker_test.go`, `autopilot_test.go`
- Verify all integration tests pass after changes

## NOT in scope

- **Narrow terminal handling:** Action bars may wrap at < 100 columns. Low priority for a developer CLI tool. (Captured in TODOS.md)
- **Keyboard shortcut help overlay (? key):** Would be nice but not part of this navigation cleanup. (Captured in TODOS.md)
- **Redesigning the wizard flows (NewSession, RepoAdd):** Their internal navigation is fine. Wizard phase-specific headers are preserved via existing internal logic.
- **Autopilot empty state warmth:** "No workflows." is serviceable. Can be improved separately.

## What already exists

- `styleActionBar` in `theme.go` (faint style, consistent padding) - reuse as-is
- Confirmation dialog pattern (`[y/enter] confirm  [n/esc] cancel`) - unchanged
- `cursorChevron`, `newBossTable()`, `updateCursorColumn()` - unchanged
- Banner system with `renderBanner()` and `bannerOpts` in `home.go` - move to `theme.go` and extend
- `clampedTableHeight()` helper in `theme.go` - reuse, but callers need overhead adjustment

## Failure modes

| Codepath                    | Failure mode                                   | Test coverage                        | Error handling              | User experience                        |
| --------------------------- | ---------------------------------------------- | ------------------------------------ | --------------------------- | -------------------------------------- |
| q-key on sub-screen         | User presses q expecting back, nothing happens | Covered by tuitest updates           | N/A (key ignored)           | Momentary confusion, esc works         |
| tableHeight off by 2        | Title removed but overhead not adjusted        | Covered by visual tuitest            | No crash, just wrong height | 2 blank rows at bottom of table        |
| actionBar helper            | Bad group separator rendering                  | Covered by new unit test             | N/A                         | Garbled action bar text                |
| Banner missing view         | New view added later without banner opts       | No test (future work)                | Falls through to default    | Shows "Bossanova" instead of view name |
| Context-sensitive autopilot | Selected workflow nil/missing                  | Covered by existing empty-state test | Falls to "no item actions"  | Shows only [esc] back                  |

No critical gaps: all failure modes have either test coverage or safe fallback behavior.

## Worktree parallelization strategy

Sequential implementation, no parallelization opportunity. All steps touch the same `views/` package and share the action bar helper introduced in Step 1. Steps 2-8 all depend on Step 1.

## GSTACK REVIEW REPORT

| Review        | Trigger               | Why                             | Runs | Status | Findings                         |
| ------------- | --------------------- | ------------------------------- | ---- | ------ | -------------------------------- |
| CEO Review    | `/plan-ceo-review`    | Scope & strategy                | 0    | --     | --                               |
| Codex Review  | `/codex review`       | Independent 2nd opinion         | 0    | --     | --                               |
| Eng Review    | `/plan-eng-review`    | Architecture & tests (required) | 1    | CLEAN  | 8 issues, 0 critical gaps        |
| Design Review | `/plan-design-review` | UI/UX gaps                      | 1    | CLEAN  | score: 5/10 -> 8/10, 6 decisions |

- **UNRESOLVED:** 0
- **VERDICT:** DESIGN + ENG CLEARED. Ready to implement.
