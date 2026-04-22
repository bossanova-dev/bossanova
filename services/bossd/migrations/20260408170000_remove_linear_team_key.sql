-- +goose Up
-- NON-REVERSIBLE: this migration drops linear_team_key. The down step
-- re-adds the column but cannot restore its previous values — any data in
-- linear_team_key at the time the up ran is permanently lost. Operators who
-- need the data back must restore from backup.
--
-- Recreate repos table without linear_team_key to support SQLite < 3.35
-- which does not have DROP COLUMN.
CREATE TABLE repos_new (
    id                             TEXT PRIMARY KEY,
    display_name                   TEXT NOT NULL,
    local_path                     TEXT NOT NULL UNIQUE,
    origin_url                     TEXT NOT NULL,
    default_base_branch            TEXT NOT NULL DEFAULT 'main',
    worktree_base_dir              TEXT NOT NULL,
    setup_script                   TEXT,
    created_at                     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at                     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    can_auto_merge                 INTEGER NOT NULL DEFAULT 0,
    can_auto_merge_dependabot      INTEGER NOT NULL DEFAULT 1,
    can_auto_address_reviews       INTEGER NOT NULL DEFAULT 1,
    can_auto_resolve_conflicts     INTEGER NOT NULL DEFAULT 1,
    merge_strategy                 TEXT NOT NULL DEFAULT 'merge',
    linear_api_key                 TEXT NOT NULL DEFAULT ''
);

INSERT INTO repos_new (
    id, display_name, local_path, origin_url, default_base_branch,
    worktree_base_dir, setup_script, created_at, updated_at,
    can_auto_merge, can_auto_merge_dependabot, can_auto_address_reviews,
    can_auto_resolve_conflicts, merge_strategy, linear_api_key
)
SELECT
    id, display_name, local_path, origin_url, default_base_branch,
    worktree_base_dir, setup_script, created_at, updated_at,
    can_auto_merge, can_auto_merge_dependabot, can_auto_address_reviews,
    can_auto_resolve_conflicts, merge_strategy, linear_api_key
FROM repos;

DROP TABLE repos;
ALTER TABLE repos_new RENAME TO repos;

-- +goose Down
-- No-op: restoring the column without its data would be misleading. See the
-- NON-REVERSIBLE note in the up section above. If a rollback is unavoidable,
-- run `ALTER TABLE repos ADD COLUMN linear_team_key TEXT NOT NULL DEFAULT ''`
-- manually after restoring the pre-migration data from backup.
SELECT 1;
