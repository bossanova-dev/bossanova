-- +goose Up
-- BOS-554: durable broadcast persistence.
--
-- A broadcast is one message addressed to a selector-resolved audience. The
-- audience is split into a second table so it is materialised once, at resolve
-- time, rather than re-evaluated on every retry: broadcasts holds the intent
-- (selector, message, expiry) and broadcast_deliveries holds one row per
-- resolved target with the lease and backoff columns a delivery worker needs.
-- Lifecycle is pending -> resolved -> completed for a broadcast, with expired
-- and canceled terminal; pending -> leased -> delivered for a delivery, with
-- failed and skipped (target vanished before delivery) terminal.
--
-- Timestamps follow the BOS-14 naming standard: TEXT ISO-8601 millisecond UTC
-- (sqlutil.TimeLayout); non-null created/updated default via strftime; nullable
-- _at columns are plain TEXT.
--
-- Foreign keys are deliberately asymmetric, and the asymmetry is the design:
--
--   * broadcast_deliveries.broadcast_id DOES get a real FK with ON DELETE
--     CASCADE. A delivery is owned by its broadcast and is meaningless without
--     it, so cascading the delete is exactly right. The connection runs with
--     PRAGMA foreign_keys=ON.
--   * broadcast_deliveries.target_chat_id gets NO foreign key, for the same
--     reason github_callbacks.target_chat_id has none (see
--     20260722000000_github_callbacks.sql): it is a logical reference to a chat
--     whose lifecycle is not co-owned by this table, and which a broadcast may
--     outlive. A cascading DELETE would silently erase delivery history.
--     Expiry, not referential cascade, retires stale rows.
--
-- broadcasts.message is a SECRET at rest: it is stored verbatim because
-- delivery needs the body, but it must never be echoed back on a read surface
-- and must never be copied into broadcast_deliveries.last_error.
--
-- target_daemon_id is forward-looking: it is added now, defaulted to '' (this
-- daemon), so the cross-daemon children need no second migration. It is inert
-- until then.

CREATE TABLE broadcasts (
    id             TEXT PRIMARY KEY,
    origin_chat_id TEXT,              -- nullable: an operator-issued broadcast has no origin chat
    selector       TEXT NOT NULL,     -- Selector.String(), the byte-stable canonical form
    message        TEXT NOT NULL,     -- SECRET: never echoed back on any read surface
    state          TEXT NOT NULL DEFAULT 'pending',
    target_count   INTEGER NOT NULL DEFAULT 0,
    expires_at     TEXT NOT NULL,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE broadcast_deliveries (
    id                TEXT PRIMARY KEY,
    broadcast_id      TEXT NOT NULL REFERENCES broadcasts(id) ON DELETE CASCADE,
    target_chat_id    TEXT NOT NULL,
    target_daemon_id  TEXT NOT NULL DEFAULT '',  -- '' = this daemon; set for cross-daemon targets
    state             TEXT NOT NULL DEFAULT 'pending',
    lease_owner       TEXT,
    lease_deadline_at TEXT,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   TEXT,
    delivered_at      TEXT,
    last_error        TEXT,           -- diagnostics only: never the message body
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Claimable delivery scan: state + retry backoff + lease recovery.
CREATE INDEX idx_broadcast_deliveries_claimable
    ON broadcast_deliveries(state, next_attempt_at, lease_deadline_at);

-- Per-broadcast delivery listing, and the ON DELETE CASCADE lookup.
CREATE INDEX idx_broadcast_deliveries_broadcast
    ON broadcast_deliveries(broadcast_id);

-- "What was sent to this chat", in creation order.
CREATE INDEX idx_broadcast_deliveries_chat
    ON broadcast_deliveries(target_chat_id, created_at);

-- The lazy expiry sweep over live broadcasts past their expiry.
CREATE INDEX idx_broadcasts_state_expires
    ON broadcasts(state, expires_at);

-- "What did this chat broadcast", in creation order. Without it the
-- ListBroadcastsFilter.OriginChatID path is the store's only filter that falls
-- back to a full table SCAN, and broadcasts grows monotonically (expiry flips
-- state, it does not prune). The listing's secondary `id` sort key still needs a
-- temp b-tree for its last term; the SCAN is what this removes.
CREATE INDEX idx_broadcasts_origin_chat
    ON broadcasts(origin_chat_id, created_at);

-- +goose Down

DROP TABLE IF EXISTS broadcast_deliveries;
DROP TABLE IF EXISTS broadcasts;
