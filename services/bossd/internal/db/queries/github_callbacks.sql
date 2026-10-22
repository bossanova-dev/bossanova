-- name: ListGitHubCallbackGroupTriggers :many
SELECT trigger_event
			 FROM github_callbacks
			 WHERE group_id = sqlc.arg(group_id) AND state IN (sqlc.arg(active_state), sqlc.arg(leased_state));

-- name: CreateGitHubCallback :one
INSERT INTO github_callbacks
			(id, group_id, target_chat_id, repo_owner, repo_name, pr_number, trigger_event,
			 state, message, should_require_transition, has_observed_baseline, attempt_count,
			 expires_at, created_at, updated_at)
		 VALUES (sqlc.arg(id), sqlc.arg(group_id), sqlc.arg(target_chat_id), sqlc.arg(repo_owner), sqlc.arg(repo_name), sqlc.arg(pr_number), sqlc.arg(trigger_event), sqlc.arg(state), sqlc.arg(message), sqlc.arg(should_require_transition), 0, 0, sqlc.arg(expires_at), sqlc.arg(created_at), sqlc.arg(updated_at)) RETURNING *;

-- name: DeleteGitHubCallback :execrows
DELETE FROM github_callbacks
		 WHERE id = sqlc.arg(id) AND (CAST(sqlc.arg(target_chat_id) AS TEXT) = '' OR target_chat_id = sqlc.arg(target_chat_id));

-- name: GetGitHubCallbackTargetChat :one
SELECT target_chat_id FROM github_callbacks WHERE id = sqlc.arg(id);

-- name: MarkGitHubCallbackBaselineObserved :execrows
UPDATE github_callbacks
		 SET has_observed_baseline = 1, updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(state) AND should_require_transition = 1 AND has_observed_baseline = 0;

-- name: MarkGitHubCallbackUnreachable :execrows
UPDATE github_callbacks
		 SET state = sqlc.arg(state), last_event = sqlc.arg(last_event), updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state);

-- name: MarkGitHubCallbackCanceled :execrows
UPDATE github_callbacks
		 SET state = sqlc.arg(state), last_event = sqlc.arg(last_event), updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state);

-- name: MarkGitHubCallbacksExpired :many
UPDATE github_callbacks
		 SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
		 WHERE state IN (sqlc.arg(active_state), sqlc.arg(leased_state), sqlc.arg(triggered_state)) AND expires_at <= sqlc.arg(expires_at)
		 RETURNING trigger_event, attempt_count;

-- name: SetGitHubCallbackLease :execrows
UPDATE github_callbacks
		 SET state = CASE WHEN state = sqlc.arg(active_state) THEN sqlc.arg(leased_state) ELSE state END,
		     lease_owner = sqlc.arg(lease_owner), lease_deadline_at = sqlc.arg(lease_deadline_at), updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id)
		   AND state IN (sqlc.arg(active_state), sqlc.arg(leased_state), sqlc.arg(triggered_state))
		   AND expires_at > sqlc.arg(expires_at)
		   AND (next_attempt_at IS NULL OR next_attempt_at <= sqlc.arg(next_attempt_at))
		   AND (lease_owner IS NULL OR lease_owner = sqlc.arg(lease_owner) OR lease_deadline_at IS NULL OR lease_deadline_at <= sqlc.arg(lease_deadline_at_check));

-- name: MarkGitHubCallbackLeaseReleased :execrows
UPDATE github_callbacks
		 SET lease_owner = NULL, lease_deadline_at = NULL,
		     state = CASE WHEN state = sqlc.arg(leased_state) THEN sqlc.arg(active_state) ELSE state END,
		     updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner);

-- name: GetGitHubCallbackTriggerState :one
SELECT state, group_id, expires_at FROM github_callbacks WHERE id = sqlc.arg(id);

-- name: MarkGitHubCallbackTriggered :exec
UPDATE github_callbacks
		 SET state = sqlc.arg(state), triggered_at = sqlc.arg(triggered_at), last_event = sqlc.arg(last_event), lease_owner = NULL,
		     lease_deadline_at = NULL, updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id);

-- name: MarkGitHubCallbackSiblingsCanceled :exec
UPDATE github_callbacks
			 SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
			 WHERE group_id = sqlc.arg(group_id) AND id != sqlc.arg(id) AND state IN (sqlc.arg(active_state), sqlc.arg(leased_state));

-- name: MarkGitHubCallbackDelivered :execrows
UPDATE github_callbacks
		 SET state = sqlc.arg(state), delivered_at = sqlc.arg(delivered_at), lease_owner = NULL, lease_deadline_at = NULL,
		     last_error = NULL, updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state) AND lease_owner = sqlc.arg(lease_owner) AND expires_at > sqlc.arg(expires_at);

-- name: SetGitHubCallbackRetry :execrows
UPDATE github_callbacks
		 SET attempt_count = attempt_count + sqlc.arg(attempt_increment),
		     next_attempt_at = sqlc.arg(next_attempt_at), last_error = sqlc.arg(last_error), last_event = COALESCE(sqlc.arg(last_event), last_event),
		     lease_owner = NULL, lease_deadline_at = NULL, updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner);

-- name: GetGitHubCallback :one
SELECT * FROM github_callbacks WHERE id = sqlc.arg(id);

-- name: ListGitHubCallbacks :many
SELECT * FROM github_callbacks
WHERE (CAST(sqlc.narg(target_chat_id) AS TEXT) IS NULL OR target_chat_id = sqlc.narg(target_chat_id))
AND (CAST(sqlc.narg(repo_owner) AS TEXT) IS NULL OR repo_owner = sqlc.narg(repo_owner))
AND (CAST(sqlc.narg(repo_name) AS TEXT) IS NULL OR repo_name = sqlc.narg(repo_name))
AND (CAST(sqlc.narg(pr_number) AS INTEGER) IS NULL OR pr_number = sqlc.narg(pr_number))
AND (CAST(sqlc.narg(trigger_event) AS TEXT) IS NULL OR trigger_event = sqlc.narg(trigger_event))
AND (CAST(sqlc.narg(state) AS TEXT) IS NULL OR state = sqlc.narg(state))
ORDER BY created_at, id;

-- name: ListGitHubCallbackCrossGroupConflicts :many
SELECT id, group_id, trigger_event, expires_at FROM github_callbacks
WHERE target_chat_id = sqlc.arg(target_chat_id) AND repo_owner = sqlc.arg(repo_owner)
AND repo_name = sqlc.arg(repo_name) AND pr_number = sqlc.arg(pr_number)
AND state IN ('active', 'leased') AND expires_at > sqlc.arg(expires_at)
AND (CAST(sqlc.narg(group_id) AS TEXT) IS NULL OR group_id IS NULL OR group_id != sqlc.narg(group_id))
ORDER BY created_at, id;
