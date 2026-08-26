-- +goose Up
-- BOS-995: opt-in transition memory for GitHub callbacks.

ALTER TABLE github_callbacks ADD COLUMN should_require_transition INTEGER NOT NULL DEFAULT 0;
ALTER TABLE github_callbacks ADD COLUMN has_observed_baseline INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE github_callbacks DROP COLUMN has_observed_baseline;
ALTER TABLE github_callbacks DROP COLUMN should_require_transition;
