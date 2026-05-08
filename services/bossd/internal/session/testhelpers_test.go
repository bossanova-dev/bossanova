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
	// against the extracted StartTmuxChat path.
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv: []string{"sh", "-c", "claude --session-id " + req.SessionId + " 2>&1 | tee " + req.LogPath},
	}, nil
}

func (f *fakeAgentForLifecycle) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}

func (f *fakeAgentForLifecycle) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}
