-- name: CreateCheckSnapshot :exec
INSERT INTO session_check_snapshots (session_id, polled_at, head_sha, raw_json, computed_status)
VALUES (?, ?, ?, ?, ?);

-- name: ListCheckSnapshotsBySession :many
SELECT * FROM session_check_snapshots WHERE session_id = ? ORDER BY polled_at DESC, id DESC LIMIT ?;

-- name: ListCheckSnapshotsBySessionHead :many
SELECT * FROM session_check_snapshots WHERE session_id = ? AND head_sha = ? ORDER BY polled_at ASC, id ASC;

-- name: GetNextCheckSnapshotBySessionHead :one
SELECT * FROM session_check_snapshots
WHERE session_id = @session_id AND head_sha = @head_sha
AND (polled_at, id) < (sqlc.arg(before_polled_at), CAST(sqlc.arg(before_id) AS INTEGER))
ORDER BY polled_at DESC, id DESC LIMIT 1;

-- name: GetNextCheckSnapshotSummaryBySessionHead :one
SELECT id, session_id, polled_at, head_sha, computed_status FROM session_check_snapshots
WHERE session_id = @session_id AND head_sha = @head_sha
AND (polled_at, id) < (sqlc.arg(before_polled_at), CAST(sqlc.arg(before_id) AS INTEGER))
ORDER BY polled_at DESC, id DESC LIMIT 1;

-- name: GetNewestCheckSnapshotBySessionHead :one
SELECT * FROM session_check_snapshots
WHERE session_id = ? AND head_sha = ?
ORDER BY polled_at DESC, id DESC LIMIT 1;

-- name: GetNewestCheckSnapshotSummaryBySessionHead :one
SELECT id, session_id, polled_at, head_sha, computed_status FROM session_check_snapshots
WHERE session_id = ? AND head_sha = ?
ORDER BY polled_at DESC, id DESC LIMIT 1;
