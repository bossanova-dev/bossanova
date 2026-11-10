-- +goose Up
-- Legacy queues have unknown provenance; treat them as cloud-controlled.
-- New local sends explicitly write zero.
ALTER TABLE broadcasts ADD COLUMN is_cloud_controlled INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE broadcasts DROP COLUMN is_cloud_controlled;
