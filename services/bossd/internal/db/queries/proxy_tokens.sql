-- name: UpsertProxyToken :exec
INSERT INTO proxy_tokens (token_sha256, session_id, agent_session_id, account_id, is_chat_shaped, is_companion_shaped, created_at)
VALUES (sqlc.arg(token_sha256), sqlc.arg(session_id), sqlc.narg(agent_session_id), sqlc.narg(account_id), sqlc.arg(is_chat_shaped), sqlc.arg(is_companion_shaped), sqlc.arg(created_at))
ON CONFLICT(token_sha256) DO UPDATE SET
    session_id = excluded.session_id,
    agent_session_id = excluded.agent_session_id,
    account_id = excluded.account_id,
    is_chat_shaped = excluded.is_chat_shaped,
    is_companion_shaped = excluded.is_companion_shaped;

-- name: ListProxyTokens :many
SELECT * FROM proxy_tokens ORDER BY created_at, token_sha256;

-- name: GetProxyTokenByTokenHash :one
SELECT * FROM proxy_tokens WHERE token_sha256 = sqlc.arg(token_sha256);

-- name: DeleteProxyTokensBySessionID :exec
DELETE FROM proxy_tokens WHERE session_id = sqlc.arg(session_id);

-- name: DeleteProxyTokensByAgentSessionID :exec
DELETE FROM proxy_tokens WHERE agent_session_id = sqlc.arg(agent_session_id);

-- name: DeleteProxyTokensOlderThan :execresult
DELETE FROM proxy_tokens WHERE created_at < sqlc.arg(created_at);
