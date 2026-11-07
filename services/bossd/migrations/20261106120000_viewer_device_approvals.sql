-- +goose Up
CREATE TABLE viewer_device_approvals (
    viewer_device_id TEXT NOT NULL PRIMARY KEY,
    public_key BLOB NOT NULL CHECK (typeof(public_key) = 'blob' AND length(public_key) = 32),
    label TEXT NOT NULL DEFAULT '',
    approved_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE viewer_device_approvals;
