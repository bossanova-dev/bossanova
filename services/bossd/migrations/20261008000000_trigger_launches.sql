-- +goose Up
-- BOS-1418: daemon-side idempotency for LaunchTriggerSessionCommand.
--
-- bosso may resend a launch after its acknowledgement timed out, and only the
-- daemon knows whether the first attempt actually created a session. One row
-- per inbound trigger invocation records that: the PRIMARY KEY on
-- invocation_id is what arbitrates two concurrent launches (the INSERT's
-- ON CONFLICT arm lets exactly one claim win, and only takes over a
-- session-less claim old enough to be abandoned, re-stamping its created_at),
-- and session_id is what a replay returns.
--
-- session_id is NULL while the claim is in flight and set once the session
-- exists. It deliberately takes NO foreign key: the row is the durable record
-- that this invocation launched that session, and a replay must keep returning
-- the same id even after the session row is hard-deleted (a cascade would let a
-- late retry launch a second session; SET NULL would make the row read as a
-- claim that is still in flight). trigger_id names a bosso-side trigger, so it
-- has no local parent table either.
--
-- Bounded growth: the launch path prunes rows older than 30 days, far beyond
-- any bosso retry window.
--
-- Timestamps follow the BOS-14 naming standard: TEXT ISO-8601 millisecond UTC
-- (sqlutil.TimeLayout); non-null created_at defaults via strftime.

CREATE TABLE trigger_launches (
    invocation_id TEXT PRIMARY KEY,
    trigger_id    TEXT NOT NULL,
    session_id    TEXT, -- NULL while the launch is in flight; no FK (deliberate; see header)
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- The prune predicate.
CREATE INDEX idx_trigger_launches_created_at ON trigger_launches(created_at);

-- +goose Down

DROP TABLE IF EXISTS trigger_launches;
