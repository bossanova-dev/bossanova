package status

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// --- the predicate, in isolation ---------------------------------------------

const idleTestThreshold = 8 * time.Hour

// idleTestNow is an arbitrary fixed instant; the predicate only ever reads
// differences from it.
var idleTestNow = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// idleTestOwner builds a fully resumable owner: a chat with a provider session
// id, attached to a session with a worktree. Tests strip whichever field they
// are proving is load-bearing.
func idleTestOwner() *chatOwner {
	return &chatOwner{
		chat: &models.AgentChat{
			ID:                "chat-1",
			SessionID:         "sess-1",
			AgentSessionID:    "agent-1",
			ProviderSessionID: ptr("provider-1"),
		},
		session: &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"},
	}
}

func idleTestEntry(status pb.ChatStatus, lastOutputAgo time.Duration) *Entry {
	return &Entry{Status: status, LastOutputAt: idleTestNow.Add(-lastOutputAgo)}
}

func TestEvaluateIdleReap(t *testing.T) {
	present := func() bool { return true }
	absent := func() bool { return false }

	tests := []struct {
		name        string
		ev          idleReapEvidence
		wantReap    bool
		wantReason  string
		wantIdleFor time.Duration
	}{
		{
			name:       "the name resolved to no single chat",
			ev:         idleReapEvidence{owner: nil, transcriptPresent: present},
			wantReason: reasonIdleUnresolvedName,
		},
		{
			name: "the owner has no session, so there is no worktree to judge",
			ev: idleReapEvidence{
				owner:             &chatOwner{chat: idleTestOwner().chat},
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
				transcriptPresent: present,
			},
			wantReason: reasonIdleUnresolvedName,
		},
		{
			// D2: absence of evidence is not evidence of idleness. Tracker.Get
			// returns nil both for a chat never polled and for one whose last
			// poll aged past the staleness threshold.
			name:       "no tracker entry at all",
			ev:         idleReapEvidence{owner: idleTestOwner(), entry: nil, transcriptPresent: present},
			wantReason: reasonIdleNoTrackerEntry,
		},
		{
			name: "reported WORKING",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_WORKING, 9*time.Hour),
				transcriptPresent: present,
			},
			wantReason: reasonIdleStatusNotIdle,
		},
		{
			// D4: QUESTION needs no separate exclusion — it is simply not IDLE.
			// A pane parked at a question produces no output for days and would
			// sail past an output-clock-only test.
			name: "reported QUESTION, however old the output clock is",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_QUESTION, 30*24*time.Hour),
				transcriptPresent: present,
			},
			wantReason: reasonIdleStatusNotIdle,
		},
		{
			name: "reported LIMITED, waiting out a usage window",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_LIMITED, 30*24*time.Hour),
				transcriptPresent: present,
			},
			wantReason: reasonIdleStatusNotIdle,
		},
		{
			name: "IDLE but the raw output clock was never set",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             &Entry{Status: pb.ChatStatus_CHAT_STATUS_IDLE},
				transcriptPresent: present,
			},
			wantReason: reasonIdleNoOutputClock,
		},
		{
			name: "IDLE but one nanosecond inside the window",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, idleTestThreshold-1),
				transcriptPresent: present,
			},
			wantReason:  reasonIdleWithinWindow,
			wantIdleFor: idleTestThreshold - 1,
		},
		{
			// BOS-1179: provider_session_id is only one possible resume id.
			// Claude commonly resumes from agent_session_id, so a missing
			// provider id is not a keep reason by itself.
			name: "idle long enough with no provider session id but an agent resume id",
			ev: func() idleReapEvidence {
				owner := idleTestOwner()
				owner.chat.ProviderSessionID = nil
				return idleReapEvidence{
					owner:             owner,
					entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
					transcriptPresent: present,
					resumeSessionID:   owner.chat.AgentSessionID,
				}
			}(),
			wantReap:    true,
			wantReason:  reasonIdleTooLong,
			wantIdleFor: 9 * time.Hour,
		},
		{
			name: "idle long enough with empty provider session id but an agent resume id",
			ev: func() idleReapEvidence {
				owner := idleTestOwner()
				owner.chat.ProviderSessionID = ptr("")
				return idleReapEvidence{
					owner:             owner,
					entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
					transcriptPresent: present,
					resumeSessionID:   owner.chat.AgentSessionID,
				}
			}(),
			wantReap:    true,
			wantReason:  reasonIdleTooLong,
			wantIdleFor: 9 * time.Hour,
		},
		{
			name: "idle long enough but the resolved resume id is empty",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
				transcriptPresent: present,
			},
			wantReason:  reasonIdleNoResumeSessionID,
			wantIdleFor: 9 * time.Hour,
		},
		{
			name: "idle long enough but a tmux client is attached",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
				transcriptPresent: present,
				resumeSessionID:   "provider-1",
				attachedClients:   1,
			},
			wantReason:  reasonIdleAttachedClient,
			wantIdleFor: 9 * time.Hour,
		},
		{
			name: "idle long enough but the transcript is gone",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
				transcriptPresent: absent,
				resumeSessionID:   "provider-1",
			},
			wantReason:  reasonIdleTranscriptMissing,
			wantIdleFor: 9 * time.Hour,
		},
		{
			// A nil probe means the oracle could not be wired at all. It must
			// fail closed, not read as "no objection".
			name: "idle long enough but no transcript oracle is wired",
			ev: idleReapEvidence{
				owner:           idleTestOwner(),
				entry:           idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour),
				resumeSessionID: "provider-1",
			},
			wantReason:  reasonIdleTranscriptOracle,
			wantIdleFor: 9 * time.Hour,
		},
		{
			name: "idle exactly at the window, resumable, transcript present",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, idleTestThreshold),
				transcriptPresent: present,
				resumeSessionID:   "provider-1",
			},
			wantReap:    true,
			wantReason:  reasonIdleTooLong,
			wantIdleFor: idleTestThreshold,
		},
		{
			name: "idle well past the window",
			ev: idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 30*time.Hour),
				transcriptPresent: present,
				resumeSessionID:   "provider-1",
			},
			wantReap:    true,
			wantReason:  reasonIdleTooLong,
			wantIdleFor: 30 * time.Hour,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reap, reason, idleFor := evaluateIdleReap(&tc.ev, idleTestNow, idleTestThreshold, policyLongIdle)
			if reap != tc.wantReap || reason != tc.wantReason {
				t.Fatalf("evaluateIdleReap = (%v, %q), want (%v, %q)", reap, reason, tc.wantReap, tc.wantReason)
			}
			if idleFor != tc.wantIdleFor {
				t.Fatalf("idleFor = %v, want %v", idleFor, tc.wantIdleFor)
			}
		})
	}
}

// TestEvaluateIdleReap_TranscriptProbeIsLazy pins the cost model: the probe is
// a plugin RPC, so a sweep over a host full of busy panes must not pay one per
// pane. Only a candidate that has cleared every cheaper gate may reach it.
func TestEvaluateIdleReap_TranscriptProbeIsLazy(t *testing.T) {
	tests := []struct {
		name       string
		entry      *Entry
		wantProbes int
	}{
		{"no tracker entry", nil, 0},
		{"reported WORKING", idleTestEntry(pb.ChatStatus_CHAT_STATUS_WORKING, 9*time.Hour), 0},
		{"inside the window", idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, time.Hour), 0},
		{"every cheaper gate passed", idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 9*time.Hour), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			ev := idleReapEvidence{
				owner:             idleTestOwner(),
				entry:             tc.entry,
				transcriptPresent: func() bool { probes++; return true },
				resumeSessionID:   "provider-1",
			}
			evaluateIdleReap(&ev, idleTestNow, idleTestThreshold, policyLongIdle)
			if probes != tc.wantProbes {
				t.Fatalf("transcript probes = %d, want %d", probes, tc.wantProbes)
			}
		})
	}
}

// --- identification ----------------------------------------------------------

// TestResolveChatOwners is the D1 fence. accountedFor's whitelist explicitly
// tolerates 8-character truncation collisions because a collision there can
// only spare an orphan; here the same names identify what to kill, so a
// collision must resolve to nothing.
func TestResolveChatOwners(t *testing.T) {
	sess := func(id string, recorded *string) *models.Session {
		return &models.Session{ID: id, RepoID: reaperRepoID, TmuxSessionName: recorded}
	}
	chat := func(id, agentSessionID string, recorded *string) *models.AgentChat {
		return &models.AgentChat{
			ID: id, SessionID: reaperSessionID,
			AgentSessionID: agentSessionID, TmuxSessionName: recorded,
		}
	}

	t.Run("a name claimed by exactly one chat resolves", func(t *testing.T) {
		owners := resolveChatOwners(
			[]*models.Session{sess(reaperSessionID, nil)},
			[]*models.AgentChat{chat("c1", reaperIdleChatID, ptr(reaperIdleChatPane))},
		)
		owner := owners[reaperIdleChatPane]
		if owner == nil || owner.chat == nil || owner.chat.ID != "c1" {
			t.Fatalf("owners[%q] = %+v, want the single claiming chat", reaperIdleChatPane, owner)
		}
		if owner.session == nil || owner.session.ID != reaperSessionID {
			t.Fatalf("owner.session = %+v, want the chat's session", owner.session)
		}
	})

	t.Run("both legs of one chat are a single claim, not a collision", func(t *testing.T) {
		// Recorded pointer and recomputed name are the same string here, but
		// they are also two separate claim() calls; they must collapse by row
		// id rather than read as two rows.
		owners := resolveChatOwners(
			[]*models.Session{sess(reaperSessionID, nil)},
			[]*models.AgentChat{chat("c1", reaperIdleChatID, ptr(reaperIdleChatPane))},
		)
		if owners[reaperIdleChatPane] == nil {
			t.Fatal("a chat claiming its own name twice resolved to nothing")
		}
	})

	t.Run("a name two chats could claim resolves to nothing", func(t *testing.T) {
		// The second chat's agent session id differs but its RECORDED pointer
		// names the first chat's pane — the truncation collision in the flesh.
		owners := resolveChatOwners(
			[]*models.Session{sess(reaperSessionID, nil)},
			[]*models.AgentChat{
				chat("c1", reaperIdleChatID, nil),
				chat("c2", "eeeeeeee55555555", ptr(reaperIdleChatPane)),
			},
		)
		if owner, ok := owners[reaperIdleChatPane]; ok {
			t.Fatalf("owners[%q] = %+v, want no entry: two rows claim it", reaperIdleChatPane, owner)
		}
	})

	t.Run("a name a session-level row could claim resolves to nothing", func(t *testing.T) {
		// tmux.SessionName and tmux.ChatSessionName both emit
		// boss-{repo8}-{id8}, so a session whose id shares the chat's first
		// eight characters mints an identical name.
		owners := resolveChatOwners(
			[]*models.Session{
				sess(reaperSessionID, nil),
				sess("dddddddd99999999", nil),
			},
			[]*models.AgentChat{chat("c1", reaperIdleChatID, nil)},
		)
		if owner, ok := owners[reaperIdleChatPane]; ok {
			t.Fatalf("owners[%q] = %+v, want no entry: a session mints the same name", reaperIdleChatPane, owner)
		}
	})

	t.Run("a legacy session-only pane has no owner", func(t *testing.T) {
		owners := resolveChatOwners(
			[]*models.Session{sess(reaperSessionID, ptr("boss-aaaaaaaa-legacy"))},
			nil,
		)
		if owner, ok := owners["boss-aaaaaaaa-legacy"]; ok {
			t.Fatalf("owners[legacy] = %+v, want no entry: a session pane is never an idle candidate", owner)
		}
	})
}

// --- sweep integration -------------------------------------------------------

// idleArmedFixture is the common setup for the sweep-level idle tests: orphan
// reaping armed too (so a bug that routed an idle pane down the orphan path
// would be visible rather than silently harmless), one live, accounted-for,
// resumable chat pane, and a chat that has been idle for a day.
func idleArmedFixture(t *testing.T) *reaperFixture {
	t.Helper()
	fx := armedFixture(t, false)
	fx.addResumableChat(ptr(reaperIdleChatPane))
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(24 * time.Hour)
	return fx
}

func TestTmuxReaper_IdleReapNeedsAConfirmingSweep(t *testing.T) {
	fx := idleArmedFixture(t)
	fx.wantsChatTeardown = true

	if n := fx.sweep(); n != 0 {
		t.Fatalf("first sweep reaped %d, want 0: the idle strike is not yet aged", n)
	}
	if len(fx.killedChats) != 0 {
		t.Fatalf("first sweep tore down %v, want nothing", fx.killedChats)
	}
	assertLogged(t, fx.logs, `"reason":"idleFirstStrike"`)

	// Deliberately SHORTER than the orphan block's grace period. AC1 says the
	// reap lands "on the sweep after the one that first saw it eligible", and
	// D7 asks for one confirming sweep — not for the strike to age past a
	// window belonging to the other reaper path. Advancing by less than
	// reaperTestGrace is what makes this test fail if an elapsed-time
	// condition is ever reintroduced alongside the strike.
	fx.now = fx.now.Add(reaperTestGrace / 10)
	if n := fx.sweep(); n != 1 {
		t.Fatalf("confirming sweep reaped %d, want 1: the sweep after the first sighting must reap", n)
	}
	// The pane name is part of the record: the teardown is bound to the pane
	// THIS sweep resolved, not to one it re-derives from the row.
	want := reaperSessionID + "/" + reaperIdleChatID + "/" + reaperIdleChatPane
	if len(fx.killedChats) != 1 || fx.killedChats[0] != want {
		t.Fatalf("killedChats = %v, want [%s]", fx.killedChats, want)
	}
	assertLogged(t, fx.logs, "tmux reaper: reaped idle chat pane")
}

func TestTmuxReaper_IdleStrikeClearedWhenChatBecomesActive(t *testing.T) {
	fx := idleArmedFixture(t)

	fx.sweep()
	if fx.reaper.strikeCount() != 1 {
		t.Fatalf("strikeCount = %d, want 1 after the first idle sighting", fx.reaper.strikeCount())
	}

	// The user comes back: the poller reports WORKING.
	fx.tracker.Update(reaperIdleChatID, pb.ChatStatus_CHAT_STATUS_WORKING, fx.now)

	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("sweep after the chat woke reaped %d, want 0", n)
	}
	if len(fx.killedChats) != 0 {
		t.Fatalf("tore down %v, want nothing: the chat is working", fx.killedChats)
	}
	if fx.reaper.strikeCount() != 0 {
		t.Fatalf("strikeCount = %d, want 0: an active observation clears the idle marker", fx.reaper.strikeCount())
	}
}

// TestTmuxReaper_IdleAndOrphanStrikesDoNotConfirmEachOther is why a strike is
// keyed by name AND reason. One sighting of each kind for one name is two
// unconfirmed strikes, not one confirmed reap.
func TestTmuxReaper_IdleAndOrphanStrikesDoNotConfirmEachOther(t *testing.T) {
	fx := idleArmedFixture(t)

	// One idle sighting.
	fx.sweep()

	// A separate orphan sighting for the SAME name, planted directly so the
	// two markers coexist.
	fx.reaper.recordStrike(reaperIdleChatPane, reasonUnaccountedForTwice, fx.now)
	if fx.reaper.strikeCount() != 2 {
		t.Fatalf("strikeCount = %d, want 2 distinct markers for one name", fx.reaper.strikeCount())
	}

	// The chat wakes. Only the idle marker may be dropped; the orphan one is a
	// different question this observation says nothing about.
	fx.tracker.Update(reaperIdleChatID, pb.ChatStatus_CHAT_STATUS_WORKING, fx.now)
	fx.now = fx.now.Add(2 * reaperTestGrace)
	fx.sweep()

	if _, ok := fx.reaper.strikes[strikeKey{name: reaperIdleChatPane, reason: reasonUnaccountedForTwice}]; ok {
		t.Fatal("the orphan marker survived: an accounted-for pane must drop it")
	}
	if _, ok := fx.reaper.strikes[strikeKey{name: reaperIdleChatPane, reason: reasonIdleTooLong}]; ok {
		t.Fatal("the idle marker survived a working observation")
	}
	if len(fx.killedChats) != 0 || len(fx.killed) != 0 {
		t.Fatalf("killed %v / %v, want nothing", fx.killed, fx.killedChats)
	}
}

func TestTmuxReaper_IdleReapDisabledKeepsIdlePanes(t *testing.T) {
	fx := newReaperFixtureWithIdle(t,
		reaperConfig(true, false, false, time.Minute),
		reaperIdleConfig(false, false))
	fx.addSession(reaperSessionID, nil)
	fx.addResumableChat(ptr(reaperIdleChatPane))
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(24 * time.Hour)

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("reaped %d with the idle knob off, want 0", n)
	}
	if len(fx.killedChats) != 0 {
		t.Fatalf("tore down %v with the idle knob off", fx.killedChats)
	}
	// Pre-BOS-886 log semantics, unchanged.
	assertLogged(t, fx.logs, `"reason":"accountedFor"`)
}

// TestTmuxReaper_OrphanReapDisabledStillRunsTheIdlePath proves the two knobs
// are genuinely independent: the loop starts for the idle path alone, and an
// unaccounted-for pane on such a host is still never touched.
func TestTmuxReaper_OrphanReapDisabledStillRunsTheIdlePath(t *testing.T) {
	fx := newReaperFixtureWithIdle(t,
		reaperConfig(false, false, true, time.Minute),
		reaperIdleConfig(true, false))
	fx.wantsChatTeardown = true
	fx.addSession(reaperSessionID, nil)
	fx.addResumableChat(ptr(reaperIdleChatPane))
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.addLivePane("boss-aaaaaaaa-99999999", time.Hour, ptr(reaperDaemonID))
	fx.markIdle(24 * time.Hour)

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 1 {
		t.Fatalf("reaped %d, want 1: the idle path runs with orphan reaping off", n)
	}
	if len(fx.killed) != 0 {
		t.Fatalf("orphan-killed %v with orphan reaping off", fx.killed)
	}
	assertLogged(t, fx.logs, `"reason":"orphanReapDisabled"`)
}

func TestTmuxReaper_IdleReapKeepsAPaneWithNoTrackerEvidence(t *testing.T) {
	fx := armedFixture(t, false)
	fx.addResumableChat(ptr(reaperIdleChatPane))
	fx.addLivePane(reaperIdleChatPane, 30*24*time.Hour, ptr(reaperDaemonID))
	// Deliberately no markIdle: nothing has ever reported on this chat.

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("reaped %d, want 0: an absent tracker entry means unknown, not idle", n)
	}
	assertLogged(t, fx.logs, `"reason":"idleNoTrackerEntry"`)
}

// TestTmuxReaper_IdleReapKeepsAChatWhoseTranscriptIsGone is D5 at the SWEEP
// level. The predicate's own transcript cases prove the rule; this one proves
// the wiring — that classifyIdle actually builds the probe from the daemon's
// oracle and honours a "no" — which is the only arming-side gate whose closure
// could be silently inverted without any predicate test noticing.
func TestTmuxReaper_IdleReapKeepsAChatWhoseTranscriptIsGone(t *testing.T) {
	fx := idleArmedFixture(t)
	fx.transcriptExists = false

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("reaped %d, want 0: waking this chat would yield an agent with no memory of it", n)
	}
	if len(fx.killedChats) != 0 {
		t.Fatalf("tore down %v, want nothing: the transcript is gone", fx.killedChats)
	}
	assertLogged(t, fx.logs, `"reason":"idleTranscriptMissing"`)
	if fx.transcriptProbes == 0 {
		t.Fatal("the oracle was never consulted: classifyIdle is not wiring the transcript probe")
	}
}

// TestTmuxReaper_IdleReapProbesTheTranscriptOncePerCandidate pins the RPC cost
// model through the sweep, not just through the predicate: the probe is the one
// gate that costs a plugin round trip, so it must be reached only by a
// candidate every cheaper gate has already passed.
func TestTmuxReaper_IdleReapProbesTheTranscriptOncePerCandidate(t *testing.T) {
	fx := idleArmedFixture(t)
	fx.wantsChatTeardown = true

	// A second accounted-for pane that is nowhere near eligible: it has no
	// tracker entry at all, so it must never reach the oracle.
	fx.addChat("cccccccc33333333", ptr("boss-aaaaaaaa-cccccccc"))
	fx.addLivePane("boss-aaaaaaaa-cccccccc", time.Hour, ptr(reaperDaemonID))

	fx.sweep()
	if fx.transcriptProbes != 1 {
		t.Fatalf("transcriptProbes = %d after one sweep, want 1: only the eligible candidate may cost an RPC",
			fx.transcriptProbes)
	}

	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 1 {
		t.Fatalf("confirming sweep reaped %d, want 1", n)
	}
	if fx.transcriptProbes != 2 {
		t.Fatalf("transcriptProbes = %d after two sweeps, want 2", fx.transcriptProbes)
	}
}

// --- kill contract -----------------------------------------------------------

// TestTmuxReaper_FailedIdleTeardownIsRetried is D8's error contract. The
// canonical routine clears the pane pointer before killing, so a failure
// anywhere in it leaves pane and pointer consistent — the candidate must be
// reported as NOT reaped and simply retried.
func TestTmuxReaper_FailedIdleTeardownIsRetried(t *testing.T) {
	fx := idleArmedFixture(t)
	fx.wantsChatTeardown = true
	fx.killChatErr = errors.New("could not clear the pane pointer")

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("reaped %d, want 0: the teardown failed", n)
	}
	if len(fx.killedChats) != 0 {
		t.Fatalf("killedChats = %v, want nothing recorded for a failed teardown", fx.killedChats)
	}
	assertLogged(t, fx.logs, "retrying on the next sweep")

	// The next sweep finds the candidate still eligible and still struck, and
	// succeeds once the underlying failure clears.
	fx.killChatErr = nil
	fx.now = fx.now.Add(reaperTestGrace)
	if n := fx.sweep(); n != 1 {
		t.Fatalf("retry sweep reaped %d, want 1", n)
	}
	if len(fx.killedChats) != 1 {
		t.Fatalf("killedChats = %v, want one teardown after the retry", fx.killedChats)
	}
}

// TestTmuxReaper_IdleReapWithNoSeamIsInert covers the daemon wiring the idle
// deps without a teardown function: classification and logging still happen,
// nothing dies.
func TestTmuxReaper_IdleReapWithNoSeamIsInert(t *testing.T) {
	fx := idleArmedFixture(t)
	fx.reaper.idle.KillChat = nil

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("reaped %d with no teardown seam, want 0", n)
	}
	assertLogged(t, fx.logs, "no chat teardown seam is wired")
}

// --- dry run -----------------------------------------------------------------

// TestTmuxReaper_IdleDryRunLogsCandidatesAndKillsNothing is also the plan's
// required proof artefact: the log line an operator reads to decide whether to
// arm the feature has to name the resolved chat, the observed idle age and the
// reason (D10 — the idle clock restarts with the daemon, so the observed age is
// the number that has to be visible, not inferred).
func TestTmuxReaper_IdleDryRunLogsCandidatesAndKillsNothing(t *testing.T) {
	fx := newReaperFixtureWithIdle(t,
		reaperConfig(true, false, false, time.Minute),
		reaperIdleConfig(true, true))
	fx.addSession(reaperSessionID, nil)
	fx.addResumableChat(ptr(reaperIdleChatPane))
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(24 * time.Hour)

	fx.sweep()
	fx.now = fx.now.Add(2 * reaperTestGrace)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("a dry run reaped %d, want 0", n)
	}
	if len(fx.killedChats) != 0 || len(fx.killed) != 0 {
		t.Fatalf("a dry run killed %v / %v", fx.killed, fx.killedChats)
	}

	for _, want := range []string{
		"tmux reaper: would reap idle chat pane (dry run)",
		`"tmuxSession":"` + reaperIdleChatPane + `"`,
		`"chatId":"chat-` + reaperIdleChatID + `"`,
		`"agentSessionId":"` + reaperIdleChatID + `"`,
		`"sessionId":"` + reaperSessionID + `"`,
		`"reason":"idleTooLong"`,
		`"idleFor":`,
		`"dryRun":true`,
		`"idleCandidates":1`,
	} {
		assertLogged(t, fx.logs, want)
	}
}

// TestNewAgentTranscriptOracle_FailsClosed pins the production oracle's
// answer when it cannot ask: false, which on this path means keep.
func TestNewAgentTranscriptOracle_FailsClosed(t *testing.T) {
	oracle := NewAgentTranscriptOracle(nil)
	if oracle.TranscriptExists(context.Background(), "claude", "/tmp/wt", "agent-1") {
		t.Fatal("a nil plugin registry answered true; an unreachable oracle must keep the pane")
	}
}

func TestEvaluateIdleReap_TerminalPolicies(t *testing.T) {
	for _, kind := range []string{"merged", "closed", "archived"} {
		t.Run(kind, func(t *testing.T) {
			owner := idleTestOwner()
			switch kind {
			case "merged":
				owner.session.State = machine.Merged
			case "closed":
				owner.session.State = machine.Closed
			case "archived":
				owner.session.ArchivedAt = &idleTestNow
			}
			policy := policyForOwner(owner)
			if policy != policyTerminal {
				t.Fatalf("policy = %q", policy)
			}
			cases := []struct {
				name   string
				mutate func(*idleReapEvidence)
				want   string
			}{
				{"unresolved", func(e *idleReapEvidence) { e.owner = nil }, reasonIdleUnresolvedName},
				{"no tracker", func(e *idleReapEvidence) { e.entry = nil }, reasonTerminalNoTrackerEntry},
				{"working", func(e *idleReapEvidence) { e.entry.Status = pb.ChatStatus_CHAT_STATUS_WORKING }, reasonTerminalChatWorking},
				{"no clock", func(e *idleReapEvidence) { e.entry.LastOutputAt = time.Time{} }, reasonIdleNoOutputClock},
				{"grace", func(e *idleReapEvidence) { e.entry.LastOutputAt = idleTestNow.Add(-10 * time.Second) }, reasonTerminalWithinGrace},
				{"attached", func(e *idleReapEvidence) { e.attachedClients = 1 }, reasonIdleAttachedClient},
				{"unknown delivery", func(e *idleReapEvidence) { e.deliveryPending = nil }, reasonTerminalDeliveryUnknown},
				{"failed delivery read", func(e *idleReapEvidence) {
					e.deliveryPending = func() (bool, error) { return false, errors.New("read failed") }
				}, reasonTerminalDeliveryUnknown},
				{"pending delivery", func(e *idleReapEvidence) { e.deliveryPending = func() (bool, error) { return true, nil } }, reasonTerminalDeliveryPending},
				{"idle", func(*idleReapEvidence) {}, reasonTerminalSession},
				{"question", func(e *idleReapEvidence) { e.entry.Status = pb.ChatStatus_CHAT_STATUS_QUESTION }, reasonTerminalSession},
				{"limited", func(e *idleReapEvidence) { e.entry.Status = pb.ChatStatus_CHAT_STATUS_LIMITED }, reasonTerminalSession},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					calls := 0
					ev := idleReapEvidence{owner: owner, entry: idleTestEntry(pb.ChatStatus_CHAT_STATUS_IDLE, 31*time.Second), deliveryPending: func() (bool, error) { calls++; return false, nil }, transcriptPresent: func() bool { t.Fatal("terminal transcript probe"); return false }}
					tc.mutate(&ev)
					reap, reason, age := evaluateIdleReap(&ev, idleTestNow, 30*time.Second, policy)
					if reason != tc.want || reap != (tc.want == reasonTerminalSession) {
						t.Fatalf("(%v,%s), want %s", reap, reason, tc.want)
					}
					if tc.want == reasonTerminalWithinGrace && age != 10*time.Second {
						t.Fatalf("age=%s", age)
					}
					if (tc.want == reasonTerminalWithinGrace || tc.want == reasonIdleAttachedClient) && calls != 0 {
						t.Fatal("delivery read before cheaper gates")
					}
				})
			}
		})
	}
	if got := policyForOwner(idleTestOwner()); got != policyLongIdle {
		t.Fatalf("live policy=%s", got)
	}
}

func finishedArmedFixture(t *testing.T) *reaperFixture {
	fx := idleArmedFixture(t)
	fx.chats.all[1].Role = models.AgentChatRoleBuild
	fx.markIdle(4 * time.Minute)
	fx.reaper.idle.ProgressWait = func(context.Context, *models.AgentChat, *models.Session) (bool, error) { return false, nil }
	fx.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) {
		return &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Since: timestamppb.New(fx.now.Add(-4 * time.Minute))}, nil
	}
	return fx
}

func TestTmuxReaper_FinishedWaiting(t *testing.T) {
	for _, kind := range []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_MERGE, pb.AttentionKind_ATTENTION_KIND_FIX} {
		t.Run(kind.String(), func(t *testing.T) {
			fx := finishedArmedFixture(t)
			fx.wantsChatTeardown = true
			fx.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) {
				return &pb.SessionAttention{Kind: kind, ParkCodes: []string{"human-review"}}, nil
			}
			if n := fx.sweep(); n != 0 {
				t.Fatalf("first sighting reaped %d", n)
			}
			fx.now = fx.now.Add(time.Second)
			if n := fx.sweep(); n != 1 {
				t.Fatalf("confirming sweep reaped %d", n)
			}
			for _, field := range []string{`"reason":"finishedWaitingOnHuman"`, `"attentionKind":"` + kind.String() + `"`, `"policy":"finishedWait"`, `"chatId":"chat-` + reaperIdleChatID + `"`, `"sessionId":"` + reaperSessionID + `"`, `"idleFor":`, `"level":"info"`} {
				assertLogged(t, fx.logs, field)
			}
			if len(fx.chats.all) != 2 || len(fx.sessions.all) != 1 {
				t.Fatal("reaping removed persisted rows")
			}
		})
	}
}

func TestTmuxReaper_FinishedKeepReasons(t *testing.T) {
	tests := []struct {
		name, reason string
		setup        func(*reaperFixture)
	}{
		{"operator", "finishedNotAgentOwned", func(f *reaperFixture) { f.chats.all[1].Role = models.AgentChatRoleOperator }},
		{"unknown role", "finishedNotAgentOwned", func(f *reaperFixture) { f.chats.all[1].Role = models.AgentChatRoleUnknown }},
		{"no clock", "idleNoOutputClock", func(f *reaperFixture) {
			f.tracker.Update(reaperIdleChatID, pb.ChatStatus_CHAT_STATUS_IDLE, time.Time{})
		}},
		{"missing transcript oracle", "idleTranscriptOracleMissing", func(f *reaperFixture) { f.reaper.idle.Transcripts = nil }},
		{"no tracker", "idleNoTrackerEntry", func(f *reaperFixture) { f.reaper.idle.Tracker = nil }},
		{"working", "idleStatusNotIdle", func(f *reaperFixture) { f.tracker.Update(reaperIdleChatID, pb.ChatStatus_CHAT_STATUS_WORKING, f.now) }},
		{"question", "idleStatusNotIdle", func(f *reaperFixture) { f.tracker.Update(reaperIdleChatID, pb.ChatStatus_CHAT_STATUS_QUESTION, f.now) }},
		{"limited", "idleStatusNotIdle", func(f *reaperFixture) { f.tracker.Update(reaperIdleChatID, pb.ChatStatus_CHAT_STATUS_LIMITED, f.now) }},
		{"recent output", "finishedWithinGrace", func(f *reaperFixture) { f.markIdle(time.Minute) }},
		{"attached", "idleAttachedClient", func(f *reaperFixture) { f.setAttached(*f.chats.all[1].TmuxSessionName, 1) }},
		{"pending delivery", "finishedDeliveryPending", func(f *reaperFixture) {
			f.reaper.idle.DeliveryPending = func(context.Context, string) (bool, error) { return true, nil }
		}},
		{"delivery error", "finishedDeliveryUnknown", func(f *reaperFixture) {
			f.reaper.idle.DeliveryPending = func(context.Context, string) (bool, error) { return false, errors.New("read") }
		}},
		{"progress wait", "finishedProgressWait", func(f *reaperFixture) {
			f.reaper.idle.ProgressWait = func(context.Context, *models.AgentChat, *models.Session) (bool, error) { return true, nil }
		}},
		{"progress error", "finishedProgressWaitUnknown", func(f *reaperFixture) {
			f.reaper.idle.ProgressWait = func(context.Context, *models.AgentChat, *models.Session) (bool, error) {
				return false, errors.New("read")
			}
		}},
		{"attention error", "finishedAttentionUnknown", func(f *reaperFixture) {
			f.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) { return nil, errors.New("read") }
		}},
		{"attention nil", "finishedAttentionUnknown", func(f *reaperFixture) {
			f.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) { return nil, nil }
		}},
		{"recent attention", "finishedAttentionWithinGrace", func(f *reaperFixture) {
			f.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) {
				return &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Since: timestamppb.New(f.now.Add(-time.Minute))}, nil
			}
		}},
		{"no resume", "idleNoResumeSessionID", func(f *reaperFixture) {
			f.chats.all[1].ProviderSessionID = nil
			f.chats.all[1].AgentSessionID = ""
			f.tracker.Update("", pb.ChatStatus_CHAT_STATUS_IDLE, f.now.Add(-4*time.Minute))
		}},
		{"no transcript", "idleTranscriptMissing", func(f *reaperFixture) { f.transcriptExists = false }},
	}
	for _, kind := range []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, pb.AttentionKind_ATTENTION_KIND_QUESTION, pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_DONE, pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED} {
		tests = append(tests, struct {
			name, reason string
			setup        func(*reaperFixture)
		}{kind.String(), "finishedAttentionNotWaiting", func(f *reaperFixture) {
			f.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) {
				return &pb.SessionAttention{Kind: kind}, nil
			}
		}})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := finishedArmedFixture(t)
			tc.setup(fx)
			fx.sweep()
			fx.now = fx.now.Add(time.Second)
			if n := fx.sweep(); n != 0 {
				t.Fatalf("reaped %d", n)
			}
			assertLogged(t, fx.logs, `"reason":"`+tc.reason+`"`)
		})
	}
}

func TestTmuxReaper_FinishedFallsThrough(t *testing.T) {
	for _, which := range []string{"operator", "progress wait", "missing seams"} {
		t.Run(which, func(t *testing.T) {
			fx := finishedArmedFixture(t)
			fx.markIdle(9 * time.Hour)
			fx.wantsChatTeardown = true
			switch which {
			case "operator":
				fx.chats.all[1].Role = models.AgentChatRoleOperator
			case "progress wait":
				fx.reaper.idle.ProgressWait = func(context.Context, *models.AgentChat, *models.Session) (bool, error) { return true, nil }
			case "missing seams":
				fx.reaper.idle.ProgressWait = nil
				fx.reaper.idle.SessionAttention = nil
			}
			fx.sweep()
			fx.now = fx.now.Add(time.Second)
			if n := fx.sweep(); n != 1 {
				t.Fatalf("long-idle reaped %d", n)
			}
			assertLogged(t, fx.logs, `"reason":"idleTooLong"`)
		})
	}
}

func TestTmuxReaper_FinishedLazySeams(t *testing.T) {
	fx := finishedArmedFixture(t)
	fx.markIdle(time.Minute)
	fx.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) {
		t.Fatal("attention read before grace gate")
		return nil, nil
	}
	fx.sweep()
	if fx.transcriptProbes != 0 {
		t.Fatal("transcript read before grace gate")
	}
	fx.markIdle(4 * time.Minute)
	fx.reaper.idle.ProgressWait = func(context.Context, *models.AgentChat, *models.Session) (bool, error) { return true, nil }
	fx.sweep()
	if fx.transcriptProbes != 0 {
		t.Fatal("transcript read before progress gate")
	}
}

func TestTmuxReaper_FinishedGraceAndStrikeReset(t *testing.T) {
	fx := finishedArmedFixture(t)
	fx.wantsChatTeardown = true
	fx.reaper.idle.Config.FinishedGraceSeconds = 300
	if n := fx.sweep(); n != 0 {
		t.Fatalf("before configured output grace: %d", n)
	}
	assertLogged(t, fx.logs, `"reason":"finishedWithinGrace"`)
	fx.markIdle(6 * time.Minute)
	since := fx.now.Add(-4 * time.Minute)
	fx.reaper.idle.SessionAttention = func(context.Context, string) (*pb.SessionAttention, error) {
		return &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Since: timestamppb.New(since)}, nil
	}
	fx.sweep()
	assertLogged(t, fx.logs, `"reason":"finishedAttentionWithinGrace"`)
	fx.now = fx.now.Add(time.Minute)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("first eligible sighting: %d", n)
	}
	fx.setAttached(*fx.chats.all[1].TmuxSessionName, 1)
	fx.sweep()
	fx.setAttached(*fx.chats.all[1].TmuxSessionName, 0)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("stale finished strike survived attach: %d", n)
	}
	if n := fx.sweep(); n != 1 {
		t.Fatalf("confirmation after reset: %d", n)
	}
}
