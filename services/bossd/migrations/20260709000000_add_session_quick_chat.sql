-- +goose Up

ALTER TABLE sessions ADD COLUMN quick_chat INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE sessions DROP COLUMN quick_chat;
