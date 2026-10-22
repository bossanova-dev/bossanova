-- name: UpdateSessionStateConditional :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(new_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(new_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state = CAST(sqlc.arg(expected_state) AS INTEGER);

-- name: UpdateSessionLinkTrackerIfUnlinked :execresult
UPDATE sessions
SET tracker_id = sqlc.arg(tracker_id),
    tracker_url = sqlc.arg(tracker_url),
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
  AND is_quick_chat = 0
  AND COALESCE(tracker_url, '') = ''
  AND COALESCE(tracker_id, '') = sqlc.arg(expected_tracker_id);

-- name: UpdateSessionOrphanHeadlessRun :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(orphaned_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(orphaned_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    blocked_reason = sqlc.arg(reason),
    rotation_resume_at = NULL,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER);

-- name: UpdateSessionClaimUnarchivedOrphan :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(implementing_plan_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state = CAST(sqlc.arg(orphaned_state) AS INTEGER)
  AND archived_at IS NULL
  AND blocked_reason = sqlc.arg(reason);

-- name: UpdateSessionResurrectToState :execresult
UPDATE sessions
SET archived_at = NULL,
    run_blocked_reason = NULL,
    state = CAST(sqlc.arg(new_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(new_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    rotation_resume_at = CASE WHEN CAST(sqlc.arg(new_state) AS INTEGER) = CAST(sqlc.arg(blocked_state) AS INTEGER) THEN rotation_resume_at ELSE NULL END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND archived_at IS NOT NULL;

-- name: UpdateSessionRollbackFailedResurrect :execresult
UPDATE sessions
SET archived_at = sqlc.arg(archived_at),
    state = CAST(sqlc.arg(restore_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(restore_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
  AND state = CAST(sqlc.arg(expect_state) AS INTEGER);

-- name: UpdateSessionCommitOrphanResume :execresult
UPDATE sessions
SET agent_session_id = sqlc.arg(new_agent_session_id),
    blocked_reason = NULL,
    rotation_resume_at = NULL,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER)
  AND archived_at IS NULL
  AND blocked_reason = sqlc.arg(reason)
  AND agent_session_id IS sqlc.narg(prior_agent_session);

-- name: UpdateSessionReleaseOrphanResumeClaim :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(orphaned_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(orphaned_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER)
  AND archived_at IS NULL
  AND (blocked_reason = sqlc.arg(reason) OR blocked_reason IS NULL)
  AND agent_session_id IS sqlc.narg(prior_agent_session);

-- name: UpdateSessionReparkOrphanResume :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(orphaned_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(orphaned_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    agent_session_id = sqlc.narg(prior_agent_session),
    blocked_reason = sqlc.arg(reason),
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER)
  AND archived_at IS NULL
  AND blocked_reason IS NULL
  AND agent_session_id = sqlc.arg(new_agent_session_id);

-- name: UpdateSessionAdvanceOrphanedSessions :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(awaiting_checks_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(awaiting_checks_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    updated_at = sqlc.arg(now)
WHERE state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER)
  AND is_quick_chat = 0
  AND NOT EXISTS (
		       SELECT 1 FROM workflows
WHERE workflows.session_id = sessions.id
  AND workflows.status IN ('running', 'pending')
		   );

-- name: UpdateSessionCommitRunResume :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(implementing_plan_state) AS INTEGER),
    state_entered_at = sqlc.arg(now),
    agent_session_id = sqlc.arg(new_agent_id),
    blocked_reason = NULL,
    run_blocked_reason = NULL,
    rotation_resume_at = NULL,
    is_automation_enabled = 1,
    run_dropped_at = NULL,
    run_dropped_agent_session_id = NULL,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
  AND state = CAST(sqlc.arg(expected_state) AS INTEGER)
  AND agent_session_id IS sqlc.narg(prior_agent_id);

-- name: UpdateSessionGiveUpRunResume :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(blocked_state) AS INTEGER),
    state_entered_at = sqlc.arg(now),
    blocked_reason = sqlc.arg(reason),
    updated_at = sqlc.arg(now)
WHERE sessions.id = sqlc.arg(id)
  AND archived_at IS NULL
  AND state = CAST(sqlc.arg(expected_state) AS INTEGER)
  AND sessions.agent_session_id IS sqlc.narg(prior_agent_id)
  AND NOT EXISTS (
      SELECT 1 FROM agent_runs
      WHERE session_id = sessions.id
        AND agent_session_id IS sqlc.narg(prior_agent_id)
        AND terminal_state != ''
  );

-- name: UpdateSessionObserveRunDeath :execresult
UPDATE sessions
SET run_dropped_at = CASE WHEN run_dropped_agent_session_id = sqlc.arg(agent_id) AND run_dropped_at IS NOT NULL THEN run_dropped_at ELSE sqlc.arg(stamp) END,
    run_dropped_agent_session_id = sqlc.arg(agent_id)
WHERE id = sqlc.arg(id)
  AND agent_session_id = sqlc.arg(agent_id)
  AND state = CAST(sqlc.arg(expected_state) AS INTEGER)
  AND archived_at IS NULL;

-- name: UpdateSessionStateConditionalFrom :execresult
UPDATE sessions
SET state = CAST(sqlc.arg(new_state) AS INTEGER),
    state_entered_at = CASE WHEN state != CAST(sqlc.arg(new_state) AS INTEGER) THEN sqlc.arg(now) ELSE state_entered_at END,
    rotation_resume_at = CASE WHEN CAST(sqlc.arg(new_state) AS INTEGER) = CAST(sqlc.arg(blocked_state) AS INTEGER) THEN rotation_resume_at ELSE NULL END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND state IN (sqlc.slice('expected_states'));

-- name: GetSessionRunDeathObservation :one
SELECT CAST(run_dropped_at AS TEXT) AS observed FROM sessions
WHERE id = sqlc.arg(id)
  AND run_dropped_agent_session_id = sqlc.narg(agent_id);
