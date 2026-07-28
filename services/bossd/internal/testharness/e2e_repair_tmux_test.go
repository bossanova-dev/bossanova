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
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/tmux"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
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

# Render an agent-activity marker (⏺) below the input box so the host's
# submit-verifier (tmux waitForSubmission) sees the line leave the prompt and
# confirms submission — a real agent shows activity the instant it accepts a
# line; without this the fake would exit at the prompt and capture-pane would
# race the pane's teardown ("verify command submission: ... exit status 1").
# The bounded sleep keeps the pane alive through the 2s verify budget, then
# self-cleans (a fresh repair chat's dynamic tmux name is not covered by any
# t.Cleanup). Run completion is signalled by ExitStatus once the prompt file
# exists above, independent of this sleep.
printf '\n⏺ working\n'
sleep 3
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

func (f *repairFakeAgentClient) SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *repairFakeAgentClient) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{Supported: true, Title: "Repair: fake"}, nil
}

func (f *repairFakeAgentClient) HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *repairFakeAgentClient) DetectUsageLimit(context.Context, *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return &bossanovav1.DetectUsageLimitResponse{}, nil
}

func (f *repairFakeAgentClient) ProbeRateLimit(context.Context, *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	return &bossanovav1.ProbeRateLimitResponse{}, nil
}

func (f *repairFakeAgentClient) HasWorkingIndicator(context.Context, *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{}, nil
}

func (f *repairFakeAgentClient) LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}

func (f *repairFakeAgentClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

func (f *repairFakeAgentClient) ReadTranscript(context.Context, *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return &bossanovav1.ReadTranscriptResponse{}, nil
}

func (f *repairFakeAgentClient) RotationCapability(context.Context, *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return &bossanovav1.RotationCapabilityResponse{}, nil
}

func (f *repairFakeAgentClient) MaterializeAccount(context.Context, *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{}, nil
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
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&bossanovav1.RegisterRepoRequest{
		DisplayName:       "repair-e2e",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   filepath.Join(t.TempDir(), "worktrees"),
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	agentName := "codex"
	sess := createSessionFromStream(t, h, ctx, &bossanovav1.CreateSessionRequest{
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

func (f *wrappedRepairFakeAgentClient) SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *wrappedRepairFakeAgentClient) GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return f.inner.GetChatTitle(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return f.inner.HasQuestionPrompt(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) DetectUsageLimit(ctx context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return f.inner.DetectUsageLimit(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) ProbeRateLimit(ctx context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	return f.inner.ProbeRateLimit(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) HasWorkingIndicator(ctx context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return f.inner.HasWorkingIndicator(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return f.inner.LastTurnIsUser(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return f.inner.TranscriptExists(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) ReadTranscript(ctx context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return f.inner.ReadTranscript(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) RotationCapability(ctx context.Context, req *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return f.inner.RotationCapability(ctx, req)
}

func (f *wrappedRepairFakeAgentClient) MaterializeAccount(ctx context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return f.inner.MaterializeAccount(ctx, req)
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
	// BOS-153: displaceability now comes from the chat tracker, not the pane-log
	// mtime. Report the stale repair chat idle 31m (matching the log above) so it
	// clears repairDisplaceMinIdle (5m) and the reclaim gate lets it through.
	h.SeedChatStatus(staleAgentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now().Add(-(31 * time.Minute)))

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
	// BOS-153: the tracker is the displaceability authority. Report the repair
	// chat as just-active (idle ~0, matching the fresh log above) so it stays
	// under repairDisplaceMinIdle (5m) and the reclaim gate refuses it.
	h.SeedChatStatus(activeAgentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now())

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

// TestRepairDisplacesQuestionStuckNonRepairChat is the BOS-347 overnight-incident
// recovery proof, end to end. A rejected/failing session whose only live chat is
// a NON-repair chat parked at an unanswered QUESTION must never be permanently
// blocked from auto-repair: once that chat has been silent past the 15m question
// window, the repair replace path (StartChatRun with ReplaceExistingChat, via the
// StartTmuxChat AlreadyExists route) displaces it and starts a repair chat — with
// NO last_repair_blocked_reason accumulating, which was the overnight failure
// signature (the daemon refused every sweep and the plugin re-recorded the block).
// The under-window twin proves the window is real: the same chat idle only 5m is
// still refused with "is at a question", and that refusal is recorded.
func TestRepairDisplacesQuestionStuckNonRepairChat(t *testing.T) {
	// arrange builds a repairable session whose sole live chat is a non-repair
	// chat parked at a QUESTION whose last visible output was at lastOutputAt.
	arrange := func(t *testing.T, lastOutputAt time.Time) (*testharness.Harness, context.Context, string, string, string) {
		fake, _, _ := newRepairFakeAgentClient(t, "require-tty")
		h, ctx, sessionID := setupRepairE2E(t, fake)

		// Repairable session: a rejected PR display status. This is narrative
		// fidelity — the daemon replace gate is display-status-agnostic; the
		// repair plugin (not exercised here) is what consults display status to
		// decide whether to attempt a repair at all.
		h.DisplayTracker.Set(sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusRejected})

		blockingAgentSessionID := "question-stuck-nonrepair-agent"
		blockingTmuxName := "boss-test-question-nonrepair"
		if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
			SessionID:      sessionID,
			AgentSessionID: blockingAgentSessionID,
			AgentName:      "codex",
			Title:          "Investigate flaky integration test", // deliberately NOT a "Repair:" title
		}); err != nil {
			t.Fatalf("seed blocking chat: %v", err)
		}
		if err := h.AgentChats.UpdateTmuxSessionName(ctx, blockingAgentSessionID, &blockingTmuxName); err != nil {
			t.Fatalf("seed blocking tmux name: %v", err)
		}
		if err := h.Tmux.NewSession(ctx, tmux.NewSessionOpts{
			Name:    blockingTmuxName,
			WorkDir: t.TempDir(),
			Command: []string{"sleep", "30"},
		}); err != nil {
			t.Fatalf("seed blocking tmux session: %v", err)
		}
		t.Cleanup(func() { _ = h.Tmux.KillSession(context.Background(), blockingTmuxName) })

		// The chat tracker is the sole displaceability authority (BOS-153): the
		// blocking chat is at a QUESTION with its last visible output at
		// lastOutputAt.
		h.SeedChatStatus(blockingAgentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_QUESTION, lastOutputAt)

		return h, ctx, sessionID, blockingAgentSessionID, blockingTmuxName
	}

	// startRepairReplace issues the repair plugin's replacement StartChatRun. The
	// observed snapshot equals the blocking chat's last output — the plugin saw no
	// output since — so the observed-snapshot guard passes and only the question
	// window decides displaceability.
	startRepairReplace := func(ctx context.Context, h *testharness.Harness, sessionID string, observed time.Time) (*bossanovav1.StartChatRunHostResponse, error) {
		return h.HostService.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
			SessionId:             sessionID,
			Command:               "boss-repair",
			Title:                 "Repair: Repair E2E",
			ReplaceExistingChat:   true,
			ReplaceExistingReason: "auto-repair displacing question-stuck chat",
			ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
		})
	}

	t.Run("displaces_question_stuck_chat_past_window_and_repairs", func(t *testing.T) {
		lastOutputAt := time.Now().Add(-20 * time.Minute)
		h, ctx, sessionID, blockingAgentSessionID, blockingTmuxName := arrange(t, lastOutputAt)

		resp, err := startRepairReplace(ctx, h, sessionID, lastOutputAt)
		if err != nil {
			t.Fatalf("StartChatRun (replace) unexpected error: %v", err)
		}
		freshID := resp.GetAgentSessionId()
		if freshID == "" || freshID == blockingAgentSessionID {
			t.Fatalf("fresh repair agent session id = %q (blocking = %q)", freshID, blockingAgentSessionID)
		}

		// The blocking chat's pane was displaced and its row cleared.
		if h.Tmux.HasSession(ctx, blockingTmuxName) {
			t.Fatalf("blocking tmux session %q still live; expected it displaced", blockingTmuxName)
		}
		blockingChat, err := h.AgentChats.GetByAgentSessionID(ctx, blockingAgentSessionID)
		if err != nil {
			t.Fatalf("get blocking chat: %v", err)
		}
		if blockingChat.TmuxSessionName != nil {
			t.Fatalf("blocking chat tmux name = %q, want nil after displace", *blockingChat.TmuxSessionName)
		}

		// A repair chat took over.
		freshChat, err := h.AgentChats.GetByAgentSessionID(ctx, freshID)
		if err != nil {
			t.Fatalf("get fresh repair chat: %v", err)
		}
		if !strings.HasPrefix(freshChat.Title, "Repair:") {
			t.Fatalf("fresh chat title = %q, want Repair prefix", freshChat.Title)
		}
		if freshChat.TmuxSessionName == nil || *freshChat.TmuxSessionName == "" {
			t.Fatal("fresh repair chat tmux session name not recorded")
		}

		// The overnight failure signature — last_repair_blocked_reason re-recorded
		// every sweep — must be absent: the daemon accepted the displacement, so a
		// repair sweep proceeds to run repair rather than record a block.
		sess, err := h.Sessions.Get(ctx, sessionID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if sess.LastRepairBlockedReason != "" || sess.LastRepairBlockedAt != nil {
			t.Fatalf("blocked fields recorded on a successful displace: reason=%q at=%v",
				sess.LastRepairBlockedReason, sess.LastRepairBlockedAt)
		}
	})

	t.Run("refuses_question_stuck_chat_under_window_and_records_block", func(t *testing.T) {
		lastOutputAt := time.Now().Add(-5 * time.Minute)
		h, ctx, sessionID, _, blockingTmuxName := arrange(t, lastOutputAt)

		_, err := startRepairReplace(ctx, h, sessionID, lastOutputAt)
		if grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Fatalf("StartChatRun (replace) code = %v, want FailedPrecondition (err=%v)", grpcstatus.Code(err), err)
		}
		if !strings.Contains(err.Error(), "is at a question") {
			t.Fatalf("refusal = %q, want it to contain 'is at a question'", err.Error())
		}

		// Under the window the blocking chat's pane is untouched — someone may
		// still answer the question.
		if !h.Tmux.HasSession(ctx, blockingTmuxName) {
			t.Fatalf("blocking tmux session %q was displaced inside the 15m window", blockingTmuxName)
		}

		// Model the repair plugin's blocked-lane bookkeeping: on a
		// FailedPrecondition refusal it records the reason via RecordRepairOutcome.
		// This is exactly the accumulation the fix eliminates once past the window.
		if _, recErr := h.HostService.RecordRepairOutcome(ctx, &bossanovav1.RecordRepairOutcomeRequest{
			SessionId:     sessionID,
			BlockedReason: err.Error(),
		}); recErr != nil {
			t.Fatalf("RecordRepairOutcome (blocked lane): %v", recErr)
		}
		sess, getErr := h.Sessions.Get(ctx, sessionID)
		if getErr != nil {
			t.Fatalf("get session: %v", getErr)
		}
		if sess.LastRepairBlockedReason == "" || sess.LastRepairBlockedAt == nil {
			t.Fatalf("blocked reason not recorded under window: reason=%q at=%v",
				sess.LastRepairBlockedReason, sess.LastRepairBlockedAt)
		}
		if !strings.Contains(sess.LastRepairBlockedReason, "is at a question") {
			t.Fatalf("recorded blocked reason = %q, want it to contain 'is at a question'", sess.LastRepairBlockedReason)
		}
	})
}
