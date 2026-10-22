-- name: UpsertPendingTrackerMerge :exec
INSERT INTO pending_tracker_merges (session_id, attempt_count, next_attempt_at, last_reason)
VALUES (sqlc.arg(session_id), sqlc.arg(attempt_count), sqlc.arg(next_attempt_at), sqlc.arg(last_reason))
ON CONFLICT(session_id) DO UPDATE SET
    attempt_count = excluded.attempt_count,
    next_attempt_at = excluded.next_attempt_at,
    last_reason = excluded.last_reason,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now');

-- name: ListPendingTrackerMergesByDueAt :many
SELECT * FROM pending_tracker_merges
WHERE next_attempt_at <= sqlc.arg(next_attempt_at)
ORDER BY next_attempt_at, session_id;

-- name: DeletePendingTrackerMerge :exec
DELETE FROM pending_tracker_merges WHERE session_id = sqlc.arg(session_id);
