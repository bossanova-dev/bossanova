-- +goose Up
ALTER TABLE agent_accounts ADD COLUMN synced_credential_version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE agent_accounts DROP COLUMN synced_credential_version;
