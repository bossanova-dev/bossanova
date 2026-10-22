-- name: CreateBroadcastSubscription :execresult
INSERT INTO broadcast_subscriptions
			(id, owner_session_id, origin_chat_id, trigger_event, selector, message,
			 state, expires_at, created_at, updated_at)
		 VALUES (sqlc.arg(id), sqlc.arg(owner_session_id), sqlc.narg(origin_chat_id), sqlc.arg(trigger_event), sqlc.arg(selector), sqlc.arg(message), sqlc.arg(state), sqlc.arg(expires_at), sqlc.arg(created_at), sqlc.arg(updated_at));

-- name: MarkBroadcastSubscriptionFired :execresult
UPDATE broadcast_subscriptions
		 SET state = sqlc.arg(state), fired_at = sqlc.narg(fired_at), fired_broadcast_id = sqlc.narg(fired_broadcast_id), updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(active_state);

-- name: MarkBroadcastSubscriptionCanceled :execresult
UPDATE broadcast_subscriptions
		 SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id) AND state = sqlc.arg(active_state);

-- name: MarkBroadcastSubscriptionsExpired :execresult
UPDATE broadcast_subscriptions
		 SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at)
		 WHERE state = sqlc.arg(active_state) AND expires_at <= sqlc.arg(expires_at);

-- name: ListActiveBroadcastSubscriptionOwnerSessionIDs :many
SELECT DISTINCT owner_session_id FROM broadcast_subscriptions
		 WHERE state = sqlc.arg(state) AND expires_at > sqlc.arg(expires_at)
		 ORDER BY owner_session_id ASC;

-- name: GetBroadcastSubscription :one
SELECT * FROM broadcast_subscriptions WHERE id = ?;

-- name: ListActiveBroadcastSubscriptionsBySession :many
SELECT * FROM broadcast_subscriptions
WHERE owner_session_id = sqlc.arg(owner_session_id) AND state = sqlc.arg(state) AND expires_at > sqlc.arg(expires_at)
ORDER BY created_at ASC, id ASC;

-- name: ListBroadcastSubscriptions :many
SELECT * FROM broadcast_subscriptions
WHERE (CAST(sqlc.narg(owner_session_id) AS TEXT) IS NULL OR owner_session_id = sqlc.narg(owner_session_id))
 AND (CAST(sqlc.narg(origin_chat_id) AS TEXT) IS NULL OR origin_chat_id = sqlc.narg(origin_chat_id))
 AND (CAST(sqlc.narg(state) AS TEXT) IS NULL OR state = sqlc.narg(state))
 AND (CAST(sqlc.narg(trigger_event) AS TEXT) IS NULL OR trigger_event = sqlc.narg(trigger_event))
ORDER BY created_at ASC, id ASC LIMIT sqlc.arg(row_limit);
