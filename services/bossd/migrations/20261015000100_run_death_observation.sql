-- +goose Up
ALTER TABLE sessions ADD COLUMN run_dropped_at TEXT;
ALTER TABLE sessions ADD COLUMN run_dropped_agent_session_id TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN run_dropped_agent_session_id;
ALTER TABLE sessions DROP COLUMN run_dropped_at;
