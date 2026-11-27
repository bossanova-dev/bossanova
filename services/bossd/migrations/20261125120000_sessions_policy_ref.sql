-- +goose Up
-- The origin branch an operator chose as trial verify policy for a verify-only session; '' for none.
ALTER TABLE sessions ADD COLUMN policy_ref TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN policy_ref;
