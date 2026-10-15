-- +goose Up
-- The purpose a chat was created for: build, repair, verify or operator. It is
-- identity, set once at creation, so automation stops inferring a chat's owner
-- from its title, which agents and operators rename. NULL is a legacy row whose
-- purpose is unknown; automation treats it like an operator chat.
ALTER TABLE agent_chats ADD COLUMN role TEXT;

-- Only titles that still carry the creator's exact wording are trusted. A
-- repair chat an agent has since renamed stays NULL.
UPDATE agent_chats SET role = 'repair' WHERE substr(title, 1, 7) = 'Repair:';
UPDATE agent_chats SET role = 'verify' WHERE title = 'verify';

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN role;
