package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

// failingStartChatLifecycle writes what a real StartTmuxChat leaves behind when
// the first input is not delivered: a chat row under the run's id, stamped
// start-failed, and an untyped error.
type failingStartChatLifecycle struct {
	chats   db.AgentChatStore
	started []string
}

func (lc *failingStartChatLifecycle) StartTmuxChat(ctx context.Context, sessionID string, input session.ChatInput, title string, hookOpts session.HookOpts) (string, error) {
	id := hookOpts.AgentSessionID
	lc.started = append(lc.started, id)
	if _, err := lc.chats.Create(ctx, db.CreateAgentChatParams{SessionID: sessionID, AgentSessionID: id, AgentName: "claude", Title: title, Role: input.Role}); err != nil {
		return "", err
	}
	reason := fmt.Sprintf("send command failed: send command to tmux session %q: command was not submitted", "boss-"+id)
	if err := lc.chats.MarkStartFailed(ctx, id, reason); err != nil {
		return "", err
	}
	return "", errors.New(reason)
}

func (lc *failingStartChatLifecycle) ReplaceBlockingChatForRepair(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
	return session.ReclaimRepairChatResult{}, nil
}

func (lc *failingStartChatLifecycle) InServiceSpawnTarget(_ context.Context, sess *models.Session) (*models.Session, *domain.AgentChat, error) {
	return sess, nil, nil
}

func (lc *failingStartChatLifecycle) ReclaimRepairChat(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
	return session.ReclaimRepairChatResult{}, nil
}

func TestStartChatRun_RepeatedRepairStartFailuresLeaveOneFailedChat(t *testing.T) {
	ctx := t.Context()
	sqlDB := openTestDB(t)
	chats := db.NewAgentChatStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	repos := db.NewRepoStore(sqlDB)
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repair-failed-start.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "dependabot bump",
		WorktreePath: "/tmp/worktrees/dependabot-bump",
		BranchName:   "dependabot-bump",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	failedChat := func(id string, role domain.AgentChatRole) {
		t.Helper()
		if _, err := chats.Create(ctx, db.CreateAgentChatParams{SessionID: sess.ID, AgentSessionID: id, AgentName: "claude", Title: id, Role: role}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := chats.MarkStartFailed(ctx, id, "never came up"); err != nil {
			t.Fatalf("mark %s failed: %v", id, err)
		}
	}
	runs := db.NewAgentRunStore(sqlDB)
	failedChat("operator-failed", domain.AgentChatRoleOperator)
	failedChat("repair-reopened", domain.AgentChatRoleRepair)
	reopenedPane := "boss-repair-reopened"
	if err := chats.UpdateTmuxSessionName(ctx, "repair-reopened", &reopenedPane); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	failedChat("repair-ran-then-resume-failed", domain.AgentChatRoleRepair)
	if _, err := runs.Create(ctx, domain.AgentRun{SessionID: sess.ID, AgentSessionID: "repair-ran-then-resume-failed", AgentName: "claude", StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	srv := newTestHostServiceServer(&mockVCSProvider{})
	srv.SetSessionDeps(repos, sessions, chats, status.NewDisplayTracker(), status.NewTracker())
	srv.SetAgentClients(map[string]agent.RunnerClient{"claude": newFakeAgentClient()})
	srv.SetAgentLogsDir(t.TempDir())
	srv.SetAgentRunStore(runs)
	lc := &failingStartChatLifecycle{chats: chats}
	srv.SetLifecycle(lc)

	for range 3 {
		if _, err := srv.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
			SessionId: sess.ID,
			Command:   "boss-repair",
			Title:     "Repair: dependabot bump",
			Role:      bossanovav1.ChatRole_CHAT_ROLE_REPAIR,
		}); err == nil {
			t.Fatal("StartChatRun: want the start failure")
		}
	}

	listed, err := chats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	var remaining []string
	for _, chat := range listed {
		remaining = append(remaining, chat.AgentSessionID)
	}
	want := []string{lc.started[len(lc.started)-1], "operator-failed", "repair-ran-then-resume-failed", "repair-reopened"}
	slices.Sort(remaining)
	slices.Sort(want)
	if !slices.Equal(remaining, want) {
		t.Fatalf("remaining chats = %v, want %v: only the newest of the 3 failed starts, plus the operator chat, the repair chat that ran and the reopened one", remaining, want)
	}
}
