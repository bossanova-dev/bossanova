-- +goose Up
ALTER TABLE sessions RENAME COLUMN claude_session_id TO agent_session_id;
ALTER TABLE claude_chats RENAME COLUMN claude_id TO agent_session_id;
ALTER TABLE claude_chats RENAME TO agent_chats;
DROP INDEX IF EXISTS idx_claude_chats_session_id;
DROP INDEX IF EXISTS idx_claude_chats_claude_id;
CREATE INDEX idx_agent_chats_session_id ON agent_chats(session_id);
CREATE INDEX idx_agent_chats_agent_session_id ON agent_chats(agent_session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_chats_agent_session_id;
DROP INDEX IF EXISTS idx_agent_chats_session_id;
ALTER TABLE agent_chats RENAME TO claude_chats;
ALTER TABLE claude_chats RENAME COLUMN agent_session_id TO claude_id;
ALTER TABLE sessions RENAME COLUMN agent_session_id TO claude_session_id;
CREATE INDEX idx_claude_chats_session_id ON claude_chats(session_id);
CREATE INDEX idx_claude_chats_claude_id ON claude_chats(claude_id);
