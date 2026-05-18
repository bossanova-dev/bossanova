-- +goose Up

ALTER TABLE sessions ADD COLUMN last_observed_review_state INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE sessions DROP COLUMN last_observed_review_state;
