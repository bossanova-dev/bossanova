package session

import (
	"context"
	"os/exec"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

const reattachPane = "boss-repo-chat-1"

// paneFake is a fakeTmux whose has-session answers true only for livePanes and
// whose `#{pane_dead}` reads 1 for deadPanes: the tmux session is still there
// (remain-on-exit) but its agent process has exited.
type paneFake struct {
	*fakeTmux
	deadPanes map[string]bool
}

func (f *paneFake) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	if len(args) >= 4 && args[0] == "display-message" && args[len(args)-1] == "#{pane_dead}" && f.deadPanes[args[len(args)-2]] {
		return exec.CommandContext(ctx, "printf", "1")
	}
	return f.fakeTmux.factory(ctx, name, args...)
}

// paneTmux returns a tmux client over a paneFake, plus the fake so a test can
// mark a pane dead.
func paneTmux(livePanes ...string) (*tmux.Client, *paneFake) {
	fake := &paneFake{fakeTmux: newFakeTmux(), deadPanes: map[string]bool{}}
	fake.hasSessionFunc = func(calls []recordedTmuxCall) bool {
		args := calls[len(calls)-1].args
		return slices.Contains(livePanes, args[len(args)-1])
	}
	return tmux.NewClient(tmux.WithPasteSettleWait(testPasteSettleWait), tmux.WithCommandFactory(fake.factory)), fake
}

func (m *mockSessionStore) MarkOrphanReattached(_ context.Context, id, reason string, newState int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.State != machine.Orphaned || s.ArchivedAt != nil || s.BlockedReason == nil || *s.BlockedReason != reason {
		return false, nil
	}
	s.State = machine.State(newState)
	s.BlockedReason = nil
	return true, nil
}

func chatWithPane(sessionID string) *domain.AgentChat {
	pane := reattachPane
	return &domain.AgentChat{ID: "chat-1", SessionID: sessionID, AgentSessionID: "chat-agent-1", TmuxSessionName: &pane}
}

// newFalseOrphanLifecycle builds a Lifecycle holding one session in the
// daemon-restart Orphaned shape, with one chat row pointing at reattachPane.
func newFalseOrphanLifecycle(t *testing.T, livePanes ...string) (*Lifecycle, *mockSessionStore, *mockAgentRunner, *paneFake) {
	t.Helper()
	sessions := newMockSessionStore()
	runID := "run-prior"
	reason := OrphanedHeadlessRunReason
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		AgentName:      "claude",
		WorktreePath:   t.TempDir(),
		State:          machine.Orphaned,
		AgentSessionID: &runID,
		BlockedReason:  &reason,
	}
	runner := newMockAgentRunner()
	client, fake := paneTmux(livePanes...)
	lc := &Lifecycle{
		sessions:    sessions,
		repos:       newMockRepoStore(),
		agentRunner: runner,
		agentChats: &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
			"sess-1": {chatWithPane("sess-1")},
		}},
		tmux:   client,
		logger: zerolog.Nop(),
	}
	return lc, sessions, runner, fake
}

func TestBootstrapDoesNotOrphanRunWhoseChatPaneIsLive(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	runID := "run-live-pane"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-live-pane",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "claude",
		AgentSessionID: &runID,
	}
	chats := &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
		"sess-1": {chatWithPane("sess-1")},
	}}
	client, _ := paneTmux(reattachPane)
	lc := newTestLifecycle(sessions, newMockRepoStore(), chats, &stubCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), client, newMockVCSProvider(), zerolog.Nop())
	// The run's own process is gone, so the session-level verdict is dead.
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}})
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	s := sessions.sessions["sess-1"]
	if s.State != machine.ImplementingPlan || s.BlockedReason != nil {
		t.Fatalf("a run with a live chat pane must not be orphaned, got %s reason %v", s.State, s.BlockedReason)
	}
}

func TestRestoreLiveOrphansReattachesFalseOrphanWithoutStartingARun(t *testing.T) {
	lc, sessions, runner, _ := newFalseOrphanLifecycle(t, reattachPane)

	if n := lc.RestoreLiveOrphans(context.Background()); n != 1 {
		t.Fatalf("restored = %d, want 1", n)
	}

	s := sessions.sessions["sess-1"]
	if s.State != machine.ImplementingPlan {
		t.Errorf("state = %s, want ImplementingPlan", s.State)
	}
	if s.BlockedReason != nil {
		t.Errorf("blocked reason = %q, want cleared", *s.BlockedReason)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "run-prior" {
		t.Errorf("agent session id = %v, want run-prior kept", s.AgentSessionID)
	}
	if len(runner.started) != 0 {
		t.Errorf("started %d runs; reattaching a live pane must never start one", len(runner.started))
	}
}

func TestRestoreLiveOrphansLeavesRealOrphansOrphaned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(lc *Lifecycle, s *models.Session, fake *paneFake)
		live  []string
	}{
		{name: "pane gone"},
		{
			name:  "pane process exited",
			live:  []string{reattachPane},
			setup: func(_ *Lifecycle, _ *models.Session, fake *paneFake) { fake.deadPanes[reattachPane] = true },
		},
		{
			name: "other orphan reason",
			live: []string{reattachPane},
			setup: func(_ *Lifecycle, s *models.Session, _ *paneFake) {
				reason := "orphaned by something else"
				s.BlockedReason = &reason
			},
		},
		{
			name: "archived",
			live: []string{reattachPane},
			setup: func(_ *Lifecycle, s *models.Session, _ *paneFake) {
				archived := s.CreatedAt
				s.ArchivedAt = &archived
			},
		},
		{
			name: "retired chat",
			live: []string{reattachPane},
			setup: func(lc *Lifecycle, _ *models.Session, _ *paneFake) {
				chat := chatWithPane("sess-1")
				startErr := "spawn failed"
				chat.StartError = &startErr
				lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{"sess-1": {chat}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc, sessions, runner, fake := newFalseOrphanLifecycle(t, tc.live...)
			s := sessions.sessions["sess-1"]
			if tc.setup != nil {
				tc.setup(lc, s, fake)
			}
			before := s.BlockedReason

			if n := lc.RestoreLiveOrphans(context.Background()); n != 0 {
				t.Fatalf("restored = %d, want 0", n)
			}
			if s.State != machine.Orphaned || s.BlockedReason != before {
				t.Errorf("state %s reason %v, want Orphaned with its reason untouched", s.State, s.BlockedReason)
			}
			if len(runner.started) != 0 {
				t.Errorf("started %d runs, want 0", len(runner.started))
			}
		})
	}
}

// TestResumeOrphanedHeadlessRunsSkipsRunWhoseChatPaneIsLive keeps the opt-in
// auto-resume from starting a second agent beside a live one.
func TestResumeOrphanedHeadlessRunsSkipsRunWhoseChatPaneIsLive(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
		f.sessionID: {
			{ID: "chat-primary", SessionID: f.sessionID, AgentSessionID: "agent-old"},
			chatWithPane(f.sessionID),
		},
	}}
	f.lc.tmux, _ = paneTmux(reattachPane)

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 0 {
		t.Fatalf("resumed = %d, want 0", n)
	}
	if len(f.runner.started) != 0 {
		t.Fatalf("StartByAgent calls = %d, want 0", len(f.runner.started))
	}
	if got := f.sessions.sessions[f.sessionID].State; got != machine.Orphaned {
		t.Fatalf("state = %s, want Orphaned (restoring it is RestoreLiveOrphans' job)", got)
	}
}

// TestResumeOrphanedHeadlessRunsStillResumesWhenPaneIsGone is the unchanged
// auto-resume path with tmux wired: no live pane, so the real orphan resumes.
func TestResumeOrphanedHeadlessRunsStillResumesWhenPaneIsGone(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
		f.sessionID: {
			{ID: "chat-primary", SessionID: f.sessionID, AgentSessionID: "agent-old"},
			chatWithPane(f.sessionID),
		},
	}}
	f.lc.tmux, _ = paneTmux()

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("StartByAgent calls = %d, want 1", len(f.runner.started))
	}
}
