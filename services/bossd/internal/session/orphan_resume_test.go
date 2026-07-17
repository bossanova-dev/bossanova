package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
)

// newOrphanResumeFixture reuses the rotation fixture but marks the session
// Orphaned with the daemon-restart orphan blocked_reason (the resume candidate
// shape) and opts auto-resume in. The runner/repos/adapters remain injectable.
func newOrphanResumeFixture(t *testing.T) *rotationFixture {
	t.Helper()
	f := newRotationFixture(t)
	enabled := true
	f.lc.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeOrphans: &enabled})
	s := f.sessions.sessions[f.sessionID]
	s.State = machine.Orphaned
	reason := OrphanedHeadlessRunReason
	s.BlockedReason = &reason
	return f
}

// --- Happy path: CAS to ImplementingPlan, resume prior session, steer, re-arm ---

func TestResumeOrphanedHeadlessRuns_HappyPath(t *testing.T) {
	f := newOrphanResumeFixture(t)
	armer := &fakePollArmer{}
	f.lc.pollArmer = armer
	f.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{ID: "chat-primary", SessionID: f.sessionID, AgentSessionID: "agent-old"}},
	}}

	n := f.lc.ResumeOrphanedHeadlessRuns(context.Background())
	if n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}

	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", s.State)
	}
	// The daemon-restart orphan marker must be cleared on a successful resume so
	// the web UI does not surface a stale "headless run orphaned" warning on the
	// now-live run.
	if s.BlockedReason != nil {
		t.Errorf("BlockedReason = %v, want nil (cleared on successful resume)", *s.BlockedReason)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("StartByAgent calls = %d, want 1", len(f.runner.started))
	}
	call := f.runner.started[0]
	wantPrefix := "Your previous turn was interrupted by a daemon restart. Verify current workspace/git/PR state before repeating any action (commits, pushes, comments may already exist)."
	if !strings.HasPrefix(call.plan, wantPrefix) {
		t.Errorf("resumed prompt missing steering prefix:\n got: %q", call.plan)
	}
	if call.plan != wantPrefix+"\n\nORIGINAL PLAN BODY" {
		t.Errorf("resumed prompt = %q, want steering + \\n\\n + plan", call.plan)
	}
	if call.resume == nil || *call.resume != "agent-old" {
		t.Errorf("resume = %v, want agent-old (prior agent session)", call.resume)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-new" {
		t.Errorf("AgentSessionID = %v, want agent-new", s.AgentSessionID)
	}
	chats := f.lc.agentChats.(*mockAgentChatStore)
	if len(chats.agentSessionIDUpdates) != 1 {
		t.Fatalf("primary chat updates = %d, want 1", len(chats.agentSessionIDUpdates))
	}
	if got := chats.agentSessionIDUpdates[0]; got.id != "chat-primary" || got.oldAgentSessionID != "agent-old" || got.newAgentSessionID != "agent-new" {
		t.Errorf("primary chat update = %+v, want agent-old -> agent-new", got)
	}
	// Same account, not a rotation: the rotation attempt count must not move.
	if s.RotationAttemptCount != 0 {
		t.Errorf("RotationAttemptCount = %d, want 0 (orphan resume is not a rotation)", s.RotationAttemptCount)
	}
	if !armer.armCalled || armer.armedID != "agent-new" {
		t.Errorf("poll fallback not re-armed for resumed run: armed=%v id=%q", armer.armCalled, armer.armedID)
	}
}

func TestResumeOrphanedHeadlessRuns_UpdatesOnlyFetchedPrimaryChat(t *testing.T) {
	f := newOrphanResumeFixture(t)
	primary := &models.AgentChat{ID: "chat-primary", SessionID: f.sessionID, AgentSessionID: "agent-old"}
	duplicate := &models.AgentChat{ID: "chat-duplicate", SessionID: f.sessionID, AgentSessionID: "agent-old"}
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {primary, duplicate},
	}}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if got := primary.AgentSessionID; got != "agent-new" {
		t.Errorf("primary chat agent session id = %q, want agent-new", got)
	}
	if got := duplicate.AgentSessionID; got != "agent-old" {
		t.Errorf("duplicate chat agent session id = %q, want agent-old", got)
	}
}

func TestResumeOrphanedHeadlessRuns_UsesPrimaryChatSpawnEnvironment(t *testing.T) {
	f := newOrphanResumeFixture(t)
	primaryAccountID := "acct-primary"
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "agent-old",
			AgentName:      "codex",
			Model:          "gpt-5",
			AccountID:      &primaryAccountID,
		}},
	}}
	accountEnv := &fakeAccountEnvResolver{env: map[string]string{"ACCOUNT_TOKEN": "primary"}}
	f.lc.SetAccountEnvResolver(accountEnv)

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}

	if accountEnv.gotSess == nil {
		t.Fatal("account environment was not resolved")
	}
	if got := accountEnv.gotSess.AgentName; got != "codex" {
		t.Errorf("account environment agent = %q, want primary chat agent codex", got)
	}
	if got := accountEnv.gotSess.Model; got != "gpt-5" {
		t.Errorf("account environment model = %q, want primary chat model gpt-5", got)
	}
	if got := accountEnv.gotSess.AccountID; got == nil || *got != primaryAccountID {
		t.Errorf("account environment account = %v, want primary chat account %q", got, primaryAccountID)
	}
	if got := f.runner.started[0].model; got != "gpt-5" {
		t.Errorf("resumed model = %q, want primary chat model gpt-5", got)
	}
	if got := f.runner.started[0].env["ACCOUNT_TOKEN"]; got != "primary" {
		t.Errorf("resumed account environment token = %q, want primary", got)
	}
}

func TestResumeOrphanedHeadlessRuns_ArmsPollFallbackWithPrimaryChatAgent(t *testing.T) {
	f := newOrphanResumeFixture(t)
	primaryAccountID := "acct-primary"
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "agent-old",
			AgentName:      "codex",
			Model:          "gpt-5",
			AccountID:      &primaryAccountID,
		}},
	}}
	claudeClient := newFakeAgent()
	codexClient := newFakeAgent()
	f.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": claudeClient, "codex": codexClient})
	armer := &fakePollArmer{}
	f.lc.SetPollArmer(armer)

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if armer.client != codexClient {
		t.Errorf("poll fallback client = %T, want primary chat codex client", armer.client)
	}
}

func TestBootstrap_DefersOrphanAutoResumeUntilAfterAdvancement(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.lc.worktrees = &mockWorktreeManager{}

	f.lc.Bootstrap(context.Background())

	if got := f.sessions.sessions[f.sessionID].State; got != machine.Orphaned {
		t.Errorf("state after Bootstrap = %v, want Orphaned before startup advancement", got)
	}
	if got := len(f.runner.started); got != 0 {
		t.Errorf("Bootstrap StartByAgent calls = %d, want 0 before startup advancement", got)
	}
}

func TestResumeOrphanedHeadlessRuns_StartsStatusTracking(t *testing.T) {
	f := newOrphanResumeFixture(t)
	status := &recordingChatStatus{}
	f.lc.SetChatStatus(status)
	f.lc.headlessStatusPollInterval = time.Millisecond

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}

	deadline := time.Now().Add(time.Second)
	for {
		for _, update := range status.snapshot() {
			if update.id == "agent-new" && update.status == pb.ChatStatus_CHAT_STATUS_WORKING {
				if err := f.runner.Stop("agent-new"); err != nil {
					t.Fatalf("stop resumed run: %v", err)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("resumed run never reported WORKING status for agent-new")
		}
		time.Sleep(time.Millisecond)
	}
}

// --- Disabled / ineligible no-ops: row stays Orphaned, no StartByAgent ---

func TestResumeOrphanedHeadlessRuns_DisabledAndIneligible(t *testing.T) {
	cases := []struct {
		name string
		mut  func(t *testing.T, f *rotationFixture)
	}{
		{
			name: "knob disabled (nil default)",
			mut: func(_ *testing.T, f *rotationFixture) {
				f.lc.SetRotationConfig(config.ManagedAccountsConfig{}) // AutoResumeOrphans nil ⇒ OFF
			},
		},
		{
			name: "knob explicitly false",
			mut: func(_ *testing.T, f *rotationFixture) {
				off := false
				f.lc.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeOrphans: &off})
			},
		},
		{
			name: "missing orphan marker",
			mut: func(_ *testing.T, f *rotationFixture) {
				other := "some unrelated blocked reason"
				f.sessions.sessions[f.sessionID].BlockedReason = &other
			},
		},
		{
			name: "nil blocked reason",
			mut: func(_ *testing.T, f *rotationFixture) {
				f.sessions.sessions[f.sessionID].BlockedReason = nil
			},
		},
		{
			name: "empty agent session id",
			mut: func(_ *testing.T, f *rotationFixture) {
				f.sessions.sessions[f.sessionID].AgentSessionID = nil
			},
		},
		{
			name: "unwired agent runner",
			mut: func(_ *testing.T, f *rotationFixture) {
				f.lc.agentRunner = nil
			},
		},
		{
			name: "archived orphan candidate",
			mut: func(_ *testing.T, f *rotationFixture) {
				archivedAt := time.Now()
				f.sessions.sessions[f.sessionID].ArchivedAt = &archivedAt
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanResumeFixture(t)
			tc.mut(t, f)

			n := f.lc.ResumeOrphanedHeadlessRuns(context.Background())
			if n != 0 {
				t.Fatalf("resumed = %d, want 0", n)
			}
			if got := f.sessions.sessions[f.sessionID].State; got != machine.Orphaned {
				t.Errorf("state = %v, want Orphaned (unchanged)", got)
			}
			if len(f.runner.started) != 0 {
				t.Errorf("StartByAgent calls = %d, want 0", len(f.runner.started))
			}
		})
	}
}

func TestResumeOrphanedHeadlessRuns_PersistFailureReparksAndStopsRestart(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.sessions.orphanResumeCommitErr = errors.New("sqlite write failed")

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.Orphaned {
		t.Errorf("state = %v, want Orphaned after persistence failure", s.State)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-old" {
		t.Errorf("AgentSessionID = %v, want original agent-old", s.AgentSessionID)
	}
	if len(f.runner.stopped) != 1 || f.runner.stopped[0] != "agent-new" {
		t.Errorf("stopped runs = %v, want [agent-new]", f.runner.stopped)
	}
}

// --- Primary-chat sync failure rolls back to Orphaned and re-stamps the marker ---

func TestResumeOrphanedHeadlessRuns_PrimaryChatSyncFailureRestampsMarker(t *testing.T) {
	f := newOrphanResumeFixture(t)
	// No chat exists for the prior agent session and Create fails, so the
	// primary-chat sync errors *after* the ImplementingPlan persist has already
	// cleared the orphan marker.
	f.lc.agentChats = &mockAgentChatStore{createErr: errors.New("sqlite write failed")}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0 on primary-chat sync failure", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.Orphaned {
		t.Errorf("state = %v, want Orphaned after sync-failure rollback", s.State)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-old" {
		t.Errorf("AgentSessionID = %v, want restored agent-old", s.AgentSessionID)
	}
	// The marker cleared by the ImplementingPlan persist must be re-stamped so
	// the next sweep still treats the row as an orphan candidate.
	if s.BlockedReason == nil || *s.BlockedReason != OrphanedHeadlessRunReason {
		t.Errorf("orphan marker not re-stamped on rollback: %v", s.BlockedReason)
	}
	if len(f.runner.stopped) != 1 || f.runner.stopped[0] != "agent-new" {
		t.Errorf("stopped runs = %v, want [agent-new]", f.runner.stopped)
	}
}

// --- Double failure: sync fails AND the rollback write fails → row stays
// ImplementingPlan (self-heals on restart), never a markerless Orphaned ---

func TestResumeOrphanedHeadlessRuns_RollbackFailureLeavesImplementingPlan(t *testing.T) {
	f := newOrphanResumeFixture(t)
	// Primary-chat sync fails after the ImplementingPlan persist has already
	// cleared the marker and swapped in the resume id.
	f.lc.agentChats = &mockAgentChatStore{createErr: errors.New("sqlite write failed")}
	// The conditional rollback fails, so the row remains in its committed shape.
	f.sessions.orphanResumeReparkErr = errors.New("sqlite write failed on rollback")

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0 on double failure", n)
	}
	s := f.sessions.sessions[f.sessionID]
	// The row must NOT be moved to a markerless Orphaned (which every future
	// sweep skips). Left in ImplementingPlan, sweepOrphanedHeadlessRuns
	// re-detects and re-marks it on the next daemon restart.
	if s.State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan after failed rollback", s.State)
	}
	if len(f.runner.stopped) != 1 || f.runner.stopped[0] != "agent-new" {
		t.Errorf("stopped runs = %v, want [agent-new]", f.runner.stopped)
	}
}

// --- Restart failure re-parks Orphaned; a later sweep retries and succeeds ---

func TestResumeOrphanedHeadlessRuns_RestartFailureReparksThenRetries(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.runner.startErr = errors.New("boom: agent plugin not ready")

	n := f.lc.ResumeOrphanedHeadlessRuns(context.Background())
	if n != 0 {
		t.Fatalf("resumed = %d, want 0 on restart failure", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.Orphaned {
		t.Errorf("state = %v, want Orphaned (re-parked after failed restart)", s.State)
	}
	if len(f.runner.started) != 1 {
		t.Errorf("StartByAgent attempts = %d, want 1", len(f.runner.started))
	}
	// The marker is untouched, so the row is still a candidate.
	if s.BlockedReason == nil || *s.BlockedReason != OrphanedHeadlessRunReason {
		t.Errorf("orphan marker cleared by a failed restart: %v", s.BlockedReason)
	}

	// Next sweep: the transient error clears and the resume succeeds.
	f.runner.startErr = nil
	n = f.lc.ResumeOrphanedHeadlessRuns(context.Background())
	if n != 1 {
		t.Fatalf("retry resumed = %d, want 1", n)
	}
	if s.State != machine.ImplementingPlan {
		t.Errorf("retry state = %v, want ImplementingPlan", s.State)
	}
}

// --- Idempotency: a second pass finds no Orphaned marker row and no-ops ---

func TestResumeOrphanedHeadlessRuns_Idempotent(t *testing.T) {
	f := newOrphanResumeFixture(t)

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("first pass resumed = %d, want 1", n)
	}
	if f.sessions.sessions[f.sessionID].State != machine.ImplementingPlan {
		t.Fatalf("first pass did not advance the row")
	}

	// Second pass: the row is ImplementingPlan (no Orphaned marker), so it is not
	// a candidate — no double-start.
	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("second pass resumed = %d, want 0 (idempotent)", n)
	}
	if len(f.runner.started) != 1 {
		t.Errorf("StartByAgent calls = %d across two passes, want 1", len(f.runner.started))
	}
}

// --- Race: a concurrent completion advanced the row first; the resume CAS loses ---

func TestResumeOrphanedHeadlessRuns_ConcurrentCompletionRaceNoop(t *testing.T) {
	f := newOrphanResumeFixture(t)
	s := f.sessions.sessions[f.sessionID]
	// Simulate a concurrent completion signal that already advanced the row out
	// of Orphaned before the resume primitive's CAS runs.
	s.State = machine.ImplementingPlan

	if ok := f.lc.resumeOrphanedRun(context.Background(), f.sessions, s); ok {
		t.Fatalf("resumeOrphanedRun = true, want false (CAS should lose)")
	}
	if len(f.runner.started) != 0 {
		t.Errorf("StartByAgent calls = %d, want 0 (no start on lost CAS)", len(f.runner.started))
	}
}

func TestResumeOrphanedHeadlessRuns_ArchiveRaceNoop(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.sessions.updateStateConditionalHook = func(id string) {
		archivedAt := time.Now()
		f.sessions.sessions[id].ArchivedAt = &archivedAt
	}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0 after archive wins claim race", n)
	}
	if got := f.sessions.sessions[f.sessionID].State; got != machine.Orphaned {
		t.Errorf("state = %v, want Orphaned after archive wins claim race", got)
	}
	if len(f.runner.started) != 0 {
		t.Errorf("StartByAgent calls = %d, want 0 after archive wins claim race", len(f.runner.started))
	}
}

// A completion can win after the resume claim but before the new agent ID is
// committed. The resume handoff must not revive that terminal transition.
func TestResumeOrphanedHeadlessRuns_CompletionAfterClaimStopsRestart(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.sessions.orphanResumeCommitHook = func(id string) {
		f.sessions.sessions[id].State = machine.Blocked
	}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0 after completion wins handoff", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.Blocked {
		t.Errorf("state = %v, want Blocked completion state", s.State)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-old" {
		t.Errorf("AgentSessionID = %v, want original agent-old", s.AgentSessionID)
	}
	if len(f.runner.stopped) != 1 || f.runner.stopped[0] != "agent-new" {
		t.Errorf("stopped = %v, want resumed agent-new stopped", f.runner.stopped)
	}
}

// An archive can win after the resume claim but before the new agent ID is
// committed. The handoff must leave the archive and old run identity intact.
func TestResumeOrphanedHeadlessRuns_ArchiveAfterClaimStopsRestart(t *testing.T) {
	f := newOrphanResumeFixture(t)
	archivedAt := time.Now()
	f.sessions.orphanResumeCommitHook = func(id string) {
		f.sessions.sessions[id].ArchivedAt = &archivedAt
	}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0 after archive wins handoff", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.ArchivedAt == nil {
		t.Fatalf("ArchivedAt = nil, want archive preserved")
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-old" {
		t.Errorf("AgentSessionID = %v, want original agent-old", s.AgentSessionID)
	}
	if len(f.runner.stopped) != 1 || f.runner.stopped[0] != "agent-new" {
		t.Errorf("stopped = %v, want resumed agent-new stopped", f.runner.stopped)
	}
}

// RetrySession can clear the orphan marker after this sweep claims the row but
// before it persists the replacement. A lost handoff must release the claimed
// ImplementingPlan state without restoring the marker Retry intentionally
// cleared; otherwise the dead prior run is left looking active forever.
func TestResumeOrphanedHeadlessRuns_RetryAfterClaimReleasesMarkerlessClaim(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.sessions.orphanResumeCommitHook = func(id string) {
		f.sessions.sessions[id].BlockedReason = nil
		f.sessions.sessions[id].AutomationEnabled = true
	}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0 after retry wins handoff", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.Orphaned {
		t.Errorf("state = %v, want Orphaned after retry clears claim marker", s.State)
	}
	if s.BlockedReason != nil {
		t.Errorf("BlockedReason = %v, want nil preserved from RetrySession", s.BlockedReason)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-old" {
		t.Errorf("AgentSessionID = %v, want original agent-old", s.AgentSessionID)
	}
	if len(f.runner.stopped) != 1 || f.runner.stopped[0] != "agent-new" {
		t.Errorf("stopped = %v, want resumed agent-new stopped", f.runner.stopped)
	}
}

// SessionStore.Update can fail after committing its write because it re-reads
// the row. The orphan handoff must use its own affected-row primitive instead.
func TestResumeOrphanedHeadlessRuns_PostWriteReadFailureDoesNotUseGenericUpdate(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.sessions.updateHook = func(id string, params db.UpdateSessionParams) error {
		if params.AgentSessionID == nil {
			return nil
		}
		s := f.sessions.sessions[id]
		s.AgentSessionID = *params.AgentSessionID
		s.State = machine.State(*params.State)
		s.BlockedReason = *params.BlockedReason
		return errors.New("post-write session read failed")
	}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1 without generic Update", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", s.State)
	}
	if s.BlockedReason != nil {
		t.Errorf("BlockedReason = %v, want marker cleared", s.BlockedReason)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-new" {
		t.Errorf("AgentSessionID = %v, want resumed agent-new", s.AgentSessionID)
	}
}

// --- Restart-safety: a fresh Lifecycle over the same store resumes on next tick ---

func TestResumeOrphanedHeadlessRuns_SurvivesDaemonRestart(t *testing.T) {
	f := newOrphanResumeFixture(t)

	runner := newMockAgentRunner()
	runner.nextID = "agent-new2"
	enabled := true
	fresh := &Lifecycle{
		sessions:    f.sessions, // SAME store holding the orphaned session
		repos:       f.lc.repos,
		agentRunner: runner,
		logger:      zerolog.Nop(),
	}
	fresh.SetProofEnvResolver(fakeProofEnvResolver{env: map[string]string{}})
	fresh.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeOrphans: &enabled})

	n := fresh.ResumeOrphanedHeadlessRuns(context.Background())
	if n != 1 {
		t.Fatalf("fresh lifecycle resumed = %d, want 1", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.ImplementingPlan {
		t.Errorf("fresh lifecycle did not advance orphaned session: state=%v", s.State)
	}
	if len(runner.started) != 1 {
		t.Errorf("fresh runner StartByAgent = %d, want 1", len(runner.started))
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-new2" {
		t.Errorf("AgentSessionID = %v, want agent-new2", s.AgentSessionID)
	}
}

// --- Config knob: default OFF, opt-in ON ---

func TestAutoResumeOrphansEnabled(t *testing.T) {
	if (config.ManagedAccountsConfig{}).AutoResumeOrphansEnabled() {
		t.Errorf("AutoResumeOrphansEnabled() = true for unset knob, want false (opt-in)")
	}
	off := false
	if (config.ManagedAccountsConfig{AutoResumeOrphans: &off}).AutoResumeOrphansEnabled() {
		t.Errorf("AutoResumeOrphansEnabled() = true for explicit false")
	}
	on := true
	if !(config.ManagedAccountsConfig{AutoResumeOrphans: &on}).AutoResumeOrphansEnabled() {
		t.Errorf("AutoResumeOrphansEnabled() = false for explicit true")
	}
}
