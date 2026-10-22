-- name: CreateFixAttempt :one
INSERT INTO fix_attempts (id, session_id, trigger, result, created_at, updated_at) VALUES (?, ?, ?, 0, ?, ?) RETURNING *;
-- name: GetFixAttempt :one
SELECT * FROM fix_attempts WHERE id = ?;
-- name: ListFixAttemptsBySession :many
SELECT * FROM fix_attempts WHERE session_id = ? ORDER BY created_at ASC;
-- name: UpdateFixAttempt :one
UPDATE fix_attempts SET result = COALESCE(sqlc.narg(result), result), error = CASE WHEN CAST(sqlc.arg(set_error) AS BOOLEAN) THEN sqlc.narg(error) ELSE error END, updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id) RETURNING *;
-- name: DeleteFixAttempt :execrows
DELETE FROM fix_attempts WHERE id = ?;
