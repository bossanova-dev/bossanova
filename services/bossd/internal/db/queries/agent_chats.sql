-- name: CreateAgentChat :one
INSERT INTO agent_chats (id, session_id, agent_session_id, provider_session_id, agent_name, title, start_error, account_id, model, role, created_at)
VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?) RETURNING *;

-- name: GetAgentChatByAgentSessionID :one
SELECT * FROM agent_chats WHERE agent_session_id = ?;

-- name: ListAgentChatsBySession :many
SELECT * FROM agent_chats WHERE session_id = ? ORDER BY created_at DESC;

-- name: ListAgentChatsBySessions :many
SELECT * FROM agent_chats WHERE session_id IN (sqlc.slice('session_ids')) ORDER BY session_id, created_at DESC;

-- name: UpdateAgentChatTitle :exec
UPDATE agent_chats SET title = ? WHERE id = ?;

-- name: UpdateAgentChatTitleByAgentSessionID :exec
UPDATE agent_chats SET title = ? WHERE agent_session_id = ?;

-- name: UpdateAgentChatTmuxSessionName :execresult
UPDATE agent_chats SET tmux_session_name = sqlc.narg(name),
 start_error = CASE WHEN CAST(sqlc.narg(name) AS TEXT) IS NOT NULL THEN NULL ELSE start_error END,
 reclaimed_at = CASE WHEN CAST(sqlc.narg(name) AS TEXT) IS NOT NULL THEN NULL ELSE reclaimed_at END,
 reclaim_reason = CASE WHEN CAST(sqlc.narg(name) AS TEXT) IS NOT NULL THEN NULL ELSE reclaim_reason END
WHERE agent_session_id = sqlc.arg(agent_session_id);

-- name: SetAgentChatTmuxSessionNameClearedIf :execresult
UPDATE agent_chats SET tmux_session_name = NULL WHERE agent_session_id = ? AND tmux_session_name = ?;

-- name: UpdateAgentChatProviderSessionID :execresult
UPDATE agent_chats SET provider_session_id = ? WHERE agent_session_id = ?;

-- name: UpdateAgentChatAccountIDByAgentSessionID :exec
UPDATE agent_chats SET account_id = ? WHERE agent_session_id = ?;

-- name: UpdateAgentChatResumed :execresult
UPDATE agent_chats SET
 agent_session_id = COALESCE(sqlc.narg(new_agent_session_id), agent_session_id),
 session_id = COALESCE(sqlc.narg(session_id), session_id),
 title = COALESCE(sqlc.narg(title), title),
 agent_name = COALESCE(sqlc.narg(agent_name), agent_name),
 model = COALESCE(sqlc.narg(model), model),
 account_id = CASE WHEN CAST(sqlc.arg(set_account_id) AS BOOLEAN) THEN sqlc.narg(account_id) ELSE account_id END,
 provider_session_id = CASE WHEN CAST(sqlc.arg(set_provider_session_id) AS BOOLEAN) THEN sqlc.narg(provider_session_id) ELSE provider_session_id END,
 start_error = CASE WHEN CAST(sqlc.arg(clear_start_error) AS BOOLEAN) THEN NULL ELSE start_error END,
 reclaimed_at = CASE WHEN CAST(sqlc.arg(clear_start_error) AS BOOLEAN) THEN NULL ELSE reclaimed_at END,
 reclaim_reason = CASE WHEN CAST(sqlc.arg(clear_start_error) AS BOOLEAN) THEN NULL ELSE reclaim_reason END
WHERE agent_session_id = sqlc.arg(agent_session_id);

-- name: MarkAgentChatStartFailed :exec
UPDATE agent_chats SET start_error = ?, tmux_session_name = NULL WHERE agent_session_id = ?;

-- name: MarkAgentChatReclaimed :execresult
UPDATE agent_chats SET reclaimed_at = ?, reclaim_reason = ?, start_error = NULL, tmux_session_name = NULL WHERE agent_session_id = ?;

-- name: GetAgentChatReclaimedPR :one
SELECT s.pr_number, r.origin_url FROM agent_chats c JOIN sessions s ON s.id = c.session_id JOIN repos r ON r.id = s.repo_id WHERE c.agent_session_id = ?;

-- name: MarkAgentChatCallbacksCanceled :exec
UPDATE github_callbacks SET state = sqlc.arg(state), last_event = sqlc.narg(last_event), updated_at = sqlc.arg(updated_at)
WHERE target_chat_id = sqlc.arg(target_chat_id) AND state IN (sqlc.arg(active_state), sqlc.arg(leased_state), sqlc.arg(triggered_state))
AND NOT (trigger_event = sqlc.arg(merge_trigger) AND pr_number IS sqlc.narg(own_pr_number) AND repo_owner = sqlc.arg(own_owner) COLLATE NOCASE AND repo_name = sqlc.arg(own_name) COLLATE NOCASE);

-- name: DeleteAgentChatByAgentSessionID :exec
DELETE FROM agent_chats WHERE agent_session_id = ?;

-- name: DeleteAgentChatProxyTokensByAgentSessionID :exec
DELETE FROM proxy_tokens WHERE agent_session_id = ?;

-- name: ListAgentChatsWithTmuxSession :many
SELECT * FROM agent_chats WHERE tmux_session_name IS NOT NULL AND tmux_session_name != '' ORDER BY created_at DESC;

-- name: ListAgentChatsRoutable :many
SELECT * FROM agent_chats
WHERE (tmux_session_name IS NOT NULL AND tmux_session_name != '')
OR ((start_error IS NULL OR start_error = '') AND reclaimed_at IS NULL)
ORDER BY created_at DESC;
