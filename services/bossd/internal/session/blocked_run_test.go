package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/status"
)

var blockedRunAt = time.Date(2026, 10, 17, 14, 3, 52, 400_000_000, time.UTC)

// blockedRunFixture is a Blocked unattended run (#6655's shape: repair spent,
// PR draft) whose chat, transcript and worktree a test drives.
type blockedRunFixture struct {
	*rotationFixture
	chats      *mockAgentChatStore
	worktree   *mockWorktreeManager
	display    *status.DisplayTracker
	working    map[string]bool
	transcript map[string]string
	// armed holds, per agent session id, when its live progress callback was
	// created; waitReads records each progress-wait read.
	armed     map[string]time.Time
	waitReads []waitRead
	marked    int
}

type waitRead struct {
	chat  string
	since time.Time
}

func (f *blockedRunFixture) progressWait(_ context.Context, chat *domain.AgentChat, _ *models.Session, since time.Time) (bool, error) {
	f.waitReads = append(f.waitReads, waitRead{chat.AgentSessionID, since})
	armed, ok := f.armed[chat.AgentSessionID]
	return ok && !armed.Before(since), nil
}

func newBlockedRunFixture(t *testing.T) *blockedRunFixture {
	t.Helper()
	f := &blockedRunFixture{
		rotationFixture: newRotationFixture(t),
		worktree:        &mockWorktreeManager{},
		display:         status.NewDisplayTracker(),
		working:         map[string]bool{},
		transcript:      map[string]string{},
		armed:           map[string]time.Time{},
	}
	row := f.sessions.sessions[f.sessionID]
	reason := "run blocked: flows CI remains red after 5 repair passes"
	at := blockedRunAt
	row.State, row.IsTmuxUnattended, row.StateEnteredAt, row.BlockedReason = machine.Blocked, true, &at, &reason
	row.PRNumber, row.BranchName = intPtr(42), "cron-build"
	row.LastRepairAttemptCount = 5
	f.chats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
		f.sessionID: {{ID: "chat", SessionID: f.sessionID, AgentSessionID: "agent-old", AgentName: "claude"}},
	}}
	f.lc.agentChats = f.chats
	f.lc.worktrees = f.worktree
	f.lc.SetAgents(map[string]agent.RunnerClient{"claude": &fakeAgentForLifecycle{
		ReadTranscriptFunc: func(req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
			ts, ok := f.transcript[req.GetAgentSessionId()]
			if !ok {
				return &bossanovav1.ReadTranscriptResponse{}, nil
			}
			return &bossanovav1.ReadTranscriptResponse{Exists: true, Messages: []*bossanovav1.ChatMessage{{Role: "assistant", Timestamp: ts}}}, nil
		},
	}})
	f.lc.SetBlockedRunDeps(func(id string) bool { return f.working[id] }, f.progressWait, f.display, func(context.Context, string) { f.marked++ })
	return f
}

func (f *blockedRunFixture) row() *models.Session { return f.sessions.sessions[f.sessionID] }

// served reports whether the served attention reads the run as working.
func (f *blockedRunFixture) served() bool {
	return BlockedAlertFacts(f.row(), f.display.Get(f.sessionID)).RunWorking
}

// assertStillBlocked fails unless the run is Blocked with its reason and its
// spent repair budget.
func (f *blockedRunFixture) assertStillBlocked(t *testing.T) {
	t.Helper()
	got := f.row()
	if got.State != machine.Blocked || got.BlockedReason == nil || got.LastRepairAttemptCount != 5 {
		t.Fatalf("state=%v reason=%v repair attempts=%d, want the run left Blocked with its repair budget spent",
			got.State, got.BlockedReason, got.LastRepairAttemptCount)
	}
}

func at(d time.Duration) string { return blockedRunAt.Add(d).Format(time.RFC3339Nano) }

// committedAfterBlock makes the worktree show work the block never saw, the
// way a rebase, an amend, a WIP commit or HEAD checked out on main does.
func committedAfterBlock(f *blockedRunFixture) {
	f.worktree.statusOut = " M flows/ci.yml"
	f.worktree.commitSubjects = []string{"wip: try the pin bump"}
	f.worktree.latestCommitSubject = "wip: try the pin bump"
}

// TestMarkWorkingBlockedRuns: a turn of the run's own chat reads in progress
// and moves nothing. No turn, commit, rebase or callback leaves Blocked.
func TestMarkWorkingBlockedRuns(t *testing.T) {
	after := 23 * time.Minute
	for _, tc := range []struct {
		name    string
		working bool
		newest  string
		arrange func(*blockedRunFixture)
		want    bool
	}{
		{name: "operator asks a question and the agent answers", working: true, newest: at(after), want: true},
		{name: "a pre-armed callback fires into the blocked chat", working: true, newest: at(time.Hour), want: true},
		{name: "the agent commits WIP, then asks a question", working: true, newest: at(after), arrange: committedAfterBlock, want: true},
		{name: "a rebase-only turn", working: true, newest: at(after), arrange: committedAfterBlock, want: true},
		{name: "HEAD checked out on main mid-turn", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			committedAfterBlock(f)
			f.worktree.latestCommitSubject = "main: an unrelated merge"
		}, want: true},
		{name: "the agent committed and its turn ended", newest: at(after), arrange: committedAfterBlock},
		{name: "operator commits by hand with no agent turn", newest: at(-time.Minute), arrange: committedAfterBlock},
		{name: "pane redraw with no new message", working: true, newest: at(-time.Minute)},
		{name: "unreadable timestamp", working: true, newest: "not a time"},
		{name: "rollout keyed by the provider session id", working: true, arrange: func(f *blockedRunFixture) {
			provider := "rollout-1"
			f.chats.chatsBySession[f.sessionID][0].ProviderSessionID = &provider
			f.transcript[provider] = at(after)
		}, want: true},
		{name: "attended session", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			f.row().IsTmuxUnattended = false
		}},
		{name: "zero-output run", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			f.row().IsQuickChat = true
		}},
		{name: "usage-limit park", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			resume := blockedRunAt.Add(time.Hour)
			f.row().RotationResumeAt = &resume
		}},
		{name: "archived", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			archived := blockedRunAt.Add(time.Hour)
			f.row().ArchivedAt = &archived
		}},
		{name: "unknown block time", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			f.row().StateEnteredAt = nil
		}},
		{name: "another chat is the one working", newest: at(-time.Minute), arrange: func(f *blockedRunFixture) {
			f.chats.seedChat(f.sessionID, &domain.AgentChat{ID: "operator", SessionID: f.sessionID, AgentSessionID: "operator", AgentName: "claude"})
			f.working["operator"] = true
			f.transcript["operator"] = at(after)
		}},
		{name: "a dead older chat has the newer transcript and a stale working status", working: true, newest: at(-time.Minute), arrange: func(f *blockedRunFixture) {
			dead := &domain.AgentChat{ID: "dead", SessionID: f.sessionID, AgentSessionID: "agent-dead", AgentName: "claude"}
			f.chats.chatsBySession[f.sessionID] = append([]*domain.AgentChat{dead}, f.chats.chatsBySession[f.sessionID]...)
			f.working["agent-dead"] = true
			f.transcript["agent-dead"] = at(after)
		}},
		{name: "another chat is live", working: true, newest: at(after), arrange: func(f *blockedRunFixture) {
			f.chats.seedChat(f.sessionID, &domain.AgentChat{ID: "operator", SessionID: f.sessionID, AgentSessionID: "operator", AgentName: "claude"})
			f.runner.running["operator"] = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBlockedRunFixture(t)
			f.working["agent-old"] = tc.working
			if tc.newest != "" {
				f.transcript["agent-old"] = tc.newest
			}
			if tc.arrange != nil {
				tc.arrange(f)
			}

			f.lc.MarkWorkingBlockedRuns(context.Background())

			f.assertStillBlocked(t)
			if f.served() != tc.want {
				t.Fatalf("served working = %v, want %v", f.served(), tc.want)
			}
		})
	}
}

// TestMarkWorkingBlockedRunsAgentAsksBackThenGoesQuiet walks the first
// verdict's scenario (b): the agent asks the operator a question and the
// operator is away. The row reads in progress only while the agent works, then
// Fix again, and the run never leaves Blocked, so nothing readies the draft.
func TestMarkWorkingBlockedRunsAgentAsksBackThenGoesQuiet(t *testing.T) {
	f := newBlockedRunFixture(t)
	f.working["agent-old"] = true
	f.transcript["agent-old"] = at(23 * time.Minute)
	ctx := context.Background()

	f.lc.MarkWorkingBlockedRuns(ctx)
	if !f.served() || f.marked != 1 {
		t.Fatalf("served working=%v marks=%d, want the working row published once", f.served(), f.marked)
	}
	f.lc.MarkWorkingBlockedRuns(ctx)
	if f.marked != 1 {
		t.Fatalf("marks=%d, want an unchanged mark left unpublished", f.marked)
	}

	f.working["agent-old"] = false
	f.transcript["agent-old"] = at(24 * time.Minute)
	for range 3 {
		f.lc.MarkWorkingBlockedRuns(ctx)
	}
	if f.served() || f.marked != 2 {
		t.Fatalf("served working=%v marks=%d, want Fix back and published once", f.served(), f.marked)
	}
	f.assertStillBlocked(t)
}

// TestMarkWorkingBlockedRunsClearsWhenTheRunCannotWork: a run archived or
// parked on a usage limit mid-turn drops its mark on the next sweep instead of
// reading in progress until a restart.
func TestMarkWorkingBlockedRunsClearsWhenTheRunCannotWork(t *testing.T) {
	for name, stop := range map[string]func(*models.Session){
		"archived": func(s *models.Session) {
			archived := blockedRunAt.Add(time.Hour)
			s.ArchivedAt = &archived
		},
		"usage-limit park": func(s *models.Session) {
			resume := blockedRunAt.Add(time.Hour)
			s.RotationResumeAt = &resume
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newBlockedRunFixture(t)
			f.working["agent-old"] = true
			f.transcript["agent-old"] = at(23 * time.Minute)
			f.lc.MarkWorkingBlockedRuns(context.Background())
			if !f.served() {
				t.Fatal("precondition: the working run is not marked")
			}
			stop(f.row())
			f.lc.MarkWorkingBlockedRuns(context.Background())
			if f.served() || f.marked != 2 {
				t.Fatalf("served working=%v marks=%d, want the mark cleared and published", f.served(), f.marked)
			}
		})
	}
}

// TestBlockedRunMarkIsScopedToItsBlock checks that a mark from an earlier
// block does not suspend a later one: a run that re-blocks reads Fix at once.
func TestBlockedRunMarkIsScopedToItsBlock(t *testing.T) {
	f := newBlockedRunFixture(t)
	f.working["agent-old"] = true
	f.transcript["agent-old"] = at(23 * time.Minute)
	f.lc.MarkWorkingBlockedRuns(context.Background())
	reblocked := blockedRunAt.Add(time.Hour)
	f.row().StateEnteredAt = &reblocked
	if f.served() {
		t.Fatal("a later block reads as working on the earlier block's mark")
	}
}

// TestMarkBlockedRunsParkedOnTheirCallback: an idle run chat that armed a
// progress callback in this block reads in progress; nobody needs to act until
// it fires.
func TestMarkBlockedRunsParkedOnTheirCallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*blockedRunFixture)
		want    bool
	}{
		{name: "armed after the block", arrange: func(f *blockedRunFixture) {
			f.armed["agent-old"] = blockedRunAt.Add(32 * time.Minute)
		}, want: true},
		{name: "armed before the block", arrange: func(f *blockedRunFixture) {
			f.armed["agent-old"] = blockedRunAt.Add(-time.Minute)
		}},
		{name: "no callback"},
		{name: "another chat holds the wait", arrange: func(f *blockedRunFixture) {
			f.chats.seedChat(f.sessionID, &domain.AgentChat{ID: "operator", SessionID: f.sessionID, AgentSessionID: "operator", AgentName: "claude"})
			f.armed["operator"] = blockedRunAt.Add(32 * time.Minute)
		}},
		{name: "another chat is live", arrange: func(f *blockedRunFixture) {
			f.armed["agent-old"] = blockedRunAt.Add(32 * time.Minute)
			f.chats.seedChat(f.sessionID, &domain.AgentChat{ID: "operator", SessionID: f.sessionID, AgentSessionID: "operator", AgentName: "claude"})
			f.runner.running["operator"] = true
		}},
		{name: "no progress-wait reader", arrange: func(f *blockedRunFixture) {
			f.armed["agent-old"] = blockedRunAt.Add(32 * time.Minute)
			f.lc.SetBlockedRunDeps(func(id string) bool { return f.working[id] }, nil, f.display, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBlockedRunFixture(t)
			if tc.arrange != nil {
				tc.arrange(f)
			}

			f.lc.MarkWorkingBlockedRuns(context.Background())

			f.assertStillBlocked(t)
			if f.served() != tc.want {
				t.Fatalf("served working = %v, want %v", f.served(), tc.want)
			}
			for _, r := range f.waitReads {
				if r.chat != "agent-old" || !r.since.Equal(blockedRunAt) {
					t.Fatalf("progress wait read %+v, want the run chat since the block", r)
				}
			}
			if tc.want && !f.display.Get(f.sessionID).BlockedRunWorkingAt.Equal(blockedRunAt) {
				t.Fatalf("marked block = %v, want %v", f.display.Get(f.sessionID).BlockedRunWorkingAt, blockedRunAt)
			}
		})
	}
}

// TestMarkBlockedRunsParkedOnTheirCallbackLapses: once the callback is
// delivered, cancelled or expired the row reads FIX again.
func TestMarkBlockedRunsParkedOnTheirCallbackLapses(t *testing.T) {
	f := newBlockedRunFixture(t)
	f.armed["agent-old"] = blockedRunAt.Add(32 * time.Minute)
	ctx := context.Background()
	f.lc.MarkWorkingBlockedRuns(ctx)
	if !f.served() || f.marked != 1 {
		t.Fatalf("served working=%v marks=%d, want the parked run marked once", f.served(), f.marked)
	}
	delete(f.armed, "agent-old")
	f.lc.MarkWorkingBlockedRuns(ctx)
	if f.served() || f.marked != 2 {
		t.Fatalf("served working=%v marks=%d, want FIX back and published", f.served(), f.marked)
	}
	f.assertStillBlocked(t)
}

func TestMarkWorkingBlockedRunsLeavesUnwiredDaemonAlone(t *testing.T) {
	f := newRotationFixture(t)
	row := f.sessions.sessions[f.sessionID]
	at := time.Now().Add(-time.Hour)
	row.State, row.IsTmuxUnattended, row.StateEnteredAt = machine.Blocked, true, &at
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{f.sessionID: {{AgentSessionID: "agent-old", AgentName: "claude"}}}}
	f.lc.MarkWorkingBlockedRuns(context.Background())
	if row.State != machine.Blocked {
		t.Fatalf("state=%v without a chat-status reader", row.State)
	}
}

// blockedFinishFixture is newRunBlockedFixture's cron run (clean branch, open
// draft PR #77, run chat agent-1) after its finalize Blocked it.
func newBlockedFinishFixture(t *testing.T) (*runBlockedFixture, *Lifecycle) {
	t.Helper()
	f := newRunBlockedFixture()
	sess := f.sessions.sessions["sess-1"]
	reason := "run blocked: flows CI remains red after 5 repair passes"
	at := blockedRunAt
	sess.State, sess.StateEnteredAt, sess.BlockedReason, sess.LastRepairAttemptCount = machine.Blocked, &at, &reason, 5
	sess.PRNumber = intPtr(77)
	return f, newFinalizeChatLifecycle(t, f.sessions, f.repos, f.chats, f.cron, f.wt, f.vp, zerolog.Nop())
}

// TestBlockedRunMovesOnlyOnItsOwnFinish: the run's own explicit claim is the
// only thing that moves a Blocked run. finish --blocked re-Blocks it with the
// new reason and leaves the PR draft and the repair budget; a plain finish
// readies the PR and gives repair a fresh budget. Another chat, the operator,
// a callback and the stranded sweep have no claim and change nothing.
func TestBlockedRunMovesOnlyOnItsOwnFinish(t *testing.T) {
	ctx := context.Background()

	t.Run("the agent commits, then re-reports BLOCKED", func(t *testing.T) {
		f, lc := newBlockedFinishFixture(t)
		f.wt.commitSubjects = []string{"fix: bump the flows pin"}
		report := "flows still red after bumping the pin"
		if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "agent-1", &report); err != nil {
			t.Fatalf("AcceptBlockedRunFinish: %v", err)
		}
		res, err := lc.FinishUnattendedRun(ctx, "sess-1")
		if err != nil || res.Outcome != domain.CronJobOutcomeRunBlocked {
			t.Fatalf("FinishUnattendedRun = %+v, %v; want run_blocked", res, err)
		}
		f.assertBlockedDraft(t, "run blocked: flows still red after bumping the pin")
		if got := f.sessions.sessions["sess-1"].LastRepairAttemptCount; got != 5 {
			t.Fatalf("repair attempts = %d, want the spent budget kept", got)
		}
	})

	t.Run("the re-report keeps only its first line", func(t *testing.T) {
		f, lc := newBlockedFinishFixture(t)
		report := "  flows still red\nsee the CI log for the failing step  "
		if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "agent-1", &report); err != nil {
			t.Fatalf("AcceptBlockedRunFinish: %v", err)
		}
		if res, err := lc.FinishUnattendedRun(ctx, "sess-1"); err != nil || res.Outcome != domain.CronJobOutcomeRunBlocked {
			t.Fatalf("FinishUnattendedRun = %+v, %v; want run_blocked", res, err)
		}
		f.assertBlockedDraft(t, "run blocked: flows still red")
	})

	t.Run("a plain finish readies the PR", func(t *testing.T) {
		f, lc := newBlockedFinishFixture(t)
		if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "agent-1", nil); err != nil {
			t.Fatalf("AcceptBlockedRunFinish: %v", err)
		}
		res, err := lc.FinishUnattendedRun(ctx, "sess-1")
		if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
			t.Fatalf("FinishUnattendedRun = %+v, %v; want pr_created", res, err)
		}
		got := f.sessions.sessions["sess-1"]
		if len(f.vp.markReadyCalls) != 1 || f.vp.markReadyCalls[0] != 77 {
			t.Fatalf("MarkReadyForReview calls = %v, want [77]", f.vp.markReadyCalls)
		}
		if got.State != machine.ReadyForReview || got.BlockedReason != nil || got.LastRepairAttemptCount != 0 {
			t.Fatalf("state=%v reason=%v repair attempts=%d, want ReadyForReview, no block and a fresh budget",
				got.State, got.BlockedReason, got.LastRepairAttemptCount)
		}
	})

	for name, refused := range map[string]func(*testing.T, *runBlockedFixture, *Lifecycle){
		"another chat of the session finishes": func(t *testing.T, f *runBlockedFixture, lc *Lifecycle) {
			if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "operator-chat", nil); !errors.Is(err, ErrNotRunChat) {
				t.Fatalf("AcceptBlockedRunFinish = %v, want ErrNotRunChat", err)
			}
		},
		"no claim: the operator, a callback or the stranded sweep": func(t *testing.T, _ *runBlockedFixture, lc *Lifecycle) {
			if err := lc.ReportRunBlocked(ctx, "sess-1", "anything"); !errors.Is(err, ErrRunAlreadyFinalized) {
				t.Fatalf("ReportRunBlocked = %v, want ErrRunAlreadyFinalized", err)
			}
		},
		"a claim from an earlier block": func(t *testing.T, f *runBlockedFixture, lc *Lifecycle) {
			sess := f.sessions.sessions["sess-1"]
			if err := lc.AcceptBlockedRunFinish(sess, "agent-1", nil); err != nil {
				t.Fatalf("AcceptBlockedRunFinish: %v", err)
			}
			reblocked := blockedRunAt.Add(time.Hour)
			sess.StateEnteredAt = &reblocked
		},
		"another live session holds the PR": func(t *testing.T, f *runBlockedFixture, lc *Lifecycle) {
			if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "agent-1", nil); err != nil {
				t.Fatalf("AcceptBlockedRunFinish: %v", err)
			}
			f.sessions.sessions["rival"] = &models.Session{ID: "rival", RepoID: "repo-1", Title: "Rival", State: machine.AwaitingChecks, PRNumber: intPtr(77), BranchName: "other"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, lc := newBlockedFinishFixture(t)
			refused(t, f, lc)
			res, err := lc.FinishUnattendedRun(ctx, "sess-1")
			if err != nil || !res.NoOp {
				t.Fatalf("FinishUnattendedRun = %+v, %v; want NoOp", res, err)
			}
			got := f.sessions.sessions["sess-1"]
			if got.State != machine.Blocked || got.LastRepairAttemptCount != 5 || len(f.vp.markReadyCalls) != 0 {
				t.Fatalf("state=%v repair attempts=%d ready calls=%v, want Blocked, budget spent and the PR draft",
					got.State, got.LastRepairAttemptCount, f.vp.markReadyCalls)
			}
		})
	}

	t.Run("the block moved between the claim and the lock", func(t *testing.T) {
		f, lc := newBlockedFinishFixture(t)
		sess := f.sessions.sessions["sess-1"]
		reblocked := blockedRunAt.Add(time.Hour)
		sess.StateEnteredAt = &reblocked
		res, err := lc.finishBlockedRun(ctx, sess, blockedFinishClaim{blockedAt: blockedRunAt})
		if err != nil || !res.NoOp {
			t.Fatalf("finishBlockedRun = %+v, %v; want NoOp", res, err)
		}
		if sess.State != machine.Blocked || sess.LastRepairAttemptCount != 5 || len(f.vp.markReadyCalls) != 0 {
			t.Fatalf("state=%v repair attempts=%d ready calls=%v, want Blocked, budget spent and the PR draft",
				sess.State, sess.LastRepairAttemptCount, f.vp.markReadyCalls)
		}
	})

	t.Run("a zero-output run cannot be finished from Blocked", func(t *testing.T) {
		f, lc := newBlockedFinishFixture(t)
		sess := f.sessions.sessions["sess-1"]
		sess.IsQuickChat = true
		if err := lc.AcceptBlockedRunFinish(sess, "agent-1", nil); !errors.Is(err, ErrRunAlreadyFinalized) {
			t.Fatalf("AcceptBlockedRunFinish = %v, want ErrRunAlreadyFinalized", err)
		}
	})
}

// TestBlockedRunDroppedFinishLeavesNoReport: a finish --blocked from Blocked
// keeps its reason with the claim until the finish fires. A daemon restart, or
// a new chat that cancels the pending finish, drops both, so the row keeps its
// old reason and block time and a later plain finish from the run's chat
// readies the PR.
func TestBlockedRunDroppedFinishLeavesNoReport(t *testing.T) {
	ctx := context.Background()
	for name, after := range map[string]func(*testing.T, *runBlockedFixture, *Lifecycle) *Lifecycle{
		"a daemon restart": func(t *testing.T, f *runBlockedFixture, _ *Lifecycle) *Lifecycle {
			return newFinalizeChatLifecycle(t, f.sessions, f.repos, f.chats, f.cron, f.wt, f.vp, zerolog.Nop())
		},
		"a new chat cancels the pending finish": func(_ *testing.T, _ *runBlockedFixture, lc *Lifecycle) *Lifecycle {
			return lc
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, lc := newBlockedFinishFixture(t)
			report := "flows still red after bumping the pin"
			if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "agent-1", &report); err != nil {
				t.Fatalf("AcceptBlockedRunFinish: %v", err)
			}
			row := f.sessions.sessions["sess-1"]
			if row.RunBlockedReason != nil || row.State != machine.Blocked || !row.StateEnteredAt.Equal(blockedRunAt) ||
				row.BlockedReason == nil || *row.BlockedReason != "run blocked: flows CI remains red after 5 repair passes" {
				t.Fatalf("run_blocked_reason=%v state=%v blocked at=%v reason=%v, want the row as it was",
					row.RunBlockedReason, row.State, row.StateEnteredAt, row.BlockedReason)
			}
			lc = after(t, f, lc)
			if err := lc.AcceptBlockedRunFinish(row, "agent-1", nil); err != nil {
				t.Fatalf("AcceptBlockedRunFinish: %v", err)
			}
			res, err := lc.FinishUnattendedRun(ctx, "sess-1")
			if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
				t.Fatalf("FinishUnattendedRun = %+v, %v; want pr_created", res, err)
			}
			got := f.sessions.sessions["sess-1"]
			if len(f.vp.markReadyCalls) != 1 || got.State != machine.ReadyForReview || got.RunBlockedReason != nil || got.BlockedReason != nil {
				t.Fatalf("ready calls=%v state=%v run_blocked_reason=%v reason=%v, want the PR readied and no block",
					f.vp.markReadyCalls, got.State, got.RunBlockedReason, got.BlockedReason)
			}
		})
	}

	t.Run("a plain finish ignores a report an earlier daemon left on the row", func(t *testing.T) {
		f, lc := newBlockedFinishFixture(t)
		stale := "flows still red after bumping the pin"
		f.sessions.sessions["sess-1"].RunBlockedReason = &stale
		if err := lc.AcceptBlockedRunFinish(f.sessions.sessions["sess-1"], "agent-1", nil); err != nil {
			t.Fatalf("AcceptBlockedRunFinish: %v", err)
		}
		res, err := lc.FinishUnattendedRun(ctx, "sess-1")
		if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
			t.Fatalf("FinishUnattendedRun = %+v, %v; want pr_created", res, err)
		}
		if got := f.sessions.sessions["sess-1"]; len(f.vp.markReadyCalls) != 1 || got.State != machine.ReadyForReview || got.RunBlockedReason != nil {
			t.Fatalf("ready calls=%v state=%v run_blocked_reason=%v, want the PR readied", f.vp.markReadyCalls, got.State, got.RunBlockedReason)
		}
	})
}
