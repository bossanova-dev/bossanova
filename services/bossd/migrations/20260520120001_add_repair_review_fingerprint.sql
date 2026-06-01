-- +goose Up
-- Persist the review-feedback fingerprint targeted by the last repair run.
-- This lets auto-repair distinguish "same rejected commit, same review input"
-- from "same rejected commit, new review input".
ALTER TABLE sessions ADD COLUMN last_repair_review_fingerprint TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_repair_review_fingerprint;
