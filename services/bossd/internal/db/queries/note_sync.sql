-- A new local payload resets retry history; only deletes supply tombstone routing metadata.
-- name: RecordNoteChange :execrows
INSERT INTO note_sync_states
    (note_id, source_version, is_deleted, sync_state, repo_id, note_created_at, created_at, updated_at)
VALUES (sqlc.arg(note_id), 1, sqlc.arg(is_deleted), 'pending', sqlc.narg(repo_id), sqlc.narg(note_created_at), sqlc.arg(created_at), sqlc.arg(updated_at))
ON CONFLICT(note_id) DO UPDATE SET
    source_version = source_version + 1,
    is_deleted = excluded.is_deleted,
    sync_state = 'pending',
    next_attempt_at = NULL,
    attempt_count = 0,
    last_error = NULL,
    repo_id = COALESCE(excluded.repo_id, repo_id),
    note_created_at = COALESCE(excluded.note_created_at, note_created_at),
    updated_at = excluded.updated_at;

-- name: GetNoteSyncStateByNoteID :one
SELECT note_id,sync_state,source_version,synced_version,is_deleted,attempt_count,next_attempt_at,last_attempted_at,synced_at,organization_id,last_error FROM note_sync_states WHERE note_id = sqlc.arg(note_id);

-- name: ListNoteSyncStatesByNoteIDs :many
SELECT * FROM note_sync_states WHERE note_id IN (sqlc.slice('note_ids'));

-- Retry transient outcomes and policy outcomes that a plan or mapping change can resolve.
-- Orphan live rows are skipped because there is no payload to send.
-- name: ListDueNoteSyncChanges :many
SELECT s.* FROM note_sync_states s
WHERE s.sync_state IN ('pending', 'rate_limited', 'failed', 'not_entitled', 'refused')
    AND (s.next_attempt_at IS NULL OR s.next_attempt_at <= sqlc.narg(next_attempt_at))
    AND (s.is_deleted = 1 OR EXISTS (SELECT 1 FROM notes n WHERE n.id = s.note_id))
ORDER BY s.updated_at ASC, s.note_id ASC
LIMIT sqlc.arg(limit);

-- name: MarkNoteSyncAttempt :execrows
UPDATE note_sync_states
SET attempt_count = attempt_count + 1, last_attempted_at = sqlc.narg(last_attempted_at)
WHERE note_id = sqlc.arg(note_id);

-- name: RecordNoteSyncSuccess :execrows
UPDATE note_sync_states
SET sync_state = 'synced', synced_version = sqlc.arg(synced_version), synced_at = sqlc.narg(synced_at),
    last_error = NULL, next_attempt_at = NULL,
    organization_id = COALESCE(sqlc.narg(organization_id), organization_id), updated_at = sqlc.arg(updated_at)
WHERE note_id = sqlc.arg(note_id) AND source_version = sqlc.arg(source_version);

-- name: RecordNoteSyncFailure :execrows
UPDATE note_sync_states
SET sync_state = sqlc.arg(sync_state), last_error = sqlc.narg(last_error), next_attempt_at = sqlc.narg(next_attempt_at),
    organization_id = COALESCE(sqlc.narg(organization_id), organization_id), updated_at = sqlc.arg(updated_at)
WHERE note_id = sqlc.arg(note_id) AND source_version = sqlc.arg(source_version);

-- name: CountNoteSyncStatesByState :many
SELECT sync_state, COUNT(*) AS count FROM note_sync_states GROUP BY sync_state;

-- name: DeleteSettledNoteSyncTombstones :execrows
DELETE FROM note_sync_states
WHERE (is_deleted = 1 AND (sync_state IN ('synced', 'suppressed') OR note_sync_states.updated_at < sqlc.arg(updated_at)))
    OR (is_deleted = 0 AND NOT EXISTS (SELECT 1 FROM notes n WHERE n.id = note_sync_states.note_id));
