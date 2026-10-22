-- +goose Up
DROP INDEX IF EXISTS idx_session_resume_attempts_session_id_run_key_attempt_number;
ALTER TABLE session_resume_attempts RENAME COLUMN run_key TO budget_key;
CREATE UNIQUE INDEX idx_session_resume_attempts_session_id_budget_key_attempt_number
    ON session_resume_attempts(session_id, budget_key, attempt_number);

-- +goose Down
DROP INDEX IF EXISTS idx_session_resume_attempts_session_id_budget_key_attempt_number;
ALTER TABLE session_resume_attempts RENAME COLUMN budget_key TO run_key;
CREATE UNIQUE INDEX idx_session_resume_attempts_session_id_run_key_attempt_number
    ON session_resume_attempts(session_id, run_key, attempt_number);
