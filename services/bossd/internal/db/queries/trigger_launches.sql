-- name: GetTriggerLaunch :one
SELECT invocation_id, trigger_id, session_id, accepted_session_id, created_at FROM trigger_launches WHERE invocation_id = ?;

-- name: UpsertTriggerLaunch :execrows
INSERT INTO trigger_launches (invocation_id, trigger_id, created_at, claim_token)
SELECT sqlc.arg(invocation_id), sqlc.arg(trigger_id), sqlc.arg(created_at), sqlc.arg(claim_token)
WHERE NOT EXISTS (
 SELECT 1 FROM trigger_launches AS existing WHERE existing.invocation_id = sqlc.arg(invocation_id)
 AND (existing.session_id IS NOT NULL OR existing.accepted_session_id IS NOT NULL OR existing.created_at >= sqlc.arg(stale_before))
)
ON CONFLICT(invocation_id) DO UPDATE SET trigger_id = excluded.trigger_id, created_at = excluded.created_at, claim_token = excluded.claim_token;

-- name: SetTriggerLaunchSession :execrows
UPDATE trigger_launches SET session_id = ? WHERE invocation_id = ? AND claim_token = ?;

-- name: DeleteTriggerLaunch :exec
DELETE FROM trigger_launches WHERE invocation_id = ? AND claim_token = ? AND session_id IS NULL;

-- name: MarkTriggerLaunchAccepted :execrows
UPDATE trigger_launches SET accepted_session_id = ? WHERE invocation_id = ? AND claim_token = ? AND session_id IS NULL;

-- name: MarkTriggerLaunchFinished :execrows
UPDATE trigger_launches SET session_id = accepted_session_id WHERE invocation_id = ? AND session_id IS NULL AND accepted_session_id IS NOT NULL AND created_at < sqlc.arg(stale_before);

-- name: DeleteTriggerLaunches :execrows
DELETE FROM trigger_launches WHERE created_at < sqlc.arg(stale_before);
