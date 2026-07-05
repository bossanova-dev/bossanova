-- +goose Up
CREATE TABLE accounts (
    id             TEXT PRIMARY KEY,
    provider       TEXT NOT NULL,                        -- 'claude' | 'codex'
    label          TEXT NOT NULL,
    account_email  TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'active',       -- 'active' | 'disabled'
    priority       INTEGER NOT NULL DEFAULT 0,           -- sort order; lower = preferred
    health         TEXT NOT NULL DEFAULT 'ok',           -- 'ok' | 'failed'
    cooldown_until TEXT,                                 -- nullable ISO-8601
    last_used_at   TEXT,                                 -- nullable ISO-8601
    tier           TEXT NOT NULL DEFAULT '',
    allowed_models TEXT NOT NULL DEFAULT '',             -- JSON array of model ids; '' = unspecified
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (provider, label)
);

CREATE INDEX idx_accounts_provider_priority ON accounts(provider, priority);

-- +goose Down
DROP TABLE IF EXISTS accounts;
