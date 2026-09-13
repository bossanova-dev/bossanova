-- +goose Up
-- BOS-1230: an optional manual list rank for the session list.
--
-- Nullable with no default is what makes "an untouched database does not
-- reorder" true by construction: every pre-existing row, and every newly
-- created session, reads back with a NULL rank and therefore keeps its natural
-- created_at DESC position. Only a session the user explicitly moved carries a
-- value, and ranked rows sort as a block ahead of the natural block.
--
-- INTEGER rather than a dense index: the rank is a sparse ordering key, spaced
-- far enough apart that a move writes exactly one row instead of renumbering
-- every sibling.
ALTER TABLE sessions ADD COLUMN list_rank INTEGER;

-- +goose Down
ALTER TABLE sessions DROP COLUMN list_rank;
