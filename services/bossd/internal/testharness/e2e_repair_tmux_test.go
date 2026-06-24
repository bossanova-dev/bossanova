//go:build integration

package testharness_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/tmux"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type repairFakeAgentClient struct {
	argv       []string
	promptPath string
}

func newRepairFakeAgentClient(t *testing.T, mode string) (*repairFakeAgentClient, string, string) {
	t.Helper()

	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	scriptPath := filepath.Join(dir, "repair-fake-agent.sh")

	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
mode=%q
prompt_path=%q

if [ "$mode" = "require-tty" ] && [ ! -t 1 ]; then
  printf 'stdout is not a terminal\n' >&2
  exit 42
fi

printf '❯'

IFS= read -r data || true
printf '%%s\n' "$data" > "$prompt_path"
`, mode, promptPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}

	return &repairFakeAgentClient{argv: []string{scriptPath}, promptPath: promptPath}, promptPath, scriptPath
}

func (f *repairFakeAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "codex", Capabilities: []string{"agent_runner"}}, nil
}

func (f *repairFakeAgentClient) StartRun(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return nil, fmt.Errorf("repair E2E must use StartChatRun, not StartRun")
}

func (f *repairFakeAgentClient) StopRun(context.Context, *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (f *repairFakeAgentClient) IsRunning(context.Context, *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{Running: false}, nil
}

func (f *repairFakeAgentClient) ExitStatus(context.Context, *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	if _, err := os.Stat(f.promptPath); err != nil {
		if os.IsNotExist(err) {
			return &bossanovav1.AgentExitStatusResponse{IsComplete: false}, nil
		}
		return nil, err
	}
	return &bossanovav1.AgentExitStatusResponse{IsComplete: true}, nil
}

func (f *repairFakeAgentClient) ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

func (f *repairFakeAgentClient) RemoveAgentRunHook(context.Context, *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}

func (f *repairFakeAgentClient) BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{Argv: append([]string(nil), f.argv...)}, nil
}

func (f *repairFakeAgentClient) ResolveInteractiveSessionID(context.Context, *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
}

func (f *repairFakeAgentClient) ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}

func (f *repairFakeAgentClient) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{Supported: true, Title: "Repair: fake"}, nil
}

func (f *repairFakeAgentClient) HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *repairFakeAgentClient) LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}

func (f *repairFakeAgentClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

func setupRepairE2E(t *testing.T, fake agent.AgentRunnerClient) (*testharness.Harness, context.Context, string) {
	t.Helper()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))

	h := testharness.NewWithOptions(t, testharness.Options{
		TmuxCommandFactory: exec.CommandContext,
	})
	h.SetAgentClientsForTest(map[string]agent.AgentRunnerClient{"codex": fake})

	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "repair-e2e",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   filepath.Join(t.TempDir(), "worktrees"),
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	agentName := "codex"
	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId:    repoResp.Msg.Repo.Id,
		Title:     "Repair E2E",
		Plan:      "test repair e2e",
		AgentName: &agentName,
	})

	return h, ctx, sess.Id
}

func writeRepairE2ELogAt(t *testing.T, h *testharness.Harness, agentSessionID string, modTime time.Time) {
	t.Helper()
	logPath := filepath.Join(h.AgentLogsDir, agentSessionID+".log")
	if err := os.WriteFile(logPath, []byte("repair output\n"), 0o600); err != nil {
		t.Fatalf("write repair log: %v", err)
	}
	if err := os.Chtimes(logPath, modTime, modTime); err != nil {
		t.Fatalf("set repair log time: %v", err)
	}
}

func readPromptFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	return string(data)
}

func requireContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected %q to contain %q", got, want)
	}
}

func TestRepairE2E_StartChatRunKeepsTTYAndInjectsPrompt(t *testing.T) {
	fake, promptPath, _ := newRepairFakeAgentClient(t, "require-tty")
	h, ctx, sessionID := setupRepairE2E(t, fake)

	startResp, err := h.HostService.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sessionID,
		Prompt:    "/boss-repair",
		Title:     "Repair: Repair E2E",
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if startResp.GetAgentSessionId() == "" {
		t.Fatalf("StartChatRun returned empty agent session id")
	}

	waitResp, err := h.HostService.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId: startResp.GetAgentSessionId(),
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if waitResp.GetExitError() != "" {
		t.Fatalf("WaitChatRun exit error = %q", waitResp.GetExitError())
	}

	prompt := readPromptFile(t, promptPath)
	requireContains(t, prompt, "/boss-repair")

	chat, err := h.AgentChats.GetByAgentSessionID(ctx, startResp.GetAgentSessionId())
	if err != nil {
		t.Fatalf("agent chat row: %v", err)
	}
	if chat.Title != "Repair: Repair E2E" {
		t.Fatalf("chat title = %q", chat.Title)
	}
	if chat.StartError != nil && *chat.StartError != "" {
		t.Fatalf("chat start_error = %q", *chat.StartError)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
		t.Fatalf("chat tmux session name not recorded")
	}
}

type wrappedRepairFakeAgentClient struct {
	inner *repairFakeAgentClient
	log   string
}

func (f *wrappedRepairFakeAgentClient) BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	resp, err := f.inner.BuildInteractiveCommand(ctx, req)
	if err != nil {
		return nil, err
	}
	script := strings.Join(resp.GetArgv(), " ") + " 2>&1 | tee " + f.log
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv: []string{"bash", "-c", "set -o pipefail; " + script},
	}, nil
}

func (f *wrappedRepairFakeAgentClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	return f.inner.GetInfo(ctx)
}

func (f *wrappedRepairFakeAgentClient) StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return f.inner.StartRun(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) StopRun(ctx context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return f.inner.StopRun(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) IsRunning(ctx context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return f.inner.IsRunning(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) ExitStatus(ctx context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return f.inner.ExitStatus(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) ConfigureFinalizeHook(ctx context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return f.inner.ConfigureFinalizeHook(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) RemoveAgentRunHook(ctx context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return f.inner.RemoveAgentRunHook(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) ResolveInteractiveSessionID(ctx context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return f.inner.ResolveInteractiveSessionID(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) ListIgnoredDirtyFiles(ctx context.Context, req *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return f.inner.ListIgnoredDirtyFiles(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return f.inner.GetChatTitle(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return f.inner.HasQuestionPrompt(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return f.inner.LastTurnIsUser(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return f.inner.TranscriptExists(ctx, req)
}

func TestRepairE2E_TeeWrapperFailsBeforePromptAndRecordsStartError(t *testing.T) {
	inner, _, _ := newRepairFakeAgentClient(t, "require-tty")
	fake := &wrappedRepairFakeAgentClient{
		inner: inner,
		log:   filepath.Join(t.TempDir(), "wrapped.log"),
	}
	h, ctx, sessionID := setupRepairE2E(t, fake)

	startResp, err := h.HostService.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sessionID,
		Prompt:    "/boss-repair",
		Title:     "Repair: wrapped failure",
	})
	if err == nil {
		t.Fatalf("StartChatRun unexpectedly succeeded")
	}
	if startResp != nil && startResp.GetAgentSessionId() != "" {
		t.Fatalf("StartChatRun returned agent session id on failed start: %q", startResp.GetAgentSessionId())
	}

	chats, listErr := h.AgentChats.ListBySession(ctx, sessionID)
	if listErr != nil {
		t.Fatalf("list chats: %v", listErr)
	}
	if len(chats) != 1 {
		t.Fatalf("chat count = %d, want 1", len(chats))
	}
	if chats[0].StartError == nil || !strings.Contains(*chats[0].StartError, "ready marker") {
		t.Fatalf("start_error = %v, want ready marker failure", chats[0].StartError)
	}
}

func TestRepairPluginReclaimsStaleRepairChatAfterRestart(t *testing.T) {
	fake, _, _ := newRepairFakeAgentClient(t, "require-tty")
	h, ctx, sessionID := setupRepairE2E(t, fake)

	staleAgentSessionID := "stale-repair-agent"
	staleTmuxName := "boss-test-stale-repair"
	if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: staleAgentSessionID,
		AgentName:      "codex",
		Title:          "Repair: stale rejected session",
	}); err != nil {
		t.Fatalf("seed stale repair chat: %v", err)
	}
	if err := h.AgentChats.UpdateTmuxSessionName(ctx, staleAgentSessionID, &staleTmuxName); err != nil {
		t.Fatalf("seed stale tmux name: %v", err)
	}
	if err := h.Tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    staleTmuxName,
		WorkDir: t.TempDir(),
		Command: []string{"sleep", "30"},
	}); err != nil {
		t.Fatalf("seed stale tmux session: %v", err)
	}
	t.Cleanup(func() { _ = h.Tmux.KillSession(context.Background(), staleTmuxName) })
	writeRepairE2ELogAt(t, h, staleAgentSessionID, time.Now().Add(-(31 * time.Minute)))

	_, err := h.HostService.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sessionID,
		Command:   "boss-repair",
		Title:     "Repair: Repair E2E",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("first StartChatRun error = %v, want AlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "agent_session_id="+staleAgentSessionID) {
		t.Fatalf("AlreadyExists error = %q, want stale agent_session_id", err.Error())
	}

	reclaimResp, err := h.HostService.ReclaimRepairChat(ctx, &bossanovav1.ReclaimRepairChatHostRequest{
		SessionId:      sessionID,
		AgentSessionId: staleAgentSessionID,
		Reason:         "reclaimed stale repair chat after daemon restart",
	})
	if err != nil {
		t.Fatalf("ReclaimRepairChat: %v", err)
	}
	if !reclaimResp.GetReclaimed() {
		t.Fatal("expected reclaimed stale repair chat")
	}
	if h.Tmux.HasSession(ctx, staleTmuxName) {
		t.Fatalf("stale tmux session %q still live", staleTmuxName)
	}

	staleChat, err := h.AgentChats.GetByAgentSessionID(ctx, staleAgentSessionID)
	if err != nil {
		t.Fatalf("get stale chat: %v", err)
	}
	if staleChat.TmuxSessionName != nil {
		t.Fatalf("stale chat tmux name = %q, want nil", *staleChat.TmuxSessionName)
	}
	if staleChat.StartError == nil || !strings.Contains(*staleChat.StartError, "reclaimed stale repair chat") {
		t.Fatalf("stale chat start_error = %v, want reclaim reason", staleChat.StartError)
	}

	freshResp, err := h.HostService.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sessionID,
		Command:   "boss-repair",
		Title:     "Repair: Repair E2E",
	})
	if err != nil {
		t.Fatalf("fresh StartChatRun: %v", err)
	}
	if freshResp.GetAgentSessionId() == "" || freshResp.GetAgentSessionId() == staleAgentSessionID {
		t.Fatalf("fresh agent session id = %q", freshResp.GetAgentSessionId())
	}
	freshChat, err := h.AgentChats.GetByAgentSessionID(ctx, freshResp.GetAgentSessionId())
	if err != nil {
		t.Fatalf("get fresh chat: %v", err)
	}
	if !strings.HasPrefix(freshChat.Title, "Repair:") {
		t.Fatalf("fresh chat title = %q, want Repair prefix", freshChat.Title)
	}
	if freshChat.TmuxSessionName == nil || *freshChat.TmuxSessionName == "" {
		t.Fatal("fresh chat tmux session name not recorded")
	}
}

func TestRepairPluginDoesNotReclaimRecentLiveRepairChat(t *testing.T) {
	fake, _, _ := newRepairFakeAgentClient(t, "require-tty")
	h, ctx, sessionID := setupRepairE2E(t, fake)

	activeAgentSessionID := "active-repair-agent"
	activeTmuxName := "boss-test-active-repair"
	if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: activeAgentSessionID,
		AgentName:      "codex",
		Title:          "Repair: active rejected session",
	}); err != nil {
		t.Fatalf("seed active repair chat: %v", err)
	}
	if err := h.AgentChats.UpdateTmuxSessionName(ctx, activeAgentSessionID, &activeTmuxName); err != nil {
		t.Fatalf("seed active tmux name: %v", err)
	}
	if err := h.Tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    activeTmuxName,
		WorkDir: t.TempDir(),
		Command: []string{"sleep", "30"},
	}); err != nil {
		t.Fatalf("seed active tmux session: %v", err)
	}
	t.Cleanup(func() { _ = h.Tmux.KillSession(context.Background(), activeTmuxName) })
	writeRepairE2ELogAt(t, h, activeAgentSessionID, time.Now())

	_, err := h.HostService.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sessionID,
		Command:   "boss-repair",
		Title:     "Repair: Repair E2E",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("first StartChatRun error = %v, want AlreadyExists", err)
	}

	_, err = h.HostService.ReclaimRepairChat(ctx, &bossanovav1.ReclaimRepairChatHostRequest{
		SessionId:      sessionID,
		AgentSessionId: activeAgentSessionID,
		Reason:         "must not reclaim active repair chat",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ReclaimRepairChat code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
	if !h.Tmux.HasSession(ctx, activeTmuxName) {
		t.Fatalf("active tmux session %q was killed", activeTmuxName)
	}
	activeChat, err := h.AgentChats.GetByAgentSessionID(ctx, activeAgentSessionID)
	if err != nil {
		t.Fatalf("get active chat: %v", err)
	}
	if activeChat.TmuxSessionName == nil || *activeChat.TmuxSessionName != activeTmuxName {
		t.Fatalf("active chat tmux name = %v, want %q", activeChat.TmuxSessionName, activeTmuxName)
	}
	if activeChat.StartError != nil {
		t.Fatalf("active chat start_error = %q, want nil", *activeChat.StartError)
	}
}
