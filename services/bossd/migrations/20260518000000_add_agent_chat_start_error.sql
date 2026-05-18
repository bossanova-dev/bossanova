-- +goose Up
-- start_error captures a short human-readable reason when a chat row's
-- agent failed to start (e.g. SendPlan timeout, hook misconfiguration).
-- Before this column the StartTmuxChat failure paths deleted the row
-- entirely, which left the user with a session that showed
-- "repair failed (321×)" and zero chats — no way to see what each
-- attempt actually tried. Preserving the row + stamping a reason lets
-- the chat list surface a "(failed to start)" badge without losing the
-- historical attempt.
ALTER TABLE agent_chats ADD COLUMN start_error TEXT;

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN start_error;
