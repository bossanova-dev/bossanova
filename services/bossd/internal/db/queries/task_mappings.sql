-- name: CreateTaskMapping :one
INSERT INTO task_mappings (id, external_id, plugin_name, repo_id, status, retry_count, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetTaskMapping :one
SELECT * FROM task_mappings WHERE id = ?;

-- name: GetTaskMappingByExternalID :one
SELECT * FROM task_mappings WHERE external_id = ?;

-- name: GetTaskMappingBySessionID :one
SELECT * FROM task_mappings WHERE session_id = ?;

-- name: UpdateTaskMapping :one
UPDATE task_mappings SET
 session_id = CASE WHEN CAST(sqlc.arg(set_session_id) AS BOOLEAN) THEN sqlc.narg(session_id) ELSE session_id END,
 status = COALESCE(sqlc.narg(status), status),
 last_error = CASE WHEN CAST(sqlc.arg(set_last_error) AS BOOLEAN) THEN sqlc.narg(last_error) ELSE last_error END,
 pending_update_status = CASE WHEN CAST(sqlc.arg(set_pending_update_status) AS BOOLEAN) THEN sqlc.narg(pending_update_status) ELSE pending_update_status END,
 pending_update_details = CASE WHEN CAST(sqlc.arg(set_pending_update_details) AS BOOLEAN) THEN sqlc.narg(pending_update_details) ELSE pending_update_details END,
 retry_count = COALESCE(sqlc.narg(retry_count), retry_count), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteTaskMapping :execrows
DELETE FROM task_mappings WHERE id = ?;

-- name: ListTaskMappingsByPendingUpdateStatus :many
SELECT * FROM task_mappings WHERE pending_update_status IS NOT NULL ORDER BY created_at ASC;

-- name: ListTaskMappingsByRecentFailure :many
SELECT * FROM task_mappings WHERE status = ?
AND updated_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-24 hours')
ORDER BY updated_at DESC, rowid DESC LIMIT ?;

-- name: MarkTaskMappingsOrphaned :execrows
UPDATE task_mappings SET status = ?, updated_at = ? WHERE status IN (?, ?);
