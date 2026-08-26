-- +goose Up
-- Agent run cost telemetry is append-only operational history for the
-- daemon-owned agent runner boundary. A session can have many agent runs:
-- initial build, resumed chats, and repairs. Keeping rows separate preserves
-- per-run cost while still allowing session-level rollups.
CREATE TABLE agent_runs (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_session_id TEXT NOT NULL,
    agent_name TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    effort TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    stopped_at TEXT,
    stop_reason TEXT NOT NULL DEFAULT 'unknown' CHECK (stop_reason IN ('clean', 'stopped', 'usage_exhausted', 'rate_limited', 'daemon_restart', 'unknown')),
    parent_model_call_count INTEGER NOT NULL DEFAULT 0,
    child_model_call_count INTEGER NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    subagent_count INTEGER NOT NULL DEFAULT 0,
    direct_subagent_count INTEGER NOT NULL DEFAULT 0,
    output_token_count INTEGER,
    reasoning_token_count INTEGER,
    is_backfilled INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_agent_runs_session_id ON agent_runs(session_id);
CREATE UNIQUE INDEX idx_agent_runs_open_agent_session_id ON agent_runs(agent_session_id) WHERE stopped_at IS NULL;
CREATE INDEX idx_agent_runs_started_at ON agent_runs(started_at);
CREATE INDEX idx_agent_runs_is_backfilled ON agent_runs(is_backfilled);

-- Child spans record dispatched descendants. All descendants count toward
-- subagent_count, but run-cost parallelism uses only direct children so nested
-- serial work cannot inflate the parallelism ratio.
CREATE TABLE agent_run_children (
    id TEXT PRIMARY KEY,
    agent_run_id TEXT NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    agent_session_id TEXT NOT NULL DEFAULT '',
    parent_agent_id TEXT NOT NULL DEFAULT '',
    spawn_depth INTEGER NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    stopped_at TEXT,
    model_call_count INTEGER NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    output_token_count INTEGER,
    reasoning_token_count INTEGER,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_agent_run_children_agent_run_id ON agent_run_children(agent_run_id);
CREATE INDEX idx_agent_run_children_started_at ON agent_run_children(started_at);

-- +goose Down
DROP TABLE IF EXISTS agent_run_children;
DROP TABLE IF EXISTS agent_runs;
