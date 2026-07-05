-- +goose Up

ALTER TABLE sessions ADD COLUMN tmux_unattended INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE sessions DROP COLUMN tmux_unattended;
