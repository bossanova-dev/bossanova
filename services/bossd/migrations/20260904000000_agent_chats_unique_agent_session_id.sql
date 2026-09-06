-- +goose Up
-- BOS-1143: make agent_chats.agent_session_id UNIQUE.
--
-- agent_session_id is bossd's durable correlation key for a chat: resume,
-- routing, and proxy-token lookups all address a chat by it and assume exactly
-- one row answers. The index created in 20260503170000_rename_claude_to_agent
-- was non-unique, so the resume path defended itself by deleting the prior row
-- and re-creating it -- which is what erased a chat's provider identity.
-- Making the column UNIQUE lets resume update the row in place instead.
--
-- This is deliberately fail-loud: no dedupe is performed here. If the index
-- fails to apply, duplicate rows exist and an operator must reconcile them by
-- hand first. List the candidate rows (not just a count) so they can be
-- compared side by side:
--   SELECT id, session_id, agent_session_id, agent_name, model, account_id,
--          provider_session_id, tmux_session_name, start_error, created_at
--     FROM agent_chats
--    WHERE agent_session_id IN (
--            SELECT agent_session_id FROM agent_chats
--             GROUP BY agent_session_id HAVING count(*) > 1)
--    ORDER BY agent_session_id, created_at;
--
-- How duplicates arise is not established -- the resume path this migration
-- replaces deleted and re-created inside one transaction, so it produced one
-- row, not two. Reconcile on the rows themselves: keep the RICHEST identity,
-- i.e. the row carrying a non-empty provider_session_id / account_id / model,
-- and drop the emptier sibling. Do not invert that rule: a row with those
-- columns unset is typically a failed-start or externally recorded chat, and
-- deleting the populated row destroys exactly the provider identity BOS-1143
-- exists to preserve.
DROP INDEX IF EXISTS idx_agent_chats_agent_session_id;

CREATE UNIQUE INDEX idx_agent_chats_agent_session_id
    ON agent_chats(agent_session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_chats_agent_session_id;

CREATE INDEX idx_agent_chats_agent_session_id
    ON agent_chats(agent_session_id);
