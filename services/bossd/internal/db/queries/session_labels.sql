-- name: CreateSessionLabel :one
INSERT INTO session_labels (id, name, color, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetSessionLabel :one
SELECT * FROM session_labels WHERE id = ?;

-- name: GetSessionLabelByName :one
SELECT * FROM session_labels WHERE name = ? COLLATE NOCASE;

-- name: ListSessionLabels :many
SELECT
    l.id,
    l.name,
    l.color,
    l.description,
    l.created_at,
    l.updated_at,
    CAST(COUNT(a.session_id) AS INTEGER) AS session_count
FROM session_labels l
LEFT JOIN session_label_assignments a ON a.label_id = l.id
GROUP BY l.id
ORDER BY LOWER(l.name) ASC, l.id ASC;

-- name: UpdateSessionLabel :one
UPDATE session_labels
SET
    name = COALESCE(sqlc.narg(name), name),
    color = COALESCE(sqlc.narg(color), color),
    description = COALESCE(sqlc.narg(description), description),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteSessionLabel :execrows
DELETE FROM session_labels WHERE id = ?;

-- name: ListSessionIDsByLabel :many
SELECT session_id FROM session_label_assignments WHERE label_id = ? ORDER BY session_id ASC;

-- name: DeleteSessionLabelAssignments :exec
DELETE FROM session_label_assignments WHERE session_id = ?;

-- name: CreateSessionLabelAssignment :exec
INSERT OR IGNORE INTO session_label_assignments (session_id, label_id, created_at)
VALUES (?, ?, ?);

-- name: ListLabelsForSession :many
SELECT l.*
FROM session_labels l
INNER JOIN session_label_assignments a ON a.label_id = l.id
WHERE a.session_id = ?
ORDER BY LOWER(l.name) ASC, l.id ASC;

-- name: ListLabelsForSessions :many
SELECT a.session_id, l.id, l.name, l.color, l.description, l.created_at, l.updated_at
FROM session_label_assignments a
INNER JOIN session_labels l ON l.id = a.label_id
WHERE a.session_id IN (sqlc.slice('session_ids'))
ORDER BY a.session_id ASC, LOWER(l.name) ASC, l.id ASC;
