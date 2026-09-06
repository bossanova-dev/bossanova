package main

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// chatSessionLookup resolves a chat's owning session. Narrowed from the agent
// chat store so the sweeps here are testable without a database. Shared by the
// waiting sweep and the eviction recompute below.
type chatSessionLookup interface {
	GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error)
}

// sessionRecomputer re-derives a session's display composite
// (DisplayStatusComputer.Recompute) — the pass that runs the waiting derivation
// and rewrites display_label.
type sessionRecomputer interface {
	Recompute(ctx context.Context, sessionID string) error
}

// sweepWaitingChats re-derives the WAITING state (BOS-668) for every session
// that currently holds a working chat.
//
// The derivation is otherwise purely event-driven: it runs inside Recompute,
// which fires on session-store writes and on tracker status TRANSITIONS. Arming
// a GitHub callback is neither — the chat keeps heartbeating WORKING throughout
// — so without this periodic pass a chat that parks on a callback and never
// changes status again would stay rendered as "working" indefinitely. It is
// deliberately a poll rather than a hook on the callback store: the callback
// lifecycle also advances from GitHub webhooks and the reconcile sweep, so a
// write-path hook would still miss the deliver/expire edges.
//
// Cost is bounded by the number of sessions with a working chat, deduped, and
// the whole thing is skipped when nothing is working — the common case for an
// idle daemon. SetWaiting only fires its hook on a real reason change, so a PR
// that sits armed for an hour produces one stream event, not one per tick.
func sweepWaitingChats(
	ctx context.Context,
	tracker *status.Tracker,
	chats chatSessionLookup,
	recomputer sessionRecomputer,
) {
	if ctx.Err() != nil {
		return
	}
	seen := map[string]struct{}{}
	for agentSessionID, entry := range tracker.Snapshot() {
		if entry == nil || entry.Status != bossanovav1.ChatStatus_CHAT_STATUS_WORKING {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		chat, err := chats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			// A deleted chat row or a transient database error must not take
			// the daemon's 30s housekeeping tick down with it.
			continue
		}
		if _, dup := seen[chat.SessionID]; dup {
			continue
		}
		seen[chat.SessionID] = struct{}{}
		_ = recomputer.Recompute(ctx, chat.SessionID)
	}
}

// recomputeEvictedSessions turns a batch of evicted agent_session_ids into one
// DisplayStatusComputer.Recompute per DISTINCT session (BOS-1096).
//
// It deliberately does not read tracker.Snapshot(), which is precisely why
// sweepWaitingChats above cannot cover this case: the ids in hand are the ones
// the snapshot no longer contains. That is the whole point — the entry is gone,
// which is the event worth reacting to.
//
// Recompute alone, no delta: there is no chat status left to describe. The
// session-level SessionDelta still reaches clients because DisplayStatusComputer's
// own onUpdate publishes whenever a recompute actually changes the row. The
// signature carries no publisher, so that is a property of the type rather than
// a behaviour a test has to assert.
//
// Each session gets a FRESH 5s budget derived inside the loop rather than one
// shared across the batch. The sibling tracker hooks spend that budget on a
// single id, and a shared batch budget would let a burst — a sleep/wake makes
// every tracked entry stale at once — silently truncate its own tail, leaving
// exactly the stale labels this exists to clear. It is likewise not pollerCtx:
// the recompute that settles a label must not be cancelled by daemon shutdown
// racing the sweep.
func recomputeEvictedSessions(
	agentSessionIDs []string,
	chats chatSessionLookup,
	recomputer sessionRecomputer,
	logger zerolog.Logger,
) {
	seen := map[string]struct{}{}
	for _, agentSessionID := range agentSessionIDs {
		// Closure so the per-session cancel runs at the end of each iteration
		// rather than accumulating until the whole batch is done.
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			chat, err := chats.GetByAgentSessionID(ctx, agentSessionID)
			if err != nil {
				// One bad id must not cost the rest of the batch their recompute —
				// each remaining session is a label that would otherwise stay
				// frozen until a daemon restart.
				//
				// This is NOT the ordinary path. Both ordinary evictions resolve
				// the row and reach the recompute below: Cleanup evicts a stale
				// entry while its chat row is untouched, and DeleteChat clears the
				// cached status at server.go:4607 while the row is still present,
				// deleting it only afterwards. That ordering is what makes the
				// deleted-chat criterion hold, and it is pinned by
				// TestDeleteChat_RecomputesWhileTheChatRowIsStillPresent — move the
				// eviction after the delete and this branch starts swallowing the
				// recompute that settles the label.
				//
				// Two genuinely unlike cases do share this branch. The benign one
				// is DeleteChat's idempotent re-delete of a row that was already
				// gone before the request (server.go:4617), where there is no
				// session left to resolve and nothing to settle; the real store
				// reports that as a wrapped db.ErrAgentChatNotFound rather than a
				// nil chat (internal/db/agent_chat_store.go). The rare one is a
				// genuinely transient failure, which loses this id's recompute for
				// good — the tracker entry is already dropped, so nothing re-emits
				// it. Classifying the two apart, and retrying only the latter, is
				// tracked as follow-up work; both are skipped today.
				logger.Debug().Err(err).
					Str("agent_session_id", agentSessionID).
					Msg("eviction recompute: chat lookup failed")
				return
			}
			if chat == nil {
				// Defensive only: the real store signals a missing row through
				// the error branch above, so this is unreachable against it and
				// guards a lookup that reports absence as (nil, nil) instead.
				return
			}
			if _, dup := seen[chat.SessionID]; dup {
				return
			}
			seen[chat.SessionID] = struct{}{}
			if err := recomputer.Recompute(ctx, chat.SessionID); err != nil {
				// Recompute already soft-fails on sql.ErrNoRows (a session
				// deleted underneath us), so anything reaching here is worth a
				// line — but still not worth abandoning the remaining sessions.
				logger.Debug().Err(err).
					Str("session_id", chat.SessionID).
					Msg("eviction recompute: recompute failed")
			}
		}()
	}
}

// wireEvictionRecompute installs the tracker's eviction hook (BOS-1096).
//
// Extracted into a named function, following publishAgentMarkerSessionDelta,
// so a test can drive the real callback body rather than re-implement it — and
// so the startup seam probe has a single call site to assert against. Dropping
// this one line would leave every other test for this fix green while the
// reported symptom returned in full.
func wireEvictionRecompute(
	tracker *status.Tracker,
	chats chatSessionLookup,
	recomputer sessionRecomputer,
	logger zerolog.Logger,
) {
	tracker.SetOnEntriesEvicted(func(agentSessionIDs []string) {
		recomputeEvictedSessions(agentSessionIDs, chats, recomputer, logger)
	})
}
