-- +goose Up
-- BOS-979: durable failover-proxy path-token registry.
--
-- bossd's in-process failover proxy mints a per-session or per-chat path token
-- and bakes http://127.0.0.1:44127/s/<token> into each Claude tmux pane's
-- environment. tmux set-environment cannot mutate an already-running process,
-- so a live pane's URL is immutable for that pane's lifetime — the daemon must
-- therefore remember the token, because it can never reissue one. Holding the
-- registry only in memory means a daemon restart wipes it and a surviving pane
-- presents a token the new proxy has never heard of (401, wedged pane). This
-- table is that memory.
--
-- WHAT IS STORED IS NOT THE SECRET. token_sha256 is hex(sha256(token)); the raw
-- token never reaches this table. That is the repo's stated doctrine — see
-- CONCEPTS.md "Daemon token authority" ("raw tokens are never Redis keys or
-- persisted values") and its bosso implementation in
-- services/bosso/internal/stream/tokens_redis.go. Resolution hashes the
-- presented token and looks the digest up, so the column is the primary key.
--
-- Ownership: a token belongs to the SESSION whose pane bakes it.
--
--   * session_id DOES take a real FK with ON DELETE CASCADE. A token is
--     meaningless once its session is gone, and session deletion is a real path
--     (session_store.go Delete), so the row must go with it. This is the
--     broadcast_deliveries.broadcast_id / note_tags.note_id precedent, not the
--     notes.session_id one: notes deliberately outlive their session, a path
--     token deliberately does not. The connection runs PRAGMA foreign_keys=ON.
--
--   * agent_session_id takes NO FK. At the time this table was added,
--     agent_chats.agent_session_id was indexed but not unique, so SQLite could
--     not accept it as a parent key at all; 20260904000000 later made it UNIQUE
--     without adding the FK, so the absence is now a deliberate choice rather
--     than a limitation. Either way the chat-only delete path
--     (agent_chat_store.go DeleteByAgentSessionID) gets no cascade and issues an
--     explicit DELETE against this table instead. Keep the two together:
--     dropping the explicit delete silently leaks chat rows.
--
-- is_chat_shaped records which of the two proxy target shapes to rebuild:
-- 0 is a bare sessionID target, 1 is session.ProxyTargetForChat's
-- "chat\0<agentSessionID>\0<fallbackAccountID>". The assembled target string is
-- deliberately NOT stored: it embeds NUL bytes, and the rebuild must repopulate
-- sessionToToken / chatToToken / sessionChats as well as tokenToSession, which
-- needs the components. session.ProxyTargetForChat stays the one author of the
-- wire format.
--
-- Bounded growth: sessions.archived_at is a SOFT delete, so an archived
-- session's tokens survive archiving — correct, because its panes may still be
-- live, but it does mean the table grows with archive volume rather than with
-- live-session count. It is one short row per pane; if that ever matters, evict
-- on hard delete of the session row, not on archive.
--
-- Timestamps follow the BOS-14 naming standard: TEXT ISO-8601 millisecond UTC
-- (sqlutil.TimeLayout); non-null created_at defaults via strftime.

CREATE TABLE proxy_tokens (
    token_sha256     TEXT PRIMARY KEY,           -- hex(sha256(token)); never the token
    session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_session_id TEXT,                       -- chat-shaped only; no FK (deliberate; see header)
    account_id       TEXT,                       -- chat-shaped fallback account at mint/refresh time
    is_chat_shaped   INTEGER NOT NULL DEFAULT 0, -- boolean: 0 = session target, 1 = chat target
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Evicting a session's tokens on Deregister, and the FK's own cascade lookup.
CREATE INDEX idx_proxy_tokens_session_id ON proxy_tokens(session_id);

-- The chat-only delete path's predicate (no FK can serve it — see above).
CREATE INDEX idx_proxy_tokens_agent_session_id ON proxy_tokens(agent_session_id);

-- +goose Down

DROP TABLE IF EXISTS proxy_tokens;
