-- +goose Up
-- last_attempt_head_sha records the PR head SHA at which the last fix-loop
-- attempt was counted. Nullable: NULL means no attempt has been counted yet
-- (or it was reset on green / auto-unblock). Distinct from last_repair_head_sha
-- (the repair plugin's cooldown lane, which has different reset semantics).
ALTER TABLE sessions ADD COLUMN last_attempt_head_sha TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_attempt_head_sha;
