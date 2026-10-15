-- +goose Up
-- claim_token fences owner writes. Empty tokens belong only to pre-upgrade
-- claims; a daemon restart abandons those owners and a takeover re-tokens them.
-- accepted_session_id durably ties a create to its invocation before bootstrap
-- settles. Like session_id, it has no foreign key: deleting a session must not
-- erase the invocation's idempotency record and permit a duplicate launch.
ALTER TABLE trigger_launches ADD COLUMN claim_token TEXT NOT NULL DEFAULT '';
ALTER TABLE trigger_launches ADD COLUMN accepted_session_id TEXT;

-- +goose Down
ALTER TABLE trigger_launches DROP COLUMN accepted_session_id;
ALTER TABLE trigger_launches DROP COLUMN claim_token;
