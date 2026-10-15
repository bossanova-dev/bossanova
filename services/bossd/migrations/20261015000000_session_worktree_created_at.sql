-- +goose Up
ALTER TABLE sessions ADD COLUMN worktree_created_at TEXT;
ALTER TABLE sessions ADD COLUMN is_branch_created INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN is_branch_created;
ALTER TABLE sessions DROP COLUMN worktree_created_at;
