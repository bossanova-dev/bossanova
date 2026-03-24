# Setup Command in Repo Settings Form — Implementation Plan

**Flight ID:** fp-2026-03-24-1120-setup-script-in-repo-settings

## Overview

Allow the setup command to be viewed and edited in the TUI repo settings form. The underlying `setup_script` field already exists in the database, domain model, and protobuf Repo message, but is missing from the `UpdateRepoRequest` protobuf message, the server handler, and the TUI settings view. This plan threads the field through all three layers. The user-facing label will read "Setup command" (not "setup script") since the value may be any shell command (e.g. `make setup`, `npm install`, `./setup.sh`).

## Affected Areas

- [ ] `proto/bossanova/v1/daemon.proto` — Add `setup_script` to `UpdateRepoRequest`
- [ ] `lib/bossalib/gen/` — Regenerated protobuf Go code
- [ ] `services/bossd/internal/server/server.go` — Handle `setup_script` in `UpdateRepo` handler
- [ ] `services/boss/internal/views/repo_settings.go` — Add "Setup command" row with inline editing

## Design References

- TUI design skill: `.claude/skills/tui-design/SKILL.md` — cursor/select pattern, inline editing
- Existing inline edit pattern: `repo_settings.go:148-176` (name editing with `textinput.Model`)
- Existing setup command input: `repo_add.go:206-209` (placeholder "Optional, e.g. make setup")
- Proto optional string pattern: `RegisterRepoRequest.setup_script` (field 5)
- Server handler pattern: `server.go:306-339` (UpdateRepo with nil checks)
- DB double-pointer pattern: `store.go:25` (`SetupScript **string`)

### Naming Note

The proto field, DB column, and Go struct field all remain `setup_script` / `SetupScript` — no schema changes. Only the **user-facing TUI label** changes to "Setup command". The repo add form (`repo_add.go`) placeholder should also be updated from "Optional, e.g. ./setup.sh" to "Optional, e.g. make setup" for consistency.

---

## Flight Leg 1: Proto + Server — Wire setup_script through UpdateRepo

### Tasks

- [ ] Add `optional string setup_script = 8` to `UpdateRepoRequest` in `proto/bossanova/v1/daemon.proto`
  - File: `proto/bossanova/v1/daemon.proto:123-131`
  - Pattern: Follow existing optional fields (e.g. `display_name` field 2, `merge_strategy` field 7)
  - Next available field number is 8
- [ ] Run `make generate` to regenerate Go protobuf code
  - This regenerates `lib/bossalib/gen/bossanova/v1/daemon.pb.go`
  - The generated `UpdateRepoRequest` struct will gain `SetupScript *string`
- [ ] Add setup_script handling to `UpdateRepo` server handler
  - File: `services/bossd/internal/server/server.go:306-339`
  - Pattern: Follow the existing nil-check pattern used for other optional fields
  - The `SetupScript` field on `UpdateRepoRequest` is `*string` (single pointer from proto optional)
  - The `SetupScript` field on `db.UpdateRepoParams` is `**string` (double pointer: nil=don't update, \*nil=set NULL)
  - When `msg.SetupScript != nil`: set `params.SetupScript` to `&msg.SetupScript` (address of the `*string`)
  - This correctly maps: proto `*string` → db `**string` where the inner `*string` can be nil (clear) or a value (set)
  - Note: proto `optional string` when set to empty string `""` should clear the setup command (set to NULL), since an empty setup command is meaningless

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make format && make generate && make build` — all pass (build proves proto + server compile)
- [ ] **Proto field exists:** `grep "setup_script" proto/bossanova/v1/daemon.proto` shows the field in UpdateRepoRequest
- [ ] **Generated code compiles:** `make build` succeeds, confirming the generated Go code is valid
- [ ] **Server handler updated:** `grep "SetupScript" services/bossd/internal/server/server.go` shows the new handling code
- [ ] **Tests pass:** `make test` — all existing tests continue to pass

### [HANDOFF] Review Flight Leg 1

Human reviews: Proto definition, server handler logic, and the double-pointer mapping from proto optional to db params.

---

## Flight Leg 2: TUI Settings Form — Add "Setup command" row

### Tasks

- [ ] Add `repoSettingsRowSetupScript` constant and update row numbering
  - File: `services/boss/internal/views/repo_settings.go:27-35`
  - Insert `repoSettingsRowSetupScript = 1` (after Name, before MergeStrategy)
  - Renumber all subsequent rows (+1 each)
  - Update `repoSettingsRowCount` to 7
  - Rationale: Setup command is a text property like Name, so it belongs in the "text fields" section before the merge strategy and checkboxes
- [ ] Add `setupInput textinput.Model` field and `editingField` tracking to the model
  - File: `services/boss/internal/views/repo_settings.go:52-68`
  - Replace `editing bool` with `editingField int` (-1 = not editing) to track which row is being edited
  - Add `setupInput textinput.Model` alongside existing `nameInput`
  - Initialize in `NewRepoSettingsModel`: placeholder "Optional, e.g. make setup", width 60
- [ ] Update `Init` to set `setupInput` value from loaded repo
  - File: `services/boss/internal/views/repo_settings.go:105-112`
  - In `repoSettingsLoadedMsg` handler: `m.setupInput.SetValue(m.repo.GetSetupScript())`
- [ ] Update `updateEditing` to handle both name and setup command inputs
  - File: `services/boss/internal/views/repo_settings.go:148-176`
  - On enter: check `m.editingField` to determine which field to save
  - For setup command: trim value; if empty, send `SetupScript: proto.String("")` (or nil) to clear; if non-empty, send the value
  - On esc: restore original value from `m.repo` for whichever field was being edited
  - Route key events to the correct `textinput.Model` based on `editingField`
- [ ] Update `activateRow` to handle setup command editing
  - File: `services/boss/internal/views/repo_settings.go:178-232`
  - Add `case repoSettingsRowSetupScript:` — set `editingField`, focus `setupInput`
  - Update `case repoSettingsRowName:` to set `editingField = repoSettingsRowName`
- [ ] Update `View` to render the setup command row
  - File: `services/boss/internal/views/repo_settings.go:247-336`
  - Add setup command rendering between Name and Merge strategy rows
  - When editing: show label + text input (same pattern as Name editing)
  - When not editing: show `Setup command: <value>` or `Setup command: (none)` if empty/nil
  - Use same padding and cursor pattern as Name row
- [ ] Update repo add form placeholder to match
  - File: `services/boss/internal/views/repo_add.go:206-209`
  - Change `Title` from "Setup script" to "Setup command"
  - Change placeholder from "Optional, e.g. ./setup.sh" to "Optional, e.g. make setup"

### Post-Flight Checks for Flight Leg 2

- [ ] **Quality gates:** `make format && make build` — all pass
- [ ] **Row constants correct:** `grep -n "repoSettingsRow" services/boss/internal/views/repo_settings.go` shows 7 rows with correct numbering
- [ ] **Text inputs initialized:** `grep "setupInput\|setupScript\|SetupScript" services/boss/internal/views/repo_settings.go` shows the new field wired through model, init, editing, activation, and view
- [ ] **Label consistency:** `grep -n "Setup command\|Setup script" services/boss/internal/views/` confirms user-facing labels all say "Setup command"
- [ ] **Tests pass:** `make test` — all existing tests continue to pass

### [HANDOFF] Review Flight Leg 2

Human reviews: TUI layout, editing behavior, label consistency ("Setup command" everywhere user-facing), and the overall user experience.

---

## Flight Leg 3: Final Verification

### Tasks

- [ ] Run full quality gates: `make format && make lint && make test`
- [ ] Run full build: `make`
- [ ] Verify no unused exports or dead code introduced

### Post-Flight Checks for Final Verification

- [ ] **End-to-end build:** `make` completes successfully (clean + generate + format + build)
- [ ] **Lint clean:** `make lint` passes with no new errors
- [ ] **All tests pass:** `make test` passes across all modules
- [ ] **Proto consistency:** `buf lint` passes (run via `make lint-proto`)

### [HANDOFF] Final Review

Human reviews: Complete feature before merge.

---

## Rollback Plan

All changes are confined to:

1. One proto field addition (additive, non-breaking)
2. One server handler clause (additive)
3. One TUI view enhancement (additive)
4. One label change in repo add form (cosmetic)

Revert the branch to roll back all changes.

## Notes

- The `UpdateRepoRequest.setup_script` is field number 8 — next available after `merge_strategy` (7)
- The DB store already supports `SetupScript **string` in `UpdateRepoParams`, so no DB changes needed
- The `repoToProto` conversion already handles `SetupScript` (in `convert.go`), so responses will include the value automatically
- Empty string from the TUI should clear the setup command (set DB to NULL) — matching the semantics of "no setup command configured"
- The `editing bool` in the current model only supports one editable text field; replacing with `editingField int` properly supports multiple inline text editors
- Proto/DB/Go field names stay as `setup_script` / `SetupScript` — only user-facing TUI labels change to "Setup command"
- The repo add form (`repo_add.go`) label and placeholder are also updated for consistency
