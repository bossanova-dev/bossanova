-- +goose Up
-- Repo API keys move to the OS keyring (BOS-1587). has_keyring_secrets = 1
-- means the row's linear_api_key / sentry_api_key live in the keyring item
-- repo-secrets:<id> and the columns are kept ''. 0 means the columns are
-- authoritative: either the row predates the move (bossd moves it once at
-- startup) or no keyring was available when the key was written. Every
-- existing row backfills to 0, which is exactly "not moved yet".
ALTER TABLE repos ADD COLUMN has_keyring_secrets INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- Keys already moved stay in the keyring; an older binary reads the cleared
-- columns as "no key" until they are re-entered (see the BOS-1587 release note).
ALTER TABLE repos DROP COLUMN has_keyring_secrets;
