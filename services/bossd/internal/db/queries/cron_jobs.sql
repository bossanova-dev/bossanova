-- name: CreateCronJob :one
INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, timezone, agent_name, model, is_enabled, gate_command, should_run_setup_command, is_zero_output, concurrency_policy, managed_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetCronJob :one
SELECT * FROM cron_jobs WHERE id = ?;

-- name: ListCronJobs :many
SELECT * FROM cron_jobs ORDER BY name COLLATE NOCASE ASC, created_at DESC;

-- name: ListCronJobsByRepo :many
SELECT * FROM cron_jobs WHERE repo_id = ? ORDER BY name COLLATE NOCASE ASC, created_at DESC;

-- name: ListCronJobsByEnabled :many
SELECT * FROM cron_jobs WHERE is_enabled = 1 ORDER BY created_at ASC;

-- name: UpdateCronJob :one
UPDATE cron_jobs SET
 name = COALESCE(sqlc.narg(name), name),
 prompt = COALESCE(sqlc.narg(prompt), prompt),
 schedule = COALESCE(sqlc.narg(schedule), schedule),
 timezone = CASE WHEN CAST(sqlc.arg(set_timezone) AS BOOLEAN) THEN sqlc.narg(timezone) ELSE timezone END,
 agent_name = COALESCE(sqlc.narg(agent_name), agent_name),
 model = COALESCE(sqlc.narg(model), model),
 is_enabled = COALESCE(CAST(sqlc.narg(is_enabled) AS BOOLEAN), is_enabled),
 next_run_at = CASE WHEN CAST(sqlc.arg(set_next_run_at) AS BOOLEAN) THEN sqlc.narg(next_run_at) ELSE next_run_at END,
 gate_command = COALESCE(sqlc.narg(gate_command), gate_command),
 should_run_setup_command = COALESCE(CAST(sqlc.narg(should_run_setup_command) AS BOOLEAN), should_run_setup_command),
 is_zero_output = COALESCE(CAST(sqlc.narg(is_zero_output) AS BOOLEAN), is_zero_output),
 concurrency_policy = COALESCE(sqlc.narg(concurrency_policy), concurrency_policy),
 managed_by = COALESCE(sqlc.narg(managed_by), managed_by),
 updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: MarkCronJobFireStarted :execrows
UPDATE cron_jobs SET last_run_session_id = ?, last_run_agent_name = ?, last_run_at = ?, last_run_outcome = NULL, updated_at = ?, next_run_at = ? WHERE id = ?;

-- name: UpdateCronJobLastRun :execrows
UPDATE cron_jobs SET last_run_at = sqlc.arg(last_run_at), last_run_outcome = sqlc.arg(last_run_outcome), updated_at = sqlc.arg(updated_at),
 last_run_session_id = COALESCE(sqlc.narg(last_run_session_id), last_run_session_id),
 next_run_at = CASE WHEN CAST(sqlc.arg(set_next_run_at) AS BOOLEAN) THEN sqlc.narg(next_run_at) ELSE next_run_at END
WHERE id = sqlc.arg(id)
 AND (CAST(sqlc.narg(expected_session_id) AS TEXT) IS NULL OR last_run_session_id = sqlc.narg(expected_session_id) OR (CAST(sqlc.arg(allow_cleared_expected_session_id) AS BOOLEAN) AND last_run_session_id IS NULL));

-- name: DeleteCronJob :execrows
DELETE FROM cron_jobs WHERE id = ?;
