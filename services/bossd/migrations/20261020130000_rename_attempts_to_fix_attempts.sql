-- +goose Up
ALTER TABLE attempts RENAME TO fix_attempts;
DROP INDEX IF EXISTS idx_attempts_session_id;
CREATE INDEX idx_fix_attempts_session_id ON fix_attempts(session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_fix_attempts_session_id;
ALTER TABLE fix_attempts RENAME TO attempts;
CREATE INDEX idx_attempts_session_id ON attempts(session_id);
