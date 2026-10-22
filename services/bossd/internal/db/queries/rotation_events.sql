-- name: CreateRotationEvent :exec
INSERT INTO rotation_events (id, session_id, chat_id, provider, trigger_kind, from_account, to_account, reset_at, outcome, detail, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListRotationEventsBySession :many
SELECT * FROM rotation_events WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: GetRotationEventByConfirmedAuthInvalidationSince :one
SELECT 1 FROM rotation_events
WHERE session_id = ? AND chat_id = ?
AND trigger_kind = 'ROTATION_TRIGGER_AUTH_INVALIDATED'
AND outcome NOT IN ('', 'ROTATION_OUTCOME_UNSPECIFIED', 'ROTATION_OUTCOME_STATUS_ONLY_DISABLED')
AND created_at >= ?
ORDER BY created_at DESC, id DESC LIMIT 1;
