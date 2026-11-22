-- name: ListRecentModelsByAgent :many
-- Timestamps are fixed-width ISO-8601 text, so MAX orders them by time.
SELECT CAST(model AS TEXT) AS model
FROM (
  SELECT s.model, s.created_at AS used_at FROM sessions AS s
  WHERE s.agent_name = sqlc.arg(agent_name) AND s.model <> ''
  UNION ALL
  SELECT c.model, COALESCE(c.last_run_at, c.updated_at) AS used_at FROM cron_jobs AS c
  WHERE c.agent_name = sqlc.arg(agent_name) AND c.model <> ''
)
GROUP BY model
ORDER BY MAX(used_at) DESC, model ASC
LIMIT sqlc.arg(row_limit);
