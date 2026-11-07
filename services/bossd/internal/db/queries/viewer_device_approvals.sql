-- name: CreateViewerDeviceApproval :exec
INSERT INTO viewer_device_approvals (viewer_device_id, public_key, label, approved_at, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetViewerDeviceApproval :one
SELECT * FROM viewer_device_approvals WHERE viewer_device_id = ?;

-- name: ListViewerDeviceApprovals :many
SELECT * FROM viewer_device_approvals ORDER BY created_at, viewer_device_id;

-- name: DeleteViewerDeviceApproval :exec
DELETE FROM viewer_device_approvals WHERE viewer_device_id = ?;
