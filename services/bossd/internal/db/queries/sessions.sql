-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: ListSessions :many
SELECT s.* FROM sessions s WHERE (CAST(sqlc.arg(repo_id) AS TEXT) = '' OR s.repo_id = sqlc.arg(repo_id)) ORDER BY (s.list_rank IS NULL) ASC, s.list_rank ASC, s.created_at DESC, s.id ASC;

-- name: ListSessionsByState :many
SELECT s.* FROM sessions s WHERE s.state = ? ORDER BY s.updated_at DESC;

-- name: ListSessionsByStates :many
SELECT s.* FROM sessions s WHERE s.state IN (sqlc.slice('states')) ORDER BY s.updated_at DESC;

-- name: ListSessionTmuxNames :many
SELECT tmux_session_name FROM sessions WHERE tmux_session_name IS NOT NULL AND tmux_session_name != '';

-- name: ListSessionsByActive :many
SELECT s.* FROM sessions s WHERE (CAST(sqlc.arg(repo_id) AS TEXT) = '' OR s.repo_id = sqlc.arg(repo_id)) AND s.archived_at IS NULL ORDER BY (s.list_rank IS NULL) ASC, s.list_rank ASC, s.created_at DESC, s.id ASC;

-- name: ListSessionsByArchived :many
SELECT s.* FROM sessions s WHERE (CAST(sqlc.arg(repo_id) AS TEXT) = '' OR s.repo_id = sqlc.arg(repo_id)) AND s.archived_at IS NOT NULL ORDER BY s.created_at DESC;

-- name: ListSessionsByActiveWithRepo :many
SELECT sqlc.embed(s), COALESCE(r.display_name, '') AS repo_display_name, COALESCE(r.origin_url, '') AS repo_origin_url FROM sessions s LEFT JOIN repos r ON r.id = s.repo_id WHERE (CAST(sqlc.arg(repo_id) AS TEXT) = '' OR s.repo_id = sqlc.arg(repo_id)) AND s.archived_at IS NULL ORDER BY (s.list_rank IS NULL) ASC, s.list_rank ASC, s.created_at DESC, s.id ASC;

-- name: ListSessionsWithRepo :many
SELECT sqlc.embed(s), COALESCE(r.display_name, '') AS repo_display_name, COALESCE(r.origin_url, '') AS repo_origin_url FROM sessions s LEFT JOIN repos r ON r.id = s.repo_id WHERE (CAST(sqlc.arg(repo_id) AS TEXT) = '' OR s.repo_id = sqlc.arg(repo_id)) ORDER BY (s.list_rank IS NULL) ASC, s.list_rank ASC, s.created_at DESC, s.id ASC;

-- name: ListSessionsByRepoAndPR :many
SELECT sqlc.embed(s), COALESCE(r.display_name, '') AS repo_display_name, COALESCE(r.origin_url, '') AS repo_origin_url FROM sessions s LEFT JOIN repos r ON r.id = s.repo_id WHERE s.repo_id = ? AND s.pr_number = ? AND s.archived_at IS NULL ORDER BY s.created_at DESC;

-- name: CreateSession :one
INSERT INTO sessions (id, repo_id, title, plan, worktree_path, branch_name, base_branch, state, state_entered_at, agent_name, model, effective_model, effective_effort, account_id, pr_number, pr_url, tracker_id, tracker_url, created_at, updated_at, is_verify_only)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: UpdateSession :one
UPDATE sessions SET
 updated_at = sqlc.arg(updated_at),
 state_entered_at = CASE WHEN CAST(sqlc.narg(state) AS INTEGER) IS NOT NULL AND state != CAST(sqlc.narg(state) AS INTEGER) THEN sqlc.arg(updated_at) ELSE state_entered_at END,
 state = COALESCE(CAST(sqlc.narg(state) AS INTEGER), state),
 title = COALESCE(sqlc.narg(title), title),
 is_branch_created = COALESCE(CAST(sqlc.narg(is_branch_created) AS BOOLEAN), is_branch_created),
 worktree_created_at = CASE WHEN CAST(sqlc.arg(set_worktree_created_at) AS BOOLEAN) THEN sqlc.narg(worktree_created_at) ELSE worktree_created_at END,
 worktree_path = COALESCE(sqlc.narg(worktree_path), worktree_path),
 branch_name = COALESCE(sqlc.narg(branch_name), branch_name),
 agent_session_id = CASE WHEN CAST(sqlc.arg(set_agent_session_id) AS BOOLEAN) THEN sqlc.narg(agent_session_id) ELSE agent_session_id END,
 pr_number = CASE WHEN CAST(sqlc.arg(set_pr_number) AS BOOLEAN) THEN sqlc.narg(pr_number) ELSE pr_number END,
 pr_url = CASE WHEN CAST(sqlc.arg(set_pr_url) AS BOOLEAN) THEN sqlc.narg(pr_url) ELSE pr_url END,
 tracker_id = CASE WHEN CAST(sqlc.arg(set_tracker_id) AS BOOLEAN) THEN sqlc.narg(tracker_id) ELSE tracker_id END,
 tracker_url = CASE WHEN CAST(sqlc.arg(set_tracker_url) AS BOOLEAN) THEN sqlc.narg(tracker_url) ELSE tracker_url END,
 tmux_session_name = CASE WHEN CAST(sqlc.arg(set_tmux_session_name) AS BOOLEAN) THEN sqlc.narg(tmux_session_name) ELSE tmux_session_name END,
 last_check_state = COALESCE(sqlc.narg(last_check_state), last_check_state),
 last_check_state_head_sha = CASE WHEN CAST(sqlc.arg(set_last_check_state_head_sha) AS BOOLEAN) THEN sqlc.narg(last_check_state_head_sha) ELSE last_check_state_head_sha END,
 last_check_state_at = CASE WHEN CAST(sqlc.arg(set_last_check_state_at) AS BOOLEAN) THEN sqlc.narg(last_check_state_at) ELSE last_check_state_at END,
 last_observed_review_state = COALESCE(sqlc.narg(last_observed_review_state), last_observed_review_state),
 is_automation_enabled = COALESCE(CAST(sqlc.narg(is_automation_enabled) AS BOOLEAN), is_automation_enabled),
 attempt_count = COALESCE(sqlc.narg(attempt_count), attempt_count),
 blocked_reason = CASE WHEN CAST(sqlc.arg(set_blocked_reason) AS BOOLEAN) THEN sqlc.narg(blocked_reason) ELSE blocked_reason END,
 last_repair_attempt_count = COALESCE(sqlc.narg(last_repair_attempt_count), last_repair_attempt_count),
 run_blocked_reason = CASE WHEN CAST(sqlc.arg(set_run_blocked_reason) AS BOOLEAN) THEN sqlc.narg(run_blocked_reason) ELSE run_blocked_reason END,
 last_attempt_head_sha = CASE WHEN CAST(sqlc.arg(set_last_attempt_head_sha) AS BOOLEAN) THEN sqlc.narg(last_attempt_head_sha) ELSE last_attempt_head_sha END,
 rotation_attempt_count = COALESCE(sqlc.narg(rotation_attempt_count), rotation_attempt_count),
 rotation_resume_at = CASE WHEN CAST(sqlc.arg(set_rotation_resume_at) AS BOOLEAN) THEN sqlc.narg(rotation_resume_at) WHEN CAST(sqlc.narg(state) AS INTEGER) IS NOT NULL AND CAST(sqlc.narg(state) AS INTEGER) != CAST(sqlc.arg(blocked_state) AS INTEGER) THEN NULL ELSE rotation_resume_at END,
 archived_at = CASE WHEN CAST(sqlc.arg(set_archived_at) AS BOOLEAN) THEN sqlc.narg(archived_at) ELSE archived_at END,
 cron_job_id = CASE WHEN CAST(sqlc.arg(set_cron_job_id) AS BOOLEAN) THEN sqlc.narg(cron_job_id) ELSE cron_job_id END,
 hook_token = CASE WHEN CAST(sqlc.arg(set_hook_token) AS BOOLEAN) THEN sqlc.narg(hook_token) ELSE hook_token END,
 account_id = CASE WHEN CAST(sqlc.arg(set_account_id) AS BOOLEAN) THEN sqlc.narg(account_id) ELSE account_id END,
 is_tmux_unattended = COALESCE(CAST(sqlc.narg(is_tmux_unattended) AS BOOLEAN), is_tmux_unattended),
 is_detached = COALESCE(CAST(sqlc.narg(is_detached) AS BOOLEAN), is_detached),
 is_quick_chat = COALESCE(CAST(sqlc.narg(is_quick_chat) AS BOOLEAN), is_quick_chat),
 display_label = COALESCE(sqlc.narg(display_label), display_label),
 display_intent = COALESCE(sqlc.narg(display_intent), display_intent),
 has_display_spinner = COALESCE(CAST(sqlc.narg(has_display_spinner) AS BOOLEAN), has_display_spinner),
 setup_error = COALESCE(sqlc.narg(setup_error), setup_error)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: MarkSessionArchived :execrows
UPDATE sessions SET archived_at = ?, updated_at = ? WHERE id = ? AND archived_at IS NULL;

-- name: UpdateSessionRepairBlocked :execrows
UPDATE sessions
		 SET last_repair_blocked_reason = ?,
		     last_repair_blocked_at     = ?
		 WHERE id = ?;

-- name: SetSessionRepairAlertCleared :execrows
UPDATE sessions
		 SET last_repair_blocked_reason = '',
		     last_repair_blocked_at     = NULL,
		     last_repair_attempt_count  = 0,
		     last_repair_runner_error   = '',
		     last_repair_exit_error     = ''
		 WHERE id = ?;

-- name: SetSessionDismissedAlert :execrows
UPDATE sessions
		 SET dismissed_alert_key = ?,
		     dismissed_alert_at  = ?
		 WHERE id = ?;

-- name: UpdateSessionRepairDiagnostics :execrows
UPDATE sessions
		 SET last_repair_started_at    = sqlc.arg(last_repair_started_at),
		     last_repair_runner_error  = sqlc.arg(last_repair_runner_error),
		     last_repair_exit_error    = sqlc.arg(last_repair_exit_error),
		     last_repair_attempt_count = CASE WHEN CAST(sqlc.arg(has_error) AS BOOLEAN) THEN last_repair_attempt_count + 1 ELSE 0 END,
		     last_repair_head_sha       = sqlc.arg(last_repair_head_sha),
		     last_repair_display_status = sqlc.arg(last_repair_display_status),
		     last_repair_review_fingerprint = COALESCE(sqlc.narg(last_repair_review_fingerprint), last_repair_review_fingerprint),
		     last_repair_base_sha       = sqlc.arg(last_repair_base_sha),
		     last_repair_blocked_reason = '',
		     last_repair_blocked_at     = NULL
		 WHERE id = sqlc.arg(id);

-- name: SetTaskMappingsSessionDetached :exec
UPDATE task_mappings SET session_id = NULL WHERE session_id = ?;

-- name: DeleteSessionCheckSnapshots :exec
DELETE FROM session_check_snapshots WHERE session_id = ?;

-- name: DeleteSessionAgentRunChildren :exec
DELETE FROM agent_run_children WHERE agent_run_id IN (SELECT id FROM agent_runs WHERE session_id = ?);

-- name: DeleteSessionAgentRuns :exec
DELETE FROM agent_runs WHERE session_id = ?;

-- name: DeleteSessionWorkflows :exec
DELETE FROM workflows WHERE session_id = ?;

-- name: DeleteSessionAgentChats :exec
DELETE FROM agent_chats WHERE session_id = ?;

-- name: DeleteSessionFixAttempts :exec
DELETE FROM fix_attempts WHERE session_id = ?;

-- name: DeleteSessionResumeAttempts :exec
DELETE FROM session_resume_attempts WHERE session_id = ?;

-- name: SetCronJobsLastRunSessionDetached :exec
UPDATE cron_jobs SET last_run_session_id = NULL WHERE last_run_session_id = ?;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE id = ?;

-- name: SetSessionListRank :execrows
UPDATE sessions SET list_rank = ? WHERE id = ?;
