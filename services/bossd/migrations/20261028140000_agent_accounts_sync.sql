-- +goose Up
ALTER TABLE agent_accounts ADD COLUMN synced_agent_account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN sync_version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE agent_accounts DROP COLUMN sync_version;
ALTER TABLE agent_accounts DROP COLUMN synced_agent_account_id;
