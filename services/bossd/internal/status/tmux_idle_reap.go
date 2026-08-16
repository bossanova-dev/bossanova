package status

import (
	"context"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// Reasons attached to an idle-reap decision (BOS-886). They are separate
// constants from the orphan reasons above, not shared ones, because a strike is
// keyed by name AND reason: sharing a string would let an orphan sighting and
// an idle sighting confirm each other.
const (
	reasonIdleUnresolvedName    = "idleUnresolvedName"
	reasonIdleNoTrackerEntry    = "idleNoTrackerEntry"
	reasonIdleStatusNotIdle     = "idleStatusNotIdle"
	reasonIdleNoOutputClock     = "idleNoOutputClock"
	reasonIdleWithinWindow      = "idleWithinWindow"
	reasonIdleNoProviderSession = "idleNoProviderSession"
	reasonIdleTranscriptMissing = "idleTranscriptMissing"
	reasonIdleFirstStrike       = "idleFirstStrike"
	reasonIdleTooLong           = "idleTooLong"
)

// idleReapDefaultAgent is the registry key a chat row persisted before
// AgentName existed resolves to, mirroring the server package's
// liveTranscriptOracle.
const idleReapDefaultAgent = "claude"

// TranscriptOracle answers "could this chat still be resumed with its history
// intact?". It is the same narrow surface the chat-spawn path uses; the reaper
// needs it because reaping a chat whose transcript is gone destroys context
// rather than reclaiming memory (BOS-886 D5).
type TranscriptOracle interface {
	TranscriptExists(ctx context.Context, agentName, workDir, agentSessionID string) bool
}

// NewAgentTranscriptOracle builds the production oracle over the daemon's
// AgentRunner plugin registry. A nil or empty registry, an unknown agent name,
// or an RPC error all answer false — which on this path means "keep", so the
// unreachable-plugin case under-reaps rather than reaping blind.
func NewAgentTranscriptOracle(clients map[string]agent.AgentRunnerClient) TranscriptOracle {
	return agentTranscriptOracle{clients: clients}
}

type agentTranscriptOracle struct {
	clients map[string]agent.AgentRunnerClient
}

func (o agentTranscriptOracle) TranscriptExists(ctx context.Context, agentName, workDir, agentSessionID string) bool {
	if o.clients == nil {
		return false
	}
	name := agentName
	if name == "" {
		name = idleReapDefaultAgent
	}
	client, ok := o.clients[name]
	if !ok {
		return false
	}
	resp, err := client.TranscriptExists(ctx, &pb.TranscriptExistsRequest{
		WorkDir:        workDir,
		AgentSessionId: agentSessionID,
	})
	if err != nil || resp == nil {
		return false
	}
	return resp.GetExists()
}

// killChatFunc is the idle path's destructive seam. It is deliberately NOT
// killSessionFunc: an idle reap must clear the chat's pane pointer as part of
// the teardown (BOS-886 D8), and that ordering plus its error contract live in
// the session package's canonical routine, which this reaches through a
// function value rather than an import (D9 — session already imports status, so
// importing back would be a cycle).
//
// A nil seam means the idle path is inert: candidates are still classified and
// logged, but nothing is killed.
//
// paneName is the pane THIS sweep classified, not one the teardown re-derives:
// resolveChatOwners resolves a chat by either of its two name legs, so passing
// the proven name is what keeps the destructive call bound to the identity the
// policy was judged against.
type killChatFunc func(ctx context.Context, sessionID, agentSessionID, paneName string) error

// chatOwner is the single chat a live pane name provably belongs to, together
// with the session that gives it a worktree.
type chatOwner struct {
	chat    *models.AgentChat
	session *models.Session
}

// resolveChatOwners maps a pane name to the ONE chat row that owns it, and
// drops every name that more than one row could claim (BOS-886 D1).
//
// This is the inversion the feature turns on. accountedFor assembles a
// PROTECTIVE set, and its comment explicitly licenses 8-character truncation
// collisions because a collision there can only spare an orphan. Here the same
// names become an IDENTIFICATION map, where a collision would kill the wrong
// pane — so the licence is withdrawn: a name claimed by two distinct rows, or
// by any session-level row, resolves to nothing and is kept.
//
// Session-level panes are counted as claimants precisely so they cannot be
// reaped. tmux.SessionName and tmux.ChatSessionName both emit
// "boss-{repo8}-{id8}", so a name alone does not say which kind of row minted
// it; a legacy session pane has no chat, no reported status and no output
// clock, and inferring "no tracker entry ⇒ never produced output ⇒ idle" from
// that absence is exactly the mistake D1 forbids.
//
// Both the recorded pointer and the recomputed name are claimed, mirroring
// accountedFor's legs: StartTmuxChat creates the pane well before it persists
// the name, so the recorded pointer alone would leave a live chat's pane
// unresolvable for the whole creation window.
func resolveChatOwners(sessions []*models.Session, chats []*models.AgentChat) map[string]*chatOwner {
	sessionByID := make(map[string]*models.Session, len(sessions))
	// claims maps a pane name to the distinct rows claiming it, keyed by a
	// namespaced row id so two claims from the SAME row (its recorded pointer
	// and its recomputed name) collapse to one rather than reading as a
	// collision. A nil value marks a session-level claim.
	claims := make(map[string]map[string]*chatOwner)
	claim := func(name, key string, owner *chatOwner) {
		if name == "" {
			return
		}
		byRow, ok := claims[name]
		if !ok {
			byRow = make(map[string]*chatOwner, 1)
			claims[name] = byRow
		}
		byRow[key] = owner
	}

	for _, s := range sessions {
		if s == nil || s.ID == "" {
			continue
		}
		sessionByID[s.ID] = s
		key := "session:" + s.ID
		if s.TmuxSessionName != nil {
			claim(*s.TmuxSessionName, key, nil)
		}
		if s.RepoID != "" {
			claim(tmux.SessionName(s.RepoID, s.ID), key, nil)
		}
	}

	for _, c := range chats {
		if c == nil || c.ID == "" {
			continue
		}
		sess := sessionByID[c.SessionID]
		key := "chat:" + c.ID
		owner := &chatOwner{chat: c, session: sess}
		if c.TmuxSessionName != nil {
			claim(*c.TmuxSessionName, key, owner)
		}
		if sess != nil && sess.RepoID != "" && c.AgentSessionID != "" {
			claim(tmux.ChatSessionName(sess.RepoID, c.AgentSessionID), key, owner)
		}
	}

	out := make(map[string]*chatOwner, len(claims))
	for name, byRow := range claims {
		if len(byRow) != 1 {
			continue
		}
		for _, owner := range byRow {
			if owner != nil {
				out[name] = owner
			}
		}
	}
	return out
}

// idleReapEvidence is everything the eligibility predicate is allowed to look
// at. Assembling it is the sweep's job; judging it is this file's, so the
// policy is testable without tmux, a database, a plugin or a clock.
type idleReapEvidence struct {
	owner *chatOwner

	// entry is the tracker's record for this chat, or nil. Nil covers both
	// "never polled" and "polled too long ago to trust" — Tracker.Get evicts
	// past its staleness threshold — and both mean UNKNOWN, never idle
	// (BOS-886 D2, mirroring the repair displacement predicate, which likewise
	// refuses on absent tracker evidence).
	entry *Entry

	// transcriptPresent is consulted ONLY after every cheaper gate has passed,
	// because answering it costs a plugin RPC per candidate. A nil probe fails
	// closed: an oracle that could not be wired keeps the pane.
	transcriptPresent func() bool
}

// evaluateIdleReap decides whether one resolved pane may be reaped for
// idleness. It can only ever spare a pane relative to the gate before it, and
// every branch returns the reason an operator will read in the sweep log.
//
// idleFor is the observed idle age, returned even when the answer is keep so a
// dry run can show the clock (D10: that clock restarts with the daemon, and an
// operator staring at a reaper that never fires needs to see the truncation
// rather than guess at it).
func evaluateIdleReap(ev idleReapEvidence, now time.Time, threshold time.Duration) (reap bool, reason string, idleFor time.Duration) {
	if ev.owner == nil || ev.owner.chat == nil || ev.owner.session == nil {
		return false, reasonIdleUnresolvedName, 0
	}
	if ev.entry == nil {
		return false, reasonIdleNoTrackerEntry, 0
	}

	// D3, first conjunct. D4: QUESTION and LIMITED need no separate exclusion —
	// a pane parked at a question or a usage-limit banner produces no output,
	// but the poller reports those statuses rather than IDLE, so this gate
	// already keeps them. A second, drifting exclusion would only rot.
	if ev.entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		return false, reasonIdleStatusNotIdle, 0
	}

	// D3, second conjunct, from the RAW output clock. Tracker.Liveness's
	// spinner-normalized "substantive output" timestamp must not be used here:
	// it deliberately ignores spinner frames, so a pane whose agent is actively
	// spinning would age past the window and be reaped mid-work. Trusting the
	// raw clock instead under-reaps a pure spinner forever, which is the safe
	// direction.
	if ev.entry.LastOutputAt.IsZero() {
		return false, reasonIdleNoOutputClock, 0
	}
	idleFor = now.Sub(ev.entry.LastOutputAt)
	if idleFor < threshold {
		return false, reasonIdleWithinWindow, idleFor
	}

	// D5. Waking a chat with no provider session id, or whose transcript is
	// gone, yields a fresh agent with no memory of the conversation. That is
	// context destruction, not memory reclamation, so such a chat keeps
	// regardless of age.
	if ev.owner.chat.ProviderSessionID == nil || *ev.owner.chat.ProviderSessionID == "" {
		return false, reasonIdleNoProviderSession, idleFor
	}
	if ev.transcriptPresent == nil || !ev.transcriptPresent() {
		return false, reasonIdleTranscriptMissing, idleFor
	}

	return true, reasonIdleTooLong, idleFor
}
