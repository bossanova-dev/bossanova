-- +goose Up
ALTER TABLE sessions ADD COLUMN tracker_rank_tier INTEGER;
ALTER TABLE sessions ADD COLUMN tracker_rank_tier_source TEXT;
ALTER TABLE sessions ADD COLUMN tracker_rank_deadline_on TEXT;
ALTER TABLE sessions ADD COLUMN tracker_rank_deadline_source TEXT;
ALTER TABLE sessions ADD COLUMN is_tracker_rank_scheduled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN tracker_rank_unblocked_ticket_count INTEGER;
ALTER TABLE sessions ADD COLUMN tracker_ranked_at TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN tracker_ranked_at;
ALTER TABLE sessions DROP COLUMN tracker_rank_unblocked_ticket_count;
ALTER TABLE sessions DROP COLUMN is_tracker_rank_scheduled;
ALTER TABLE sessions DROP COLUMN tracker_rank_deadline_source;
ALTER TABLE sessions DROP COLUMN tracker_rank_deadline_on;
ALTER TABLE sessions DROP COLUMN tracker_rank_tier_source;
ALTER TABLE sessions DROP COLUMN tracker_rank_tier;
