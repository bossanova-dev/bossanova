-- +goose Up
-- BOS-1141: durable, redacted credential-verification state for managed
-- accounts. These columns record ONLY metadata about the last daemon-owned
-- credential check: when it ran, how it classified, and when the account is
-- next eligible for a retry. They never hold credential material, provider
-- log text, or anything derived from a credential blob.
--
-- Deliberately separate from last_test_ok_at / last_test_error (manual
-- TestAccount bookkeeping) and from the usage_* snapshot columns, so a
-- verification result can never overwrite either.
ALTER TABLE accounts ADD COLUMN auth_checked_at TEXT;
ALTER TABLE accounts ADD COLUMN auth_check_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN auth_check_failure_class TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN auth_check_next_retry_at TEXT;

-- +goose Down
ALTER TABLE accounts DROP COLUMN auth_check_next_retry_at;
ALTER TABLE accounts DROP COLUMN auth_check_failure_class;
ALTER TABLE accounts DROP COLUMN auth_check_outcome;
ALTER TABLE accounts DROP COLUMN auth_checked_at;
