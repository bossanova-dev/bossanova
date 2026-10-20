-- +goose Up
-- The table holds agent accounts (docs/structure/entities.yaml agent-account);
-- the bare `accounts` name collided with the cloud account entity (BOS-1784).
-- No migration declares a foreign key into it, so it renames in place.
ALTER TABLE accounts RENAME TO agent_accounts;
DROP INDEX IF EXISTS idx_accounts_provider_priority;
CREATE INDEX idx_agent_accounts_provider_priority ON agent_accounts(provider, priority);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_accounts_provider_priority;
ALTER TABLE agent_accounts RENAME TO accounts;
CREATE INDEX idx_accounts_provider_priority ON accounts(provider, priority);
