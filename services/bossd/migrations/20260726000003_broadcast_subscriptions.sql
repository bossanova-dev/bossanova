-- +goose Up
-- BOS-557: standing broadcast subscriptions.
--
-- A subscription is a standing rule — "when this session completes, or errors,
-- broadcast this message to this audience" — so a coordinator learns a child
-- finished without polling. It is deliberately thin: it holds the intent only
-- and fires by constructing a broadcast through the existing resolver/delivery
-- path (20260726000001_broadcasts.sql), so no new delivery machinery lives here.
-- Lifecycle is active -> fired, with canceled and expired terminal.
--
-- state is compare-and-swap guarded by the store: the active -> fired
-- transition is the single guard against a double fire when a session is
-- transitioned from more than one code path, or when the reconcile sweep runs
-- alongside one. fired_broadcast_id records which broadcast the winning CAS
-- produced, so a fired row is auditable back to what was actually sent.
--
-- Timestamps follow the BOS-14 naming standard: TEXT ISO-8601 millisecond UTC
-- (sqlutil.TimeLayout); non-null created/updated default via strftime; nullable
-- _at columns are plain TEXT. expires_at is caller-supplied and carries no
-- default: a strftime('now') default would mean "already expired" on every row
-- that forgot to set it.
--
-- owner_session_id deliberately has NO foreign key, and the omission is the
-- design. It is the same call broadcast_deliveries.target_chat_id and
-- github_callbacks.target_chat_id make (see 20260722000000_github_callbacks.sql):
-- it is a logical reference to a session whose lifecycle this table does not
-- co-own, and which a subscription may outlive. A cascading DELETE would
-- silently erase a standing registration — and its firing history — the moment
-- the session row went away, turning "the child finished" into silence. Expiry,
-- not referential cascade, retires a subscription whose session will never
-- reach a trigger state. origin_chat_id and fired_broadcast_id are unreferenced
-- for the same reason: neither may drag a subscription row away with it.
--
-- message is a SECRET at rest: it is stored verbatim because firing needs the
-- body, but it must never be echoed back on a read surface, never logged, and
-- never copied into a diagnostic column. Every other column on this table is
-- safe to log; this one is not.

CREATE TABLE broadcast_subscriptions (
    id                 TEXT PRIMARY KEY,
    owner_session_id   TEXT NOT NULL,     -- the session whose outcome fires this; no FK, see above
    origin_chat_id     TEXT,              -- nullable: an operator-issued subscription has no origin chat
    trigger_event      TEXT NOT NULL,     -- 'completed' | 'errored' | 'settled'
    selector           TEXT NOT NULL,     -- Selector.String(), the byte-stable canonical form
    message            TEXT NOT NULL,     -- SECRET: never echoed back on any read surface
    state              TEXT NOT NULL DEFAULT 'active',
    fired_at           TEXT,
    fired_broadcast_id TEXT,              -- the broadcast the winning CAS produced
    expires_at         TEXT NOT NULL,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- The evaluator's hot path: "which live subscriptions does this session's
-- transition fire", run on every classified state change.
CREATE INDEX idx_broadcast_subscriptions_owner_state
    ON broadcast_subscriptions(owner_session_id, state);

-- The reconcile sweep's expiry scan over live subscriptions past their expiry,
-- and the distinct-owner scan that finds sessions still carrying one.
CREATE INDEX idx_broadcast_subscriptions_state_expires
    ON broadcast_subscriptions(state, expires_at);

-- +goose Down

DROP TABLE IF EXISTS broadcast_subscriptions;
