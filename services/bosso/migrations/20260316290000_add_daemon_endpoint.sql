-- +goose Up

ALTER TABLE daemons ADD COLUMN endpoint TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE daemons DROP COLUMN endpoint;
