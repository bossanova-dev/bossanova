-- name: CreateAgentRun :exec
INSERT INTO agent_runs
		   (id, session_id, agent_session_id, agent_name, model, effort, started_at, stopped_at,
		    stop_reason, parent_model_call_count, child_model_call_count, tool_call_count,
		    subagent_count, direct_subagent_count, output_token_count, reasoning_token_count,
		    reviewer_dispatch_count, terminal_state, is_backfilled, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: MarkAgentRunStoppedByAgentSessionID :execrows
UPDATE agent_runs
		 SET stopped_at = ?, stop_reason = ?, updated_at = ?
		 WHERE agent_session_id = ? AND stopped_at IS NULL;

-- name: MarkAgentRunStopped :execrows
UPDATE agent_runs
		 SET stopped_at = ?, stop_reason = ?, updated_at = ?
		 WHERE id = ? AND stopped_at IS NULL;

-- name: RecordAgentRunTelemetry :execrows
UPDATE agent_runs
		 SET parent_model_call_count = sqlc.arg(parent_model_call_count), child_model_call_count = sqlc.arg(child_model_call_count), tool_call_count = sqlc.arg(tool_call_count),
		     subagent_count = sqlc.arg(subagent_count), direct_subagent_count = sqlc.arg(direct_subagent_count), output_token_count = sqlc.arg(output_token_count),
		     reasoning_token_count = sqlc.arg(reasoning_token_count),
		     -- A recorder without review fields must preserve earlier review telemetry.
		     reviewer_dispatch_count = CASE WHEN CAST(sqlc.arg(reviewer_dispatch_count) AS INTEGER) = 0 THEN reviewer_dispatch_count ELSE sqlc.arg(reviewer_dispatch_count) END,
		     terminal_state = CASE WHEN CAST(sqlc.arg(terminal_state) AS TEXT) = '' THEN terminal_state ELSE sqlc.arg(terminal_state) END,
		     updated_at = sqlc.arg(updated_at)
		 WHERE id = sqlc.arg(id);

-- name: DeleteAgentRunChildren :exec
DELETE FROM agent_run_children WHERE agent_run_id = ?;

-- name: CreateAgentRunChild :exec
INSERT INTO agent_run_children
			   (id, agent_run_id, agent_session_id, parent_agent_id, spawn_depth, started_at,
			    stopped_at, model_call_count, tool_call_count, output_token_count, reasoning_token_count)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetLatestAgentRunID :one
SELECT id FROM agent_runs
		 WHERE agent_session_id = ?
		 ORDER BY stopped_at IS NOT NULL, started_at DESC, id DESC
		 LIMIT 1;

-- name: MarkOpenAgentRunsStopped :execrows
UPDATE agent_runs SET stopped_at = ?, stop_reason = ?, updated_at = ? WHERE stopped_at IS NULL AND (CAST(sqlc.arg(no_active) AS BOOLEAN) OR agent_session_id NOT IN (sqlc.slice('active_ids')));

-- name: ListAgentRuns :many
SELECT ar.id, ar.session_id, ar.agent_session_id, CAST(COALESCE(NULLIF(ar.agent_name, ''), s.agent_name) AS TEXT) AS agent_name, ar.model, ar.effort,
		        ar.started_at, ar.stopped_at, ar.stop_reason, ar.parent_model_call_count,
		        ar.child_model_call_count, ar.tool_call_count, ar.subagent_count,
		        ar.direct_subagent_count, ar.output_token_count, ar.reasoning_token_count,
		        ar.reviewer_dispatch_count, ar.terminal_state,
		        ar.is_backfilled, CAST(COALESCE(r.display_name, '') AS TEXT) AS repo_display_name
		 FROM agent_runs ar
		 JOIN sessions s ON s.id = ar.session_id
		 LEFT JOIN repos r ON r.id = s.repo_id WHERE (CAST(sqlc.narg(filter_session_id) AS TEXT) IS NULL OR ar.session_id = sqlc.narg(filter_session_id))
AND (CAST(sqlc.narg(filter_agent_name) AS TEXT) IS NULL OR COALESCE(NULLIF(ar.agent_name, ''), s.agent_name) = sqlc.narg(filter_agent_name))
AND (CAST(sqlc.narg(filter_repo_display_name) AS TEXT) IS NULL OR r.display_name = sqlc.narg(filter_repo_display_name))
AND (CAST(sqlc.narg(filter_since) AS TEXT) IS NULL OR ar.started_at >= sqlc.narg(filter_since))
AND (CAST(sqlc.narg(filter_until) AS TEXT) IS NULL OR ar.started_at < sqlc.narg(filter_until))
AND (CAST(sqlc.arg(include_open) AS BOOLEAN) OR ar.stopped_at IS NOT NULL)
AND (CAST(sqlc.arg(include_all_reasons) AS BOOLEAN) OR (CAST(sqlc.arg(include_open) AS BOOLEAN) AND ar.stopped_at IS NULL) OR ar.stop_reason IN ('clean','stopped','usage_exhausted','rate_limited'))
AND (CAST(sqlc.arg(include_backfilled) AS BOOLEAN) OR ar.is_backfilled = 0)
ORDER BY ar.started_at DESC, ar.id DESC;

-- name: ListAgentRunChildren :many
SELECT * FROM agent_run_children WHERE agent_run_id = ? ORDER BY started_at, id;

-- name: ListAgentRunBackfillSessions :many
SELECT sessions.id,
		       sessions.agent_session_id,
		       CAST(COALESCE(agent_chats.provider_session_id, '') AS TEXT) AS provider_session_id,
		       CAST(COALESCE(NULLIF(agent_chats.agent_name, ''), '') AS TEXT) AS provider_agent_name,
		       CAST(COALESCE(NULLIF(agent_chats.model, ''), '') AS TEXT) AS provider_model,
		       sessions.worktree_path,
		       sessions.agent_name,
		       CAST(COALESCE(NULLIF(sessions.effective_model, ''), sessions.model) AS TEXT) AS model,
		       sessions.effective_effort,
		       sessions.created_at,
		       sessions.archived_at
		  FROM sessions
		  LEFT JOIN agent_chats
		    ON agent_chats.session_id = sessions.id
		   AND agent_chats.provider_session_id IS NOT NULL
		   AND agent_chats.provider_session_id != ''
		 ORDER BY sessions.created_at, sessions.id, agent_chats.created_at;

-- name: GetExistingAgentRunBackfill :one
SELECT 1 FROM agent_runs
		 WHERE agent_runs.session_id = ? AND agent_runs.agent_name = ?
		   AND (agent_runs.agent_session_id = ? OR EXISTS (
		     SELECT 1 FROM agent_chats
		      WHERE agent_chats.session_id = agent_runs.session_id
		        AND agent_chats.agent_session_id = agent_runs.agent_session_id
		        AND agent_chats.provider_session_id = ?
		   ))
		   AND agent_runs.started_at >= ? AND agent_runs.started_at <= ?
		 LIMIT 1;
