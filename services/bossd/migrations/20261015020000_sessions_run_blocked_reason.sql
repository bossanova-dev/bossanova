-- +goose Up
-- The reason an unattended run gave when it reported itself BLOCKED. It lives
-- in its own column, not in blocked_reason, because blocked_reason is shared
-- with the usage-limit park, the draft-PR markers and other writers that would
-- overwrite the report before finalize reads it. NULL means the run made no
-- report. Finalize consumes it, and a resurrected session starts without it.
ALTER TABLE sessions ADD COLUMN run_blocked_reason TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN run_blocked_reason;
