-- +goose Up

-- Records why a session's configured setup script failed. A setup-script
-- failure is non-fatal (the worktree is still usable), so the session starts
-- in a degraded/flagged state with the failure captured here for the TUI and
-- `boss show` to surface. Empty for clean runs or repos with no setup script.
ALTER TABLE sessions ADD COLUMN setup_error TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE sessions DROP COLUMN setup_error;
