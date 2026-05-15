-- +goose Up
-- GetByOrigin first tries the raw stored origin_url before falling back to
-- canonical provider matching for webhook HTML URLs.
CREATE INDEX IF NOT EXISTS idx_repos_origin_url ON repos(origin_url);

-- +goose Down
DROP INDEX IF EXISTS idx_repos_origin_url;
