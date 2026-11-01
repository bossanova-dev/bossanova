-- +goose Up
ALTER TABLE sessions ADD COLUMN pr_readied_at TEXT;
-- ReadyForReview is state 9; preserve the existing ready sessions on upgrade.
UPDATE sessions SET pr_readied_at = updated_at WHERE state = 9 AND pr_number IS NOT NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN pr_readied_at;
