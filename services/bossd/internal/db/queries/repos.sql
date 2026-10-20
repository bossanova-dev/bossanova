-- name: CountRepos :one
SELECT count(*) FROM repos;

-- name: ListTrustedRepos :many
SELECT is_trusted FROM repos;
