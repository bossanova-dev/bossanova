-- name: CreateNote :one
INSERT OR IGNORE INTO notes (id,repo_id,session_id,chat_id,body,idempotency_key,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?) RETURNING *;

-- name: GetNote :one
SELECT * FROM notes WHERE id = ?;

-- name: GetNoteByIdempotencyKey :one
SELECT * FROM notes WHERE repo_id = ? AND idempotency_key = ?;

-- Positional scalar bindings avoid sqlc's numbered-placeholder/slice interaction.
-- name: ListNotes :many
SELECT * FROM notes
WHERE (NOT CAST(? AS BOOLEAN) OR repo_id = ?)
AND (NOT CAST(? AS BOOLEAN) OR session_id = ?)
AND (NOT CAST(? AS BOOLEAN) OR chat_id = ?)
AND (NOT CAST(? AS BOOLEAN) OR like(CAST(? AS TEXT), LOWER(body), '\'))
AND (NOT CAST(? AS BOOLEAN) OR id IN (SELECT note_id FROM note_tags WHERE tag IN (sqlc.slice('tags'))))
ORDER BY created_at ASC,id ASC LIMIT ?;

-- name: UpdateNote :one
UPDATE notes SET body=COALESCE(sqlc.narg(body),body), updated_at=sqlc.arg(updated_at)
WHERE id=sqlc.arg(id) RETURNING *;

-- name: DeleteNote :one
DELETE FROM notes WHERE id=? RETURNING repo_id,created_at;

-- name: CreateNoteTag :exec
INSERT INTO note_tags(note_id,tag) VALUES (?,?);

-- name: DeleteNoteTags :exec
DELETE FROM note_tags WHERE note_id=?;

-- name: ListNoteTags :many
SELECT tag FROM note_tags WHERE note_id=? ORDER BY tag ASC;

-- name: ListNoteTagsByNoteIDs :many
SELECT note_id,tag FROM note_tags WHERE note_id IN (sqlc.slice('note_ids')) ORDER BY note_id ASC,tag ASC;

-- name: DeleteExpiredNotes :exec
DELETE FROM notes WHERE repo_id=? AND created_at<?;

-- name: DeleteOverCapNotes :exec
DELETE FROM notes WHERE id IN (SELECT notes.id FROM notes WHERE notes.repo_id=? AND notes.id<>? ORDER BY notes.created_at DESC,notes.id DESC LIMIT -1 OFFSET ?);

-- name: DeleteExpiredNoteSyncStates :exec
DELETE FROM note_sync_states WHERE note_id IN (SELECT notes.id FROM notes WHERE notes.repo_id=? AND notes.created_at<?);

-- name: DeleteOverCapNoteSyncStates :exec
DELETE FROM note_sync_states WHERE note_id IN (SELECT notes.id FROM notes WHERE notes.repo_id=? AND notes.id<>? ORDER BY notes.created_at DESC,notes.id DESC LIMIT -1 OFFSET ?);
