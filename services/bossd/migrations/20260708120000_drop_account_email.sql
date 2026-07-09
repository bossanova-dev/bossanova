-- +goose Up
-- Drop account_email, which was informational metadata redundant with label.
CREATE TABLE accounts_new (
    id                TEXT PRIMARY KEY,
    provider          TEXT NOT NULL,                        -- 'claude' | 'codex'
    label             TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'active',       -- 'active' | 'disabled'
    priority          INTEGER NOT NULL DEFAULT 0,           -- sort order; lower = preferred
    health            TEXT NOT NULL DEFAULT 'ok',           -- 'ok' | 'failed'
    cooldown_until    TEXT,                                 -- nullable ISO-8601
    last_used_at      TEXT,                                 -- nullable ISO-8601
    tier              TEXT NOT NULL DEFAULT '',
    allowed_models    TEXT NOT NULL DEFAULT '',             -- JSON array of model ids; '' = unspecified
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_test_ok_at   TEXT,                                 -- nullable ISO-8601; NULL = never OK
    last_test_error   TEXT NOT NULL DEFAULT '',
    usage_util_5h     REAL NOT NULL DEFAULT 0,
    usage_util_7d     REAL NOT NULL DEFAULT 0,
    usage_reset_5h    TEXT,
    usage_reset_7d    TEXT,
    usage_status      TEXT NOT NULL DEFAULT '',
    usage_plan_tier   TEXT NOT NULL DEFAULT '',
    usage_fetched_at  TEXT,
    UNIQUE (provider, label)
);

INSERT INTO accounts_new (
    id, provider, label, status, priority, health, cooldown_until,
    last_used_at, tier, allowed_models, created_at, updated_at,
    last_test_ok_at, last_test_error, usage_util_5h, usage_util_7d,
    usage_reset_5h, usage_reset_7d, usage_status, usage_plan_tier,
    usage_fetched_at
)
SELECT
    id, provider, label, status, priority, health, cooldown_until,
    last_used_at, tier, allowed_models, created_at, updated_at,
    last_test_ok_at, last_test_error, usage_util_5h, usage_util_7d,
    usage_reset_5h, usage_reset_7d, usage_status, usage_plan_tier,
    usage_fetched_at
FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
CREATE INDEX idx_accounts_provider_priority ON accounts(provider, priority);

-- +goose Down
-- Non-reversible: account_email values were discarded. Recreate the column
-- with an empty default so older binaries can start after a rollback.
ALTER TABLE accounts ADD COLUMN account_email TEXT NOT NULL DEFAULT '';
