-- +goose Up
ALTER TABLE repos ADD COLUMN serve_script TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN serve_script;
