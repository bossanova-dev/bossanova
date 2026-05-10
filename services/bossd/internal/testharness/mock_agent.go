package testharness

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

var _ agent.AgentRunnerClient = (*MockAgentClient)(nil)

// MockAgentClient is a no-op fake for agent.AgentRunnerClient used by the
// testharness to satisfy Lifecycle.SetAgent. The default zero-value is the
// "claude" mock; set Name to swap shapes (e.g. "codex" returns a `codex …`
// argv so end-to-end RecordChat tests can assert agent-name routing).
// ConfigureFinalizeHook returns IsSupported=true so the Stop-hook code
// path proceeds the same way it would with a real plugin.
type MockAgentClient struct {
	// Name selects the argv shape returned by BuildInteractiveCommand.
	// Empty defaults to "claude" — preserves behavior for tests that
	// pre-date the per-agent routing.
	Name string
}

func (m *MockAgentClient) name() string {
	if m == nil || m.Name == "" {
		return "claude"
	}
	return m.Name
}

func (m *MockAgentClient) GetInfo(_ context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: m.name(), Version: "test"}, nil
}

func (*MockAgentClient) StartRun(_ context.Context, _ *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return &bossanovav1.StartAgentRunResponse{SessionId: "fake"}, nil
}

func (*MockAgentClient) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (*MockAgentClient) IsRunning(_ context.Context, _ *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{}, nil
}

func (*MockAgentClient) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{IsComplete: true}, nil
}

func (*MockAgentClient) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: true}, nil
}

func (m *MockAgentClient) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	// Mirror the shape each real plugin produces so tests catch wrapping
	// regressions like the empty-LogPath `tee ''` bug. We deliberately
	// route through agentruntime.LogTeeArgv (the same call site real
	// plugins use) instead of conditionally hand-rolling the argv — the
	// LogTeeArgv contract is the bit that broke last time, and exercising
	// it here means the existing e2e RecordChat coverage acts as a
	// regression net for any future change to that helper.
	switch m.name() {
	case "codex":
		// Mirrors plugins/bossd-plugin-codex: positional `resume` subcommand.
		args := []string{"codex"}
		if req.Resume {
			args = append(args, "resume", req.SessionId)
		}
		return &bossanovav1.BuildInteractiveCommandResponse{
			Argv: agentruntime.LogTeeArgv(args, req.LogPath),
		}, nil
	default:
		// Mirrors plugins/bossd-plugin-claude: --resume / --session-id flag.
		flag := "--session-id"
		if req.Resume {
			flag = "--resume"
		}
		args := []string{"claude", flag, req.SessionId}
		return &bossanovav1.BuildInteractiveCommandResponse{
			Argv: agentruntime.LogTeeArgv(args, req.LogPath),
		}, nil
	}
}

func (*MockAgentClient) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}

func (*MockAgentClient) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}

func (*MockAgentClient) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (*MockAgentClient) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}

// TranscriptExists checks the conventional Claude transcript path on disk so
// e2e tests that materialise a JSONL fixture under $HOME/.claude/projects/...
// continue to drive the spawnChatTmux resume branch the same way they did
// when the daemon owned its own status.TranscriptExists helper.
func (*MockAgentClient) TranscriptExists(_ context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return &bossanovav1.TranscriptExistsResponse{}, nil
	}
	key := strings.NewReplacer("/", "-", ".", "-").Replace(req.GetWorkDir())
	path := filepath.Join(home, ".claude", "projects", key, req.GetAgentSessionId()+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return &bossanovav1.TranscriptExistsResponse{}, nil
	}
	return &bossanovav1.TranscriptExistsResponse{Exists: !info.IsDir() && info.Size() > 0}, nil
}
