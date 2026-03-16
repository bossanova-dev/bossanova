-- +goose Up

-- Webhook configurations map repo origin URLs to HMAC secrets for
-- signature verification. Each provider (github, gitlab) may have
-- different signature schemes.
CREATE TABLE webhook_configs (
    id              TEXT PRIMARY KEY,
    repo_origin_url TEXT NOT NULL,
    provider        TEXT NOT NULL DEFAULT 'github',
    secret          TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE UNIQUE INDEX idx_webhook_configs_repo_provider ON webhook_configs(repo_origin_url, provider);

-- +goose Down

DROP TABLE IF EXISTS webhook_configs;
