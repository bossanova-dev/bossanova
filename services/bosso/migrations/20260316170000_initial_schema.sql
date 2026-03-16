-- +goose Up

-- Users authenticated via OIDC (Auth0). The sub field is the unique
-- identifier from the identity provider.
CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    sub        TEXT NOT NULL UNIQUE,
    email      TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Registered daemons. Each daemon connects with a unique ID and reports
-- its hostname and managed repos via heartbeat.
CREATE TABLE daemons (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hostname        TEXT NOT NULL,
    session_token   TEXT NOT NULL UNIQUE,
    active_sessions INTEGER NOT NULL DEFAULT 0,
    last_heartbeat  TEXT,
    online          INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_daemons_user_id ON daemons(user_id);

-- Repos managed by each daemon. A daemon can manage multiple repos,
-- and the same repo URL might be managed by multiple daemons.
CREATE TABLE daemon_repos (
    daemon_id TEXT NOT NULL REFERENCES daemons(id) ON DELETE CASCADE,
    repo_id   TEXT NOT NULL,
    PRIMARY KEY (daemon_id, repo_id)
);

-- Lightweight session registry for routing. The full session data lives
-- on the daemon; the orchestrator only tracks which daemon owns which session.
CREATE TABLE sessions_registry (
    session_id TEXT PRIMARY KEY,
    daemon_id  TEXT NOT NULL REFERENCES daemons(id) ON DELETE CASCADE,
    title      TEXT NOT NULL DEFAULT '',
    state      INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_sessions_registry_daemon_id ON sessions_registry(daemon_id);

-- Append-only audit log for observability.
CREATE TABLE audit_log (
    id        TEXT PRIMARY KEY,
    user_id   TEXT REFERENCES users(id) ON DELETE SET NULL,
    action    TEXT NOT NULL,
    resource  TEXT NOT NULL,
    detail    TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_audit_log_user_id ON audit_log(user_id);
CREATE INDEX idx_audit_log_action ON audit_log(action);
CREATE INDEX idx_audit_log_created_at ON audit_log(created_at);

-- +goose Down

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS sessions_registry;
DROP TABLE IF EXISTS daemon_repos;
DROP TABLE IF EXISTS daemons;
DROP TABLE IF EXISTS users;
