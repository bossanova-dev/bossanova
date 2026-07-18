-- +goose Up

ALTER TABLE sessions ADD COLUMN detach INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE sessions DROP COLUMN detach;
