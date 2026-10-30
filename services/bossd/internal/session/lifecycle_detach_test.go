package session

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
)

func TestStartSession_DetachedExistingPRRunsPlan_Sessions_AC128(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tmuxAvailable bool
	}{
		{name: "tmux hosted", tmuxAvailable: true},
		{name: "headless fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			chats := &mockAgentChatStore{}
			wt := &mockWorktreeManager{}
			runner := newMockAgentRunner()
			provider := newMockVCSProvider()
			fake := newFakeTmux()
			fake.available = tc.tmuxAvailable
			tx := tmux.NewClient(tmux.WithPasteSettleWait(testPasteSettleWait), tmux.WithCommandFactory(fake.factory))
			repos.repos["repo-1"] = &models.Repo{
				ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main",
				WorktreeBaseDir: "/tmp/worktrees", OriginURL: "owner/repo",
			}
			const plan = "Rebase the dependency bump and repair its failing checks"
			const branch = "dependabot/npm/x-1.2.3"
			sessions.sessions["sess-1"] = &models.Session{
				ID: "sess-1", RepoID: "repo-1", Title: "Repair dependency bump", Plan: plan,
				BaseBranch: "main", PRNumber: ptr(42), AgentName: "claude", State: machine.CreatingWorktree,
			}
			lc := newTestLifecycle(sessions, repos, chats, nil, wt, runner, tx, provider, zerolog.Nop())
			lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
			lc.SetAgentLogsDir(t.TempDir())

			if err := lc.StartSession(context.Background(), "sess-1", StartSessionOpts{
				Detach: true, ExistingBranch: branch, SkipSetupScript: true,
			}); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			awaitDraftPR(t, lc, "sess-1")
			got := sessions.sessions["sess-1"]
			if got.State != machine.ImplementingPlan || got.AgentSessionID == nil {
				t.Fatalf("session state = %v, agent session = %v; want ImplementingPlan with an agent", got.State, got.AgentSessionID)
			}
			if len(provider.createPRCalls) != 0 {
				t.Errorf("draft PR calls = %d, want 0", len(provider.createPRCalls))
			}
			if got.PRNumber == nil || *got.PRNumber != 42 {
				t.Errorf("PRNumber = %v, want existing PR 42", got.PRNumber)
			}
			if tc.tmuxAvailable {
				if len(runner.started) != 0 || fake.subcommandCount("new-session") != 1 || len(chats.createCalls) != 1 {
					t.Fatalf("headless starts = %d, tmux starts = %d, chats = %d; want 0, 1, 1", len(runner.started), fake.subcommandCount("new-session"), len(chats.createCalls))
				}
				if !got.Detach {
					t.Error("Detach was not persisted for the tmux-hosted run")
				}
				if chats.createCalls[0].AgentSessionID != *got.AgentSessionID {
					t.Error("session agent does not match the started tmux chat")
				}
				var delivered string
				for _, call := range fake.calls {
					if call.subcommand == "load-buffer" && call.loaded != nil {
						delivered = call.loaded.String()
					}
				}
				if delivered != plan || fake.subcommandCount("paste-buffer") != 1 || fake.enterSendKeysCount() != 1 {
					t.Errorf("delivered prompt = %q, pastes = %d, submits = %d; want %q, 1, 1", delivered, fake.subcommandCount("paste-buffer"), fake.enterSendKeysCount(), plan)
				}
			} else {
				if len(runner.started) != 1 || fake.subcommandCount("new-session") != 0 {
					t.Fatalf("headless starts = %d, tmux starts = %d; want 1, 0", len(runner.started), fake.subcommandCount("new-session"))
				}
				if runner.started[0].plan != plan {
					t.Errorf("headless prompt = %q, want %q", runner.started[0].plan, plan)
				}
			}
		})
	}
}
