package session

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

// TestBootstrapDoesNotReArmArchivedSession pins BOS-2228: an archived
// session's worktree is gone, and a hook write into it would re-create the
// directory to hold a lone settings.local.json. A chat row that still names a
// tmux session after the archive must therefore not reach ConfigureFinalizeHook,
// while a live sibling still does.
func TestBootstrapDoesNotReArmArchivedSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	archivedAt := time.Now().UTC()
	liveTok, archivedTok := "tok-live", "tok-archived"
	sessions.sessions["sess-live"] = &models.Session{
		ID: "sess-live", RepoID: "repo-1", WorktreePath: "/tmp/wt-live", BaseBranch: "main",
		State: machine.ImplementingPlan, AgentName: "claude", HookToken: &liveTok,
	}
	sessions.sessions["sess-archived"] = &models.Session{
		ID: "sess-archived", RepoID: "repo-1", WorktreePath: "/tmp/wt-archived", BaseBranch: "main",
		State: machine.ImplementingPlan, AgentName: "claude", HookToken: &archivedTok, ArchivedAt: &archivedAt,
	}
	liveTmux, archivedTmux := "tmux-live", "tmux-archived"
	chats := &mockAgentChatStore{chatsWithTmux: []*domain.AgentChat{
		{ID: "chat-live", SessionID: "sess-live", AgentSessionID: "run-live", AgentName: "claude", TmuxSessionName: &liveTmux},
		{ID: "chat-archived", SessionID: "sess-archived", AgentSessionID: "run-archived", AgentName: "claude", TmuxSessionName: &archivedTmux},
	}}

	fa := newFakeAgent()
	lc := newTestLifecycle(sessions, newMockRepoStore(), chats, &stubCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.RunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	for _, req := range fa.ConfigureHookReqs {
		if req.GetSessionId() == "sess-archived" || req.GetWorkDir() == "/tmp/wt-archived" {
			t.Fatalf("Bootstrap re-armed the archived session's hook: %+v", req)
		}
	}
	if len(fa.ConfigureHookReqs) != 1 || fa.ConfigureHookReqs[0].GetSessionId() != "sess-live" {
		t.Fatalf("ConfigureFinalizeHook calls = %+v, want exactly one for sess-live", fa.ConfigureHookReqs)
	}
}
