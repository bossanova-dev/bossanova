-- +goose Up
-- Run-efficiency telemetry (BOS-1107): make the epic's targets queryable rather
-- than anecdotal. `reviewer_dispatch_count` is how many reviewer subagents a run
-- dispatched; `terminal_state` is the boss-build terminal state the run ended in.
-- Both are additive defaulted columns, so existing rows read as "not recorded"
-- (0 / '') rather than being rewritten.
ALTER TABLE agent_runs ADD COLUMN reviewer_dispatch_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN terminal_state TEXT NOT NULL DEFAULT '' CHECK (terminal_state IN ('REVIEW_READY', 'PARTIAL', 'BLOCKED', 'NO_CHANGE', ''));

-- +goose Down
ALTER TABLE agent_runs DROP COLUMN terminal_state;
ALTER TABLE agent_runs DROP COLUMN reviewer_dispatch_count;
