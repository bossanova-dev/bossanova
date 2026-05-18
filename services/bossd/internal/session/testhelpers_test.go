package session

import (
	"context"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

// Compile-time check that fakeAgentForLifecycle satisfies AgentRunnerClient.
var _ agent.AgentRunnerClient = (*fakeAgentForLifecycle)(nil)

// fakeAgentForLifecycle is a no-op fake for agent.AgentRunnerClient.
// Tests that exercise the HookToken path inject this via lc.SetAgent.
// The LastConfigureHookReq field lets callers verify what was passed.
type fakeAgentForLifecycle struct {
	LastConfigureHookReq *bossanovav1.ConfigureFinalizeHookRequest
	IsSupported          bool  // controls ConfigureFinalizeHook response
	ConfigureHookErr     error // when non-nil, ConfigureFinalizeHook returns it
}

func newFakeAgent() *fakeAgentForLifecycle {
	return &fakeAgentForLifecycle{IsSupported: true}
}

func (f *fakeAgentForLifecycle) GetInfo(_ context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}

func (f *fakeAgentForLifecycle) StartRun(_ context.Context, _ *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return &bossanovav1.StartAgentRunResponse{SessionId: "fake"}, nil
}

func (f *fakeAgentForLifecycle) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (f *fakeAgentForLifecycle) IsRunning(_ context.Context, _ *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{}, nil
}

func (f *fakeAgentForLifecycle) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{IsComplete: true}, nil
}

func (f *fakeAgentForLifecycle) ConfigureFinalizeHook(_ context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	f.LastConfigureHookReq = req
	if f.ConfigureHookErr != nil {
		return nil, f.ConfigureHookErr
	}
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: f.IsSupported}, nil
}

func (f *fakeAgentForLifecycle) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	// Mirror the shape of plugins/bossd-plugin-claude's real argv so cron
	// tests that assert on the substring "claude --session-id " still pass
	// against the extracted StartTmuxChat path. The production plugin
	// returns bare argv (no shell wrapper) so tmux can spawn claude
	// directly on a PTY; capture-to-disk is handled by `tmux pipe-pane`
	// after the spawn, not by piping stdout through tee.
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv: []string{"claude", "--session-id", req.SessionId},
	}, nil
}

func (f *fakeAgentForLifecycle) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: req.GetRequestedSessionId() != "", SessionId: req.GetRequestedSessionId()}, nil
}

func (f *fakeAgentForLifecycle) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}

func (f *fakeAgentForLifecycle) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}

func (f *fakeAgentForLifecycle) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *fakeAgentForLifecycle) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}

func (f *fakeAgentForLifecycle) TranscriptExists(_ context.Context, _ *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

// fakePollArmer records calls to Arm so tests can assert that the poll
// fallback was (or was not) wired for a given agent_session_id.
type fakePollArmer struct {
	armCalled bool
	armedID   string
}

func (f *fakePollArmer) Arm(_ context.Context, agentSessionID string, _ agent.AgentRunnerClient) {
	f.armCalled = true
	f.armedID = agentSessionID
}
