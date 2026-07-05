-- +goose Up

-- TestAccount bookkeeping: record the outcome of the last credential test so
-- the registry can surface health without re-running a smoke on every read.
-- Both columns are additive and nullable/defaulted so existing rows migrate
-- cleanly. last_test_ok_at is a nullable ISO-8601 timestamp (NULL = never
-- passed a test); last_test_error holds the most recent failure detail
-- ('' = no error / last test passed).
ALTER TABLE accounts ADD COLUMN last_test_ok_at TEXT;              -- nullable ISO-8601; NULL = never OK
ALTER TABLE accounts ADD COLUMN last_test_error TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE accounts DROP COLUMN last_test_error;
ALTER TABLE accounts DROP COLUMN last_test_ok_at;
