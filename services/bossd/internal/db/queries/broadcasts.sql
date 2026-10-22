-- name: CreateBroadcast :execresult
INSERT INTO broadcasts
			(id, origin_chat_id, selector, message, state, target_count, expires_at, created_at, updated_at)
		 VALUES (sqlc.arg(id), sqlc.narg(origin_chat_id), sqlc.arg(selector), sqlc.arg(message), sqlc.arg(state), sqlc.arg(target_count), sqlc.arg(expires_at), sqlc.arg(created_at), sqlc.arg(updated_at));

-- name: UpdateBroadcastAudience :execresult
UPDATE broadcasts
		 SET target_count = sqlc.arg(target_count), state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(pending_state);

-- name: GetBroadcastState :one
SELECT state FROM broadcasts WHERE id = sqlc.arg(id);

-- name: CreateBroadcastDelivery :execresult
INSERT INTO broadcast_deliveries
				(id, broadcast_id, target_chat_id, target_daemon_id, state, attempt_count, created_at, updated_at)
			 VALUES (sqlc.arg(id), sqlc.arg(broadcast_id), sqlc.arg(target_chat_id), sqlc.arg(target_daemon_id), sqlc.arg(state), 0, sqlc.arg(created_at), sqlc.arg(updated_at));

-- name: DeleteBroadcast :execresult
DELETE FROM broadcasts WHERE id = sqlc.arg(id);

-- name: MarkBroadcastsExpired :execresult
UPDATE broadcasts
		 SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
		 WHERE state IN (sqlc.arg(pending_state), sqlc.arg(resolved_state)) AND expires_at <= sqlc.arg(expires_at);

-- name: MarkExpiredBroadcastDeliveriesSkipped :many
UPDATE broadcast_deliveries
		 SET state = sqlc.arg(state), lease_owner = NULL, lease_deadline_at = NULL, updated_at = sqlc.arg(updated_at)
		 WHERE broadcast_deliveries.state IN (sqlc.arg(pending_state), sqlc.arg(leased_state))
		   AND broadcast_id IN (SELECT id FROM broadcasts WHERE broadcasts.state = sqlc.arg(broadcast_state) AND expires_at <= sqlc.arg(expires_at))
		 RETURNING attempt_count;

-- name: SetBroadcastDeliveryLease :execresult
UPDATE broadcast_deliveries
		 SET state = sqlc.arg(state), lease_owner = sqlc.narg(lease_owner), lease_deadline_at = sqlc.narg(lease_deadline_at), updated_at = sqlc.arg(updated_at)
		 WHERE broadcast_deliveries.id = sqlc.arg(id)
		   AND broadcast_deliveries.state IN (sqlc.arg(pending_state), sqlc.arg(leased_state))
		   AND (next_attempt_at IS NULL OR next_attempt_at <= sqlc.narg(next_attempt_at))
		   AND (lease_owner IS NULL OR lease_owner = sqlc.narg(owner) OR lease_deadline_at IS NULL OR lease_deadline_at <= sqlc.narg(lease_expiry))
		   AND EXISTS (
		         SELECT 1 FROM broadcasts b
		         WHERE b.id = broadcast_deliveries.broadcast_id
		           AND b.state IN (sqlc.arg(pending_broadcast_state), sqlc.arg(resolved_broadcast_state))
		           AND b.expires_at > sqlc.arg(broadcast_expiry)
		       );

-- name: MarkBroadcastDeliveryDelivered :execresult
UPDATE broadcast_deliveries
		 SET state = sqlc.arg(state), delivered_at = sqlc.narg(delivered_at), lease_owner = NULL, lease_deadline_at = NULL,
		     last_error = NULL, updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(leased_state) AND lease_owner = sqlc.narg(lease_owner);

-- name: UpdateBroadcastDeliveryRetry :execresult
UPDATE broadcast_deliveries
		 SET attempt_count = attempt_count + 1,
		     next_attempt_at = sqlc.narg(next_attempt_at), last_error = sqlc.narg(last_error),
		     state = CASE WHEN state = sqlc.arg(leased_state) THEN sqlc.arg(pending_state) ELSE state END,
		     lease_owner = NULL, lease_deadline_at = NULL, updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND lease_owner = sqlc.narg(lease_owner);

-- name: MarkBroadcastCompletedIfSettled :execresult
UPDATE broadcasts
		 SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
		 WHERE broadcasts.id = sqlc.arg(id) AND broadcasts.state = sqlc.arg(resolved_state)
		   AND NOT EXISTS (
		         SELECT 1 FROM broadcast_deliveries d
		         WHERE d.broadcast_id = broadcasts.id AND d.state IN (sqlc.arg(pending_state), sqlc.arg(leased_state))
		       );

-- name: DeleteUnmaterialisedBroadcast :execresult
DELETE FROM broadcasts WHERE id = sqlc.arg(id) AND state = sqlc.arg(state);

-- name: GetBroadcast :one
SELECT * FROM broadcasts WHERE id = ?;

-- name: GetBroadcastDelivery :one
SELECT * FROM broadcast_deliveries WHERE id = ?;

-- name: ListBroadcasts :many
SELECT * FROM broadcasts
WHERE (CAST(sqlc.narg(state) AS TEXT) IS NULL OR state = sqlc.narg(state))
 AND (CAST(sqlc.narg(origin_chat_id) AS TEXT) IS NULL OR origin_chat_id = sqlc.narg(origin_chat_id))
 AND (CAST(sqlc.narg(target_chat_id) AS TEXT) IS NULL OR id IN (SELECT broadcast_id FROM broadcast_deliveries WHERE target_chat_id = sqlc.narg(target_chat_id)))
ORDER BY created_at ASC, id ASC LIMIT sqlc.arg(row_limit);

-- name: ListBroadcastDeliveries :many
SELECT * FROM broadcast_deliveries WHERE broadcast_id = ? ORDER BY created_at ASC, id ASC;
