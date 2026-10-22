-- name: CreateWorkflow :one
INSERT INTO workflows (id, session_id, repo_id, plan_path, max_legs, start_commit_sha, config_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetWorkflow :one
SELECT * FROM workflows WHERE id = ?;

-- name: UpdateWorkflow :one
UPDATE workflows SET status = COALESCE(sqlc.narg(status), status),
 current_step = COALESCE(sqlc.narg(current_step), current_step),
 flight_leg = COALESCE(sqlc.narg(flight_leg), flight_leg),
 last_error = CASE WHEN CAST(sqlc.arg(set_last_error) AS BOOLEAN) THEN sqlc.narg(last_error) ELSE last_error END,
 updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: ListWorkflows :many
SELECT * FROM workflows ORDER BY created_at DESC;

-- name: ListWorkflowsByStatus :many
SELECT * FROM workflows WHERE status = ? ORDER BY created_at DESC;

-- name: ListWorkflowsByActiveSessionIDs :many
SELECT * FROM workflows WHERE session_id IN (sqlc.slice('session_ids'))
AND status IN ('pending', 'running', 'paused') ORDER BY created_at DESC;

-- name: MarkWorkflowsFailed :execrows
UPDATE workflows SET status = 'failed', last_error = 'daemon restarted', updated_at = ?
WHERE status IN ('running', 'pending');
