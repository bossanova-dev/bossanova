-- +goose Up
-- Persisted repair-attempt diagnostics for the TUI. Populated by the
-- repair plugin via host_service.RecordRepairOutcome so a 0-byte agent
-- log file (or any other failure mode) becomes visible without grep.
--
-- runner_error captures daemon-side StartAgentRun refusals (eg. claude
-- not on PATH); exit_error captures non-zero/signalled agent exits.
-- attempt_count grows monotonically so the TUI can render "(3×)".
ALTER TABLE sessions ADD COLUMN last_repair_started_at INTEGER;
ALTER TABLE sessions ADD COLUMN last_repair_runner_error TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_repair_exit_error TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_repair_attempt_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_repair_attempt_count;
ALTER TABLE sessions DROP COLUMN last_repair_exit_error;
ALTER TABLE sessions DROP COLUMN last_repair_runner_error;
ALTER TABLE sessions DROP COLUMN last_repair_started_at;
