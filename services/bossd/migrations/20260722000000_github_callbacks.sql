-- +goose Up
-- BOS-467: durable one-shot GitHub callback registry.
--
-- A callback is registered against a repository pull request + trigger event and
-- carries a chat message to deliver when that event fires. Callbacks may belong
-- to a group; the first sibling to trigger atomically cancels the others. The
-- lifecycle is active -> leased -> triggered -> delivered, with canceled and
-- expired as terminal states. Timestamps follow the BOS-14 naming standard:
-- TEXT ISO-8601 millisecond UTC (sqlutil.TimeLayout); non-null created/updated
-- default via strftime; nullable _at columns are plain TEXT.
--
-- No foreign keys: target_chat_id and repo owner/name are logical references to
-- entities whose lifecycle is not co-owned by this table (a callback can outlive,
-- or be registered ahead of, the referenced chat), so a cascading DELETE would be
-- wrong. Expiry/cancellation, not referential cascade, retires stale rows.

CREATE TABLE github_callbacks (
    id                TEXT PRIMARY KEY,
    group_id          TEXT,
    target_chat_id    TEXT NOT NULL,
    repo_owner        TEXT NOT NULL,
    repo_name         TEXT NOT NULL,
    pr_number         INTEGER NOT NULL,
    trigger_event     TEXT NOT NULL,
    state             TEXT NOT NULL DEFAULT 'active',
    message           TEXT NOT NULL,
    lease_owner       TEXT,
    lease_deadline_at TEXT,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   TEXT,
    triggered_at      TEXT,
    delivered_at      TEXT,
    last_error        TEXT,
    last_event        TEXT,
    expires_at        TEXT NOT NULL,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Active callbacks matched by an incoming webhook (repo + PR + trigger).
CREATE INDEX idx_github_callbacks_repo_pr_trigger
    ON github_callbacks(repo_owner, repo_name, pr_number, trigger_event);

-- Listing a target chat's callbacks in creation order.
CREATE INDEX idx_github_callbacks_chat_created
    ON github_callbacks(target_chat_id, created_at);

-- Claimable delivery scan: state + retry backoff + lease recovery.
CREATE INDEX idx_github_callbacks_claimable
    ON github_callbacks(state, next_attempt_at, lease_deadline_at);

-- Sibling lookup for atomic group cancellation.
CREATE INDEX idx_github_callbacks_group
    ON github_callbacks(group_id);

-- +goose Down

DROP TABLE IF EXISTS github_callbacks;
