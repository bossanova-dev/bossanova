-- name: CreateAgentAccount :one
INSERT INTO agent_accounts (id, provider, label, status, priority, health, tier, allowed_models, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetAgentAccount :one
SELECT * FROM agent_accounts WHERE id = ?;

-- name: ListAgentAccounts :many
SELECT * FROM agent_accounts ORDER BY provider ASC, priority ASC, created_at ASC;

-- name: ListAgentAccountsByProvider :many
SELECT * FROM agent_accounts WHERE provider = ? ORDER BY provider ASC, priority ASC, created_at ASC;

-- name: UpdateAgentAccount :one
UPDATE agent_accounts SET
 label = COALESCE(sqlc.narg(label), label),
 status = COALESCE(sqlc.narg(status), status),
 priority = COALESCE(sqlc.narg(priority), priority),
 health = COALESCE(sqlc.narg(health), health),
 cooldown_until = CASE WHEN CAST(sqlc.arg(set_cooldown_until) AS BOOLEAN) THEN sqlc.narg(cooldown_until) ELSE cooldown_until END,
 last_used_at = CASE WHEN CAST(sqlc.arg(set_last_used_at) AS BOOLEAN) THEN sqlc.narg(last_used_at) ELSE last_used_at END,
 tier = COALESCE(sqlc.narg(tier), tier),
 allowed_models = COALESCE(sqlc.narg(allowed_models), allowed_models),
 updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteAgentAccount :execrows
DELETE FROM agent_accounts WHERE id = ?;

-- name: RecordAgentAccountTestResult :execrows
UPDATE agent_accounts SET last_test_ok_at = sqlc.narg(last_test_ok_at), last_test_error = sqlc.arg(last_test_error), updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id);

-- name: MarkAgentAccountSuspended :execrows
UPDATE agent_accounts SET health = ?, last_test_error = ?, last_test_ok_at = NULL, updated_at = ? WHERE id = ?;

-- name: RecordAgentAccountInjectionFailure :execrows
UPDATE agent_accounts SET health = sqlc.arg(failed_health),
 last_test_error = CASE WHEN health = sqlc.arg(failed_health) AND last_test_error <> '' AND last_test_error NOT LIKE sqlc.arg(prefix)
 THEN last_test_error ELSE sqlc.arg(reason) END,
 updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id);

-- name: MarkAgentAccountInjectionFailureCleared :exec
UPDATE agent_accounts SET health = sqlc.arg(healthy_health), last_test_error = '', updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND health = sqlc.arg(failed_health) AND last_test_error LIKE sqlc.arg(prefix);

-- name: RecordAgentAccountUsageProbe :execrows
UPDATE agent_accounts SET usage_util_5h = sqlc.arg(usage_util_5h), usage_util_7d = sqlc.arg(usage_util_7d), usage_reset_5h = sqlc.narg(usage_reset_5h), usage_reset_7d = sqlc.narg(usage_reset_7d),
 usage_status = sqlc.arg(usage_status), usage_plan_tier = sqlc.arg(usage_plan_tier), usage_fetched_at = sqlc.narg(usage_fetched_at), usage_model_windows = sqlc.arg(usage_model_windows), updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id);

-- name: RecordAgentAccountAuthCheck :execrows
UPDATE agent_accounts SET auth_checked_at = sqlc.narg(auth_checked_at), auth_check_outcome = sqlc.arg(auth_check_outcome), auth_check_failure_class = sqlc.arg(auth_check_failure_class),
 auth_check_next_retry_at = sqlc.narg(auth_check_next_retry_at), updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id);

-- name: MarkAgentAccountAuthCheckCleared :execrows
UPDATE agent_accounts SET auth_checked_at = NULL, auth_check_outcome = '', auth_check_failure_class = '', auth_check_next_retry_at = NULL, updated_at = ? WHERE id = ?;

-- name: SetAgentAccountCooldownIfNotCooling :execrows
UPDATE agent_accounts SET cooldown_until = sqlc.narg(cooldown_until), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND provider = sqlc.arg(provider) AND (cooldown_until IS NULL OR cooldown_until <= sqlc.arg(now));

-- name: SetAgentAccountHealthFailed :exec
UPDATE agent_accounts SET health = ?, updated_at = ? WHERE id = ? AND provider = ?;

-- name: SetAgentAccountIdentity :execrows
UPDATE agent_accounts SET identity_kind = ?, identity_digest = ?, updated_at = ? WHERE id = ?;

-- name: SetAgentAccountSync :execrows
UPDATE agent_accounts SET synced_agent_account_id = sqlc.arg(synced_agent_account_id),
 sync_version = sqlc.arg(sync_version), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND sync_version < sqlc.arg(sync_version);

-- name: UpdateAgentAccountSync :execrows
UPDATE agent_accounts SET status = COALESCE(sqlc.narg(status), status), priority = sqlc.arg(priority),
 synced_agent_account_id = sqlc.arg(synced_agent_account_id), sync_version = sqlc.arg(sync_version), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND sync_version < sqlc.arg(sync_version);

-- name: SetAgentAccountCredentialVersion :execrows
UPDATE agent_accounts SET synced_credential_version = sqlc.arg(synced_credential_version), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND synced_credential_version <= sqlc.arg(synced_credential_version);
