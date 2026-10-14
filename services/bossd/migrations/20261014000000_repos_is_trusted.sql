-- +goose Up
-- Local trust flag for a registered repo (BOS-1562). A trusted repo's own
-- configuration may steer its agents (Codex project trust) and its sessions
-- receive the global proof secrets; an untrusted one gets neither. The column
-- defaults untrusted so a newly registered repo starts that way, and every
-- repo registered before this migration is backfilled trusted so nothing
-- changes for it. repos has inbound foreign keys, so this is a direct ADD
-- COLUMN rather than a table rebuild.
ALTER TABLE repos ADD COLUMN is_trusted INTEGER NOT NULL DEFAULT 0;
UPDATE repos SET is_trusted = 1;

-- +goose Down
ALTER TABLE repos DROP COLUMN is_trusted;
