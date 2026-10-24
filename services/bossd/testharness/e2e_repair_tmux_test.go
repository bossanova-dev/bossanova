package testharness_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/testharness"
)

type repairFakeAgentClient struct {
	argv       []string
	promptPath string
}

// newRepairFakeAgentClient returns an agent whose argv is a script that exits
// 42 unless its stdout is a terminal, then writes the first line it reads to
// promptPath.
func newRepairFakeAgentClient(t *testing.T) *repairFakeAgentClient {
	t.Helper()

	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	scriptPath := filepath.Join(dir, "repair-fake-agent.sh")

	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
prompt_path=%q

if [ ! -t 1 ]; then
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
# The bounded sleep keeps the pane alive through the 2s verify budget. Run
# completion is signalled by ExitStatus once the prompt file exists above,
# independent of this sleep.
printf '\n⏺ working\n'
sleep 3
`, promptPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}

	return &repairFakeAgentClient{argv: []string{scriptPath}, promptPath: promptPath}
}

func (f *repairFakeAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "codex", Capabilities: []string{"agent_runner"}}, nil
}

func (f *repairFakeAgentClient) StartAgentRun(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return nil, fmt.Errorf("repair E2E must use StartChatRun, not StartAgentRun")
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

func (f *repairFakeAgentClient) MaterializeAgentAccount(context.Context, *bossanovav1.MaterializeAgentAccountRequest) (*bossanovav1.MaterializeAgentAccountResponse, error) {
	return &bossanovav1.MaterializeAgentAccountResponse{}, nil
}

func setupRepairE2E(t *testing.T, fake agent.RunnerClient) (*testharness.Harness, context.Context, string) {
	t.Helper()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))

	h := testharness.NewWithOptions(t, testharness.Options{
		TmuxCommandFactory: realTmuxFactory(t),
	})
	h.SetAgentClientsForTest(map[string]agent.RunnerClient{"codex": fake})

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

// TestRepairE2E_StartChatRunKeepsTTYAndInjectsPrompt drives a repair chat run
// through a real tmux server. The fake agent exits 42 unless its stdout is a
// terminal, so any wrapping of the agent's stdout between StartChatRun and the
// pane fails the run, and the prompt file proves the prompt reached its stdin.
func TestRepairE2E_StartChatRunKeepsTTYAndInjectsPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := newRepairFakeAgentClient(t)
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

	prompt, err := os.ReadFile(fake.promptPath)
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	if !strings.Contains(string(prompt), "/boss-repair") {
		t.Fatalf("prompt file = %q, want it to contain /boss-repair", prompt)
	}

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
