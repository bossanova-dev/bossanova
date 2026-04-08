-- +goose Up
ALTER TABLE sessions ADD COLUMN tracker_id TEXT;
ALTER TABLE sessions ADD COLUMN tracker_url TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN tracker_url;
ALTER TABLE sessions DROP COLUMN tracker_id;
