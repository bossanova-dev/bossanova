-- name: CountRepos :one
SELECT count(*) FROM repos;

-- name: ListTrustedRepos :many
SELECT is_trusted FROM repos;

-- name: GetRepo :one
SELECT * FROM repos WHERE id = ?;

-- name: GetRepoByLocalPath :one
SELECT * FROM repos WHERE local_path = ?;

-- name: GetRepoByOriginURL :one
SELECT * FROM repos WHERE origin_url = ?;

-- name: ListRepos :many
SELECT * FROM repos ORDER BY created_at DESC;

-- name: CreateRepo :one
INSERT INTO repos (id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, should_archive_sessions_after_merge, can_auto_delete_branches, should_keep_branches_current, merge_strategy, linear_api_key, sentry_api_key, sentry_org, is_trusted, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, 1, 1, 1, 1, 0, 0, 'merge', '', '', '', ?, ?, ?) RETURNING *;

-- name: GetRepoKeyState :one
SELECT updated_at, has_keyring_secrets FROM repos WHERE id = ?;

-- name: GetRepoKeys :one
SELECT linear_api_key, sentry_api_key, has_keyring_secrets FROM repos WHERE id = ?;

-- name: ListReposByPlaintextKeys :many
SELECT id FROM repos WHERE has_keyring_secrets = 0 AND (linear_api_key != '' OR sentry_api_key != '') ORDER BY id;

-- name: UpdateRepoKeysToKeyring :execrows
UPDATE repos SET linear_api_key = '', sentry_api_key = '', has_keyring_secrets = 1
WHERE id = ? AND has_keyring_secrets = 0 AND linear_api_key = ? AND sentry_api_key = ?;

-- name: RecordRepoSecretScrub :exec
INSERT OR IGNORE INTO pending_secret_scrubs (id) VALUES (1);

-- name: UpdateRepo :one
UPDATE repos SET
display_name = COALESCE(sqlc.narg(display_name), display_name),
origin_url = COALESCE(sqlc.narg(origin_url), origin_url),
default_base_branch = COALESCE(sqlc.narg(default_base_branch), default_base_branch),
worktree_base_dir = COALESCE(sqlc.narg(worktree_base_dir), worktree_base_dir),
setup_script = CASE WHEN CAST(sqlc.arg(set_setup_script) AS BOOLEAN) THEN sqlc.narg(setup_script) ELSE setup_script END,
can_auto_merge = COALESCE(CAST(sqlc.narg(can_auto_merge) AS BOOLEAN), can_auto_merge),
can_auto_merge_dependabot = COALESCE(CAST(sqlc.narg(can_auto_merge_dependabot) AS BOOLEAN), can_auto_merge_dependabot),
can_auto_repair = COALESCE(CAST(sqlc.narg(can_auto_repair) AS BOOLEAN), can_auto_repair),
can_auto_rotate = COALESCE(CAST(sqlc.narg(can_auto_rotate) AS BOOLEAN), can_auto_rotate),
should_archive_sessions_after_merge = COALESCE(CAST(sqlc.narg(should_archive_sessions_after_merge) AS BOOLEAN), should_archive_sessions_after_merge),
can_auto_delete_branches = COALESCE(CAST(sqlc.narg(can_auto_delete_branches) AS BOOLEAN), can_auto_delete_branches),
should_keep_branches_current = COALESCE(CAST(sqlc.narg(should_keep_branches_current) AS BOOLEAN), should_keep_branches_current),
merge_strategy = COALESCE(sqlc.narg(merge_strategy), merge_strategy),
sentry_org = COALESCE(sqlc.narg(sentry_org), sentry_org),
is_trusted = COALESCE(CAST(sqlc.narg(is_trusted) AS BOOLEAN), is_trusted),
linear_api_key = CASE WHEN CAST(sqlc.arg(set_keys) AS BOOLEAN) THEN sqlc.arg(linear_api_key) ELSE linear_api_key END,
sentry_api_key = CASE WHEN CAST(sqlc.arg(set_keys) AS BOOLEAN) THEN sqlc.arg(sentry_api_key) ELSE sentry_api_key END,
has_keyring_secrets = CASE WHEN CAST(sqlc.arg(set_keys) AS BOOLEAN) THEN CAST(sqlc.arg(has_keyring_secrets) AS BOOLEAN) ELSE has_keyring_secrets END,
updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteRepoCheckSnapshots :exec
DELETE FROM session_check_snapshots WHERE session_id IN (SELECT sessions.id FROM sessions WHERE sessions.repo_id = ?);

-- name: DeleteRepoWorkflows :exec
DELETE FROM workflows WHERE repo_id = ?;

-- name: DeleteRepoTaskMappings :exec
DELETE FROM task_mappings WHERE repo_id = ?;

-- name: DeleteRepoFixAttempts :exec
DELETE FROM fix_attempts WHERE session_id IN (SELECT sessions.id FROM sessions WHERE sessions.repo_id = ?);

-- name: DeleteRepoAgentChats :exec
DELETE FROM agent_chats WHERE session_id IN (SELECT sessions.id FROM sessions WHERE sessions.repo_id = ?);

-- name: SetRepoCronJobsLastRunSessionNull :exec
UPDATE cron_jobs SET last_run_session_id = NULL WHERE last_run_session_id IN (SELECT sessions.id FROM sessions WHERE sessions.repo_id = ?);

-- name: DeleteRepoCronJobs :exec
DELETE FROM cron_jobs WHERE repo_id = ?;

-- name: DeleteRepoSessions :exec
DELETE FROM sessions WHERE repo_id = ?;

-- name: DeleteRepo :execrows
DELETE FROM repos WHERE id = ?;

-- name: CountRepoSecretScrubs :one
SELECT count(*) FROM pending_secret_scrubs;

-- name: DeleteRepoSecretScrubs :exec
DELETE FROM pending_secret_scrubs;
