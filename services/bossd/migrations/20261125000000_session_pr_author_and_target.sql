-- +goose Up
-- The PR author's GitHub login and the PR's last observed base branch, read
-- from the open-PR listing bossd already polls. Stored so both survive a merge,
-- a close and a daemon restart. base_branch stays the creation-time base.
ALTER TABLE sessions ADD COLUMN pr_author_login TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN pr_target_branch TEXT NOT NULL DEFAULT '';
-- A merged PR never reappears in the open-PR listing, so its creation-time base
-- is the best target known. An open PR overwrites it on the next poll. The
-- author of an already-merged PR cannot be recovered and stays empty.
UPDATE sessions SET pr_target_branch = base_branch WHERE pr_number IS NOT NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN pr_target_branch;
ALTER TABLE sessions DROP COLUMN pr_author_login;
