## Handoff: Flight Leg 1 — Proto + Server

**Date:** 2026-03-24 13:20
**Branch:** allow-the-setup-script-to-be-set-in-repo-settings-form
**Flight ID:** fp-2026-03-24-1120-setup-script-in-repo-settings
**Planning Doc:** docs/plans/2026-03-24-1120-setup-script-in-repo-settings.md

### Tasks Completed This Flight Leg

- bossanova-f8sa: Add `optional string setup_script = 8` to `UpdateRepoRequest` in daemon.proto
- bossanova-vocn: Run `make generate` to regenerate Go protobuf code
- bossanova-gufj: Add setup_script handling to `UpdateRepo` server handler

### Files Changed

- `proto/bossanova/v1/daemon.proto:154` — Added `optional string setup_script = 8` to `UpdateRepoRequest`
- `lib/bossalib/gen/bossanova/v1/daemon.pb.go` — Regenerated protobuf Go code (auto-generated)
- `services/bossd/internal/server/server.go:341-348` — Added SetupScript mapping in UpdateRepo handler

### Implementation Notes

- Proto field 8 added to `UpdateRepoRequest` as `optional string` — follows existing pattern of other optional fields
- Server handler maps proto `*string` to DB `**string` double-pointer pattern:
  - `nil` → don't update (field not sent)
  - `*""` (empty string) → set DB to NULL (clear the setup command)
  - `*"value"` → set the value
- Used `new(*string)` to create `**string` pointing to nil for the "clear" case

### Current Status

- Format: pass
- Generate: pass
- Build: pass
- Tests: pass (all modules)

### Next Flight Leg

Flight Leg 2: TUI Settings Form — Add "Setup command" row

- bossanova-z5an: Add repoSettingsRowSetupScript constant, update row numbering and count
- bossanova-t5d7: Add setupInput field, replace editing bool with editingField int, init from loaded repo
- bossanova-ryoa: Update updateEditing and activateRow to handle setup command input
- bossanova-4kif: Update View to render setup command row with inline editing
- bossanova-pjls: Update repo add form label and placeholder to "Setup command"
- bossanova-zzqf: [HANDOFF] Review Flight Leg 2
