-- +goose Up
-- Effective runtime values are populated for sessions created after this
-- migration. Existing rows keep '' because historical effort is not
-- recoverable from the session row alone.
ALTER TABLE sessions ADD COLUMN effective_model TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN effective_effort TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN effective_effort;
ALTER TABLE sessions DROP COLUMN effective_model;
