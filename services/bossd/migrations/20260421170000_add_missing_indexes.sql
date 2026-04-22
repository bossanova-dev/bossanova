-- +goose Up
-- Add indexes that were missing from prior migrations. Each backs a WHERE
-- clause that would otherwise SCAN TABLE. Verified with EXPLAIN QUERY PLAN.

-- workflows.repo_id: cascades from repos FK + future per-repo workflow listings
CREATE INDEX IF NOT EXISTS idx_workflows_repo_id ON workflows(repo_id);

-- claude_chats.claude_id: GetByClaudeID / UpdateTitle / UpdateTmuxSessionName /
-- Delete all filter by claude_id (see claude_chat_store.go).
CREATE INDEX IF NOT EXISTS idx_claude_chats_claude_id ON claude_chats(claude_id);

-- +goose Down
DROP INDEX IF EXISTS idx_claude_chats_claude_id;
DROP INDEX IF EXISTS idx_workflows_repo_id;
