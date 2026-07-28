-- +goose Up
-- Whether the daemon proactively rebases this repo's in-flight session branches
-- onto the base branch each time a merge advances it (BOS-521 keep-current
-- sweep). Opt-in: every rebase re-runs CI, so the trade (CI churn for merge
-- safety) is one an operator makes deliberately. Defaults OFF (0), matching the
-- proto3 bool zero value, so existing repos are unaffected by this migration.
ALTER TABLE repos ADD COLUMN should_keep_branches_current INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE repos DROP COLUMN should_keep_branches_current;
