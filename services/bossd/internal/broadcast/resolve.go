// Package broadcast contains the bossd runtime that turns a broadcast selector
// into a concrete audience and delivers the operator's message to each target
// chat with bounded, capped exponential-backoff retry.
//
// It is the bossd-side counterpart of the storage-free selector vocabulary in
// lib/bossalib/broadcast (imported here as bcast, since this package shares its
// name): that package decides who is in the audience, this one decides which
// chats are candidates in the first place, renders what they receive, and drives
// the lease/retry loop over services/bossd/internal/db.SQLiteBroadcastStore.
//
// State flow (as enforced by services/bossd/internal/db/broadcast_store.go):
//
//	Create                          -> pending
//	Resolve + CreateDeliveries      -> resolved   (one delivery row per target)
//	Worker.AcquireLease             -> delivery pending/leased -> leased
//	Worker deliver ok               -> MarkDelivered -> delivery delivered
//	Worker deliver err              -> ScheduleRetry -> delivery pending, backoff set
//	CompleteIfSettled               -> resolved -> completed (nothing outstanding)
//	ExpireOverdue(now) (lazy sweep) -> pending/resolved past expiry -> expired,
//	                                   outstanding deliveries -> skipped
//
// The delivery state `failed` is deliberately never written by this runtime. A
// delivery that keeps erroring is retried until its next attempt would land past
// the parent's expiry, at which point it is pinned there and the lazy sweep
// retires it as `skipped`; there is no separate "retry budget exhausted" verdict.
// `last_error` is what distinguishes a target that was never reachable from one
// that errored repeatedly.
//
// Delivery is at-least-once: a crash between the send and MarkDelivered can
// duplicate. The broadcast id in the rendered prompt is what lets a receiving
// agent recognise the repeat.
package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// MaxTargets caps how many chats one broadcast may address. Exceeding it is a
// refusal, never a truncation: a runaway selector must be loud rather than
// silently partial, and a caller that meant to reach more than this should say
// so with a narrower, explicit set of broadcasts.
const MaxTargets = 64

// ErrTooManyTargets marks a resolve that exceeded MaxTargets, so the RPC layer
// can map it to CodeInvalidArgument without matching on the message. The
// concrete error is a *TooManyTargetsError, which names the resolved count and
// the cap.
var ErrTooManyTargets = errors.New("broadcast audience exceeds the fan-out cap")

// TooManyTargetsError reports a selector whose audience is larger than
// MaxTargets. It unwraps to ErrTooManyTargets.
type TooManyTargetsError struct {
	// Count is how many chats the selector actually matched.
	Count int
	// Max is the cap that was exceeded (MaxTargets).
	Max int
}

// Error implements error, naming both the resolved count and the cap so the
// operator can see how far over the selector went.
func (e *TooManyTargetsError) Error() string {
	return fmt.Sprintf("broadcast selector resolved %d targets, which exceeds the fan-out cap of %d: narrow the selector (the audience is refused, not truncated)",
		e.Count, e.Max)
}

// Unwrap lets errors.Is(err, ErrTooManyTargets) succeed.
func (e *TooManyTargetsError) Unwrap() error { return ErrTooManyTargets }

// chatLister is the subset of db.AgentChatStore the resolver uses.
// ListRoutableChats is deliberately the candidate source: it is already the
// daemon's definition of "chats I can route a message to" (tmux-hosted chats
// plus headless runs), so the audience cannot drift from the set the delivery
// path can actually reach.
type chatLister interface {
	ListRoutableChats(ctx context.Context) ([]*models.AgentChat, error)
}

// sessionGetter is the subset of db.SessionStore the resolver uses: a chat only
// knows its session id, and the repo dimension lives on the session.
type sessionGetter interface {
	Get(ctx context.Context, id string) (*models.Session, error)
}

// Resolver turns a validated selector into the concrete audience of a
// broadcast.
//
// Validation of the selector itself belongs to the caller — the SendBroadcast
// handler calls Selector.Validate() before it gets here — because an invalid
// selector is an argument error the RPC must reject, not an audience the
// resolver should silently narrow. Resolve therefore assumes a validated
// selector and only applies the guards that need daemon state: liveness,
// self-exclusion and the fan-out cap.
//
// Resolution happens once, at send time, and the result is frozen into
// broadcast_deliveries. A chat created afterwards is not retroactively
// included.
type Resolver struct {
	chats    chatLister
	sessions sessionGetter
	logger   zerolog.Logger
}

// NewResolver constructs a Resolver over the daemon's chat and session stores.
func NewResolver(chats chatLister, sessions sessionGetter, logger zerolog.Logger) *Resolver {
	return &Resolver{chats: chats, sessions: sessions, logger: logger}
}

// Resolve returns the chats the selector addresses.
//
// It lists the daemon's routable chats, joins each to its session to fill the
// session-owned repo dimension, and runs Selector.Filter over the result. Three
// guards then apply, in order:
//
//  1. Liveness — a chat with a non-nil StartError never comes up, so it can
//     never receive. It is dropped from the CANDIDATE set, before matching, so
//     no selector can address it (not even chat:<id> naming it directly).
//  2. Self-exclusion — the origin chat is dropped from its own audience unless
//     includeOrigin. A coordinator broadcasting "I finished" must not wake
//     itself into a loop.
//  3. Fan-out cap — more than MaxTargets is refused outright (see
//     TooManyTargetsError); no partial audience is returned.
//
// A chat whose session cannot be loaded is skipped rather than failing the
// resolve: it cannot be addressed by repo and has no live pane, so it is
// counted and logged instead. A failure to list chats at all IS fatal — the
// candidate set is then unknown, and delivering to "whatever we managed to
// read" would be an arbitrary subset.
//
// An empty audience is not an error: "nobody matched" is a legitimate outcome.
func (r *Resolver) Resolve(ctx context.Context, sel bcast.Selector, originChatID string, includeOrigin bool) ([]bcast.Target, error) {
	chats, err := r.chats.ListRoutableChats(ctx)
	if err != nil {
		return nil, fmt.Errorf("list routable chats for broadcast: %w", err)
	}

	candidates := make([]bcast.Target, 0, len(chats))
	skippedStartError, skippedNoSession, skippedTerminalSession, skippedBlankID := 0, 0, 0, 0
	// One session backs several chats (start_chat adds siblings), and
	// ListRoutableChats is unbounded in the daemon's chat history, so the naive
	// read-per-candidate is O(all chats ever) point reads inside the RPC. Memoize
	// per resolve; a nil entry records "absent", so a missing session is not
	// re-queried either. Only the reads are cached — the classification below is
	// unchanged.
	sessionCache := make(map[string]*models.Session, len(chats))
	for _, c := range chats {
		if c.StartError != nil && *c.StartError != "" {
			// Preserved as an "attempted but never came up" row; unreachable.
			skippedStartError++
			continue
		}
		// agent_session_id is NOT NULL but not non-blank, and a blank one is not a
		// deliverable target. Skipping it costs one unreachable chat; keeping it
		// would make CreateDeliveries reject the ENTIRE batch, so a single
		// malformed row would fail every broadcast the daemon ever sends.
		if strings.TrimSpace(c.AgentSessionID) == "" {
			skippedBlankID++
			continue
		}
		session, cached := sessionCache[c.SessionID]
		if !cached {
			var serr error
			session, serr = r.sessions.Get(ctx, c.SessionID)
			if serr != nil {
				// Only an ABSENT session is a skip. Any other error — a busy database,
				// a cancelled context, a scan failure — is a read we could not make,
				// and silently dropping the chat would freeze a narrower audience than
				// the selector matched while the RPC still reported success. That is
				// exactly the silent-truncation the RPC contract forbids, and the same
				// reasoning that makes a ListRoutableChats failure fatal below.
				if !errors.Is(serr, sql.ErrNoRows) {
					return nil, fmt.Errorf("load session %s for broadcast audience: %w", c.SessionID, serr)
				}
				session = nil
			}
			sessionCache[c.SessionID] = session
		}
		if session == nil {
			skippedNoSession++
			continue
		}
		// A chat whose session a human deliberately ended, or that was orphaned
		// mid-flight, is not a broadcast target: delivery wakes a chat
		// (WakeIfAsleep), so including one would resurrect an agent in a merged or
		// archived worktree — and these rows accumulate, so leaving them in also
		// pushes ordinary repo:/agent: selectors over the fan-out cap within
		// weeks. ListRoutableChats does not filter on session state at all (its
		// predicate is about the chat row: a live tmux name OR no start error), so
		// this guard has to live here. It mirrors the predicate the transient-resume wiring
		// already applies to the same delivery primitive in cmd/main.go.
		if !sessionIsBroadcastable(session) {
			skippedTerminalSession++
			continue
		}
		accountID := ""
		if c.AccountID != nil {
			accountID = *c.AccountID
		}
		candidates = append(candidates, bcast.Target{
			// Normalised once, here: the store trims before storing and comparing,
			// so carrying a padded id would give the resolver one notion of a chat
			// id and the store another.
			ChatID:    strings.TrimSpace(c.AgentSessionID),
			SessionID: c.SessionID,
			RepoID:    session.RepoID,
			AgentName: c.AgentName,
			AccountID: accountID,
			DaemonID:  c.DaemonID,
		})
	}
	if skippedStartError > 0 || skippedNoSession > 0 || skippedTerminalSession > 0 || skippedBlankID > 0 {
		r.logger.Debug().
			Int("skipped_start_error", skippedStartError).
			Int("skipped_no_session", skippedNoSession).
			Int("skipped_terminal_session", skippedTerminalSession).
			Int("skipped_blank_chat_id", skippedBlankID).
			Msg("broadcast resolve: skipped unreachable chats")
	}

	matched := sel.Filter(candidates)

	targets := make([]bcast.Target, 0, len(matched))
	seen := make(map[string]struct{}, len(matched))
	for _, t := range matched {
		if !includeOrigin && originChatID != "" && t.ChatID == originChatID {
			continue
		}
		// Dedupe by chat id. CreateDeliveries rejects a batch that addresses one
		// chat twice — correctly, since two rows for one chat are two
		// independently claimable deliveries — and it rejects the WHOLE batch, so
		// a single duplicate from the candidate listing would fail the send
		// outright and strand the created broadcast in pending. A duplicate would
		// also inflate the count checked against MaxTargets. ChatID is already
		// normalised at candidate construction, so the raw value is the key.
		if _, dup := seen[t.ChatID]; dup {
			continue
		}
		seen[t.ChatID] = struct{}{}
		targets = append(targets, t)
	}

	if len(targets) > MaxTargets {
		return nil, &TooManyTargetsError{Count: len(targets), Max: MaxTargets}
	}
	return targets, nil
}

// sessionIsBroadcastable reports whether a chat's session is one a broadcast may
// wake. Archived, merged and closed all mean a human deliberately ended the
// work; orphaned means the run was killed mid-flight and has no live agent to
// deliver into. Every other state — including blocked and the post-implement
// waiting states — keeps a pane a delivery can reach. This mirrors the
// transient-resume eligibility predicate in cmd/main.go, which guards the same
// SendChatMessage(WakeIfAsleep) primitive.
func sessionIsBroadcastable(s *models.Session) bool {
	if s == nil {
		return false
	}
	return s.ArchivedAt == nil &&
		s.State != machine.Merged &&
		s.State != machine.Closed &&
		s.State != machine.Orphaned
}
