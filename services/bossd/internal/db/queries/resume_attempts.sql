-- name: RecordResumeAttempt :one
INSERT INTO session_resume_attempts (id, session_id, budget_key, attempt_number, attempted_at, failure_class, failure_detail)
SELECT sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(budget_key), COALESCE(MAX(attempt_number), 0) + 1, sqlc.arg(attempted_at), sqlc.arg(failure_class), sqlc.arg(failure_detail)
FROM session_resume_attempts WHERE session_id = sqlc.arg(session_id) AND budget_key = sqlc.arg(budget_key) RETURNING *;
-- name: ListResumeAttemptsByBudget :many
SELECT * FROM session_resume_attempts WHERE session_id = ? AND budget_key = ? ORDER BY attempt_number ASC;
-- name: DeleteResumeAttempts :execrows
DELETE FROM session_resume_attempts WHERE session_id = ? AND budget_key = ?;
-- name: GetResumeAttempt :one
SELECT * FROM session_resume_attempts WHERE id = ?;
-- name: SetResumeAttemptFailure :exec
UPDATE session_resume_attempts SET failure_class = sqlc.arg(failure_class) WHERE id = sqlc.arg(id) AND failure_class != sqlc.arg(failure_class);
