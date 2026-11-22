-- +goose Up
-- Per-chat reasoning effort. Empty string means "inherit": the session's
-- effort when the chat runs the session's agent, else that agent's configured
-- default. bossd never enumerates valid efforts; the agent CLI validates it.
ALTER TABLE agent_chats ADD COLUMN effort TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN effort;
