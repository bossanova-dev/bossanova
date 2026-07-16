-- +goose Up
-- Per-chat agent model id. Empty string means "inherit the agent CLI default"
-- (today: Opus for claude). bossd never enumerates valid models; the agent CLI
-- validates the name at runtime. Moves model authority from the session to the
-- chat (BOS-381), superseding the BOS-255 modelForChatAgent suppression gate.
ALTER TABLE agent_chats ADD COLUMN model TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN model;
