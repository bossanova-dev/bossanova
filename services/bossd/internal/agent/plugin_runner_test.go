package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeAgentClient implements AgentRunnerClient for tests. Each method
// records the request and returns the configured response/error.
type fakeAgentClient struct {
	startResp *bossanovav1.StartAgentRunResponse
	startErr  error
	startReq  atomic.Pointer[bossanovav1.StartAgentRunRequest]
	stopErr   error
	running   bool
}

func (f *fakeAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}
func (f *fakeAgentClient) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	f.startReq.Store(req)
	return f.startResp, f.startErr
}
func (f *fakeAgentClient) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, f.stopErr
}
func (f *fakeAgentClient) IsRunning(_ context.Context, _ *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{Running: f.running}, nil
}
func (f *fakeAgentClient) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{IsComplete: !f.running, ExitError: ""}, nil
}
func (f *fakeAgentClient) ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: true}, nil
}
func (f *fakeAgentClient) BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{Argv: []string{"sh", "-c", "true"}}, nil
}
func (f *fakeAgentClient) ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: []string{".claude/settings.local.json"}}, nil
}
func (f *fakeAgentClient) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{Supported: true, Title: ""}, nil
}
func (f *fakeAgentClient) HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}
func (f *fakeAgentClient) LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (f *fakeAgentClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

func TestPluginRunner_Start_ResolvesLogPath(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	tl := NewTailer(zerolog.Nop())
	pr := NewPluginRunner(fc, tl, t.TempDir(), zerolog.Nop())

	sid, err := pr.Start(context.Background(), "/work", "plan", nil, "explicit-sid")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "sid" {
		t.Errorf("returned sid = %q, want sid", sid)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.WorkDir != "/work" || got.Plan != "plan" || got.SessionId != "explicit-sid" {
		t.Errorf("unexpected req: %+v", got)
	}
	if got.LogPath == "" {
		t.Error("LogPath empty — pluginRunner must set it")
	}
}

func TestPluginRunner_Start_PropagatesError(t *testing.T) {
	fc := &fakeAgentClient{startErr: errors.New("boom")}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())
	_, err := pr.Start(context.Background(), "/w", "p", nil, "sid")
	if err == nil || !errors.Is(err, fc.startErr) && err.Error() != "boom" {
		t.Errorf("expected wrapped err, got %v", err)
	}
}
