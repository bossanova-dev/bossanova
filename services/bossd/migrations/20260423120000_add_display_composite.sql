-- +goose Up
ALTER TABLE sessions ADD COLUMN display_label   TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN display_intent  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN display_spinner INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN display_label;
ALTER TABLE sessions DROP COLUMN display_intent;
ALTER TABLE sessions DROP COLUMN display_spinner;
