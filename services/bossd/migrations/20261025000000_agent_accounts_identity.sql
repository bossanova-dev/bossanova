-- +goose Up
ALTER TABLE agent_accounts ADD COLUMN identity_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN identity_digest TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE agent_accounts DROP COLUMN identity_digest;
ALTER TABLE agent_accounts DROP COLUMN identity_kind;
