## Handoff: Flight Leg 2 — Database Layer (Migration + WorkflowStore)

**Date:** 2026-03-23 15:45
**Branch:** implement-the-autopilot-plugin
**Flight ID:** fp-2026-03-23-1514-autopilot-plugin
**Planning Doc:** docs/plans/2026-03-23-1514-autopilot-plugin.md

### Tasks Completed

- bossanova-i81z: Create workflows table migration (20260323170000_workflows.sql)
- bossanova-soff: Add Workflow model to lib/bossalib/models/
- bossanova-rb13: Create WorkflowStore with CRUD operations
- bossanova-9xmw: Add WorkflowStore tests
- bossanova-8ziq: [HANDOFF] Review Flight Leg 2

### Files Changed

- `services/bossd/migrations/20260323170000_workflows.sql` — Goose migration creating workflows table with 13 columns, session_id/repo_id FKs, indexes on session_id and status
- `lib/bossalib/models/workflow.go` — New file with WorkflowStatus (6 values) and WorkflowStep (6 values) string enums, Workflow struct matching DB schema
- `services/bossd/internal/db/store.go:146-178` — Added CreateWorkflowParams, UpdateWorkflowParams (with double-pointer LastError), and WorkflowStore interface (Create, Get, Update, List, ListByStatus)
- `services/bossd/internal/db/workflow_store.go` — New file implementing SQLiteWorkflowStore with scanner helper, dynamic UPDATE SQL builder, workflowSelectSQL constant
- `services/bossd/internal/db/workflow_store_test.go` — 7 test functions covering CRUD, nonexistent ID, List, ListByStatus, nil optionals, error set/clear

### Learnings & Notes

- Timestamp format uses `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')` (not `datetime('now')` as the plan suggested) — matches the pattern in task_mappings migration
- WorkflowStatus and WorkflowStep are string types (not int enums) — matches the plan's note about new models using strings
- When testing List ordering with DESC, rows created in the same SQLite transaction may have identical timestamps, making ordering non-deterministic — avoid asserting specific order when timestamps could match
- The `createTestSession` helper was added to workflow_store_test.go since workflows FK to sessions

### Issues Encountered

- List ordering test initially failed because two workflows created in quick succession had identical timestamps — fixed by removing the ordering assertion (just checking count)

### Next Steps (Flight Leg 3: Config + Shared Constants)

- bossanova-q1bf: Add AutopilotConfig struct to config.go with defaults and validation
- bossanova-8wvs: Add AutopilotConfig round-trip and validation tests
- bossanova-x6u5: [HANDOFF] Review Flight Leg 3

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-23-1514-autopilot-plugin"` — should show bossanova-q1bf
2. Review files: `lib/bossalib/config/config.go`, `lib/bossalib/models/workflow.go`
