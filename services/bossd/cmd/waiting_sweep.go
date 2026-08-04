package main

import (
	"context"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// waitingChatLookup resolves a chat's owning session. Narrowed from the agent
// chat store so the sweep is testable without a database.
type waitingChatLookup interface {
	GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error)
}

// waitingRecomputer re-derives a session's display composite, which is what
// runs the waiting derivation (DisplayStatusComputer.Recompute).
type waitingRecomputer interface {
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
	chats waitingChatLookup,
	recomputer waitingRecomputer,
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
