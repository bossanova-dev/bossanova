-- +goose Up
-- When and why automation replaced a chat it owned (repair reclaiming an idle
-- build, repair or verify chat). It is kept apart from start_error, which means
-- the agent never came up: a replaced chat did run, and showing it as a failed
-- start sent operators hunting for a launch fault that never happened. NULL
-- means the chat was never replaced.
ALTER TABLE agent_chats ADD COLUMN reclaimed_at TEXT;
ALTER TABLE agent_chats ADD COLUMN reclaim_reason TEXT;

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN reclaim_reason;
ALTER TABLE agent_chats DROP COLUMN reclaimed_at;
