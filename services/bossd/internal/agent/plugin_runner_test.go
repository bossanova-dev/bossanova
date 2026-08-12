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
	startResp    *bossanovav1.StartAgentRunResponse
	startErr     error
	startReq     atomic.Pointer[bossanovav1.StartAgentRunRequest]
	preflightErr error
	preflightReq atomic.Pointer[bossanovav1.PreflightHeadlessRunRequest]
	stopErr      error
	running      bool
}

func (f *fakeAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}
func (f *fakeAgentClient) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	f.startReq.Store(req)
	return f.startResp, f.startErr
}
func (f *fakeAgentClient) PreflightHeadlessRun(_ context.Context, req *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error) {
	f.preflightReq.Store(req)
	return &bossanovav1.PreflightHeadlessRunResponse{}, f.preflightErr
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
func (f *fakeAgentClient) RemoveAgentRunHook(context.Context, *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}
func (f *fakeAgentClient) BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{Argv: []string{"sh", "-c", "true"}}, nil
}
func (f *fakeAgentClient) ResolveInteractiveSessionID(context.Context, *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
}
func (f *fakeAgentClient) ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: []string{".claude/settings.local.json"}}, nil
}
func (f *fakeAgentClient) SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *fakeAgentClient) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{Supported: true, Title: ""}, nil
}
func (f *fakeAgentClient) HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *fakeAgentClient) DetectUsageLimit(context.Context, *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return &bossanovav1.DetectUsageLimitResponse{}, nil
}

func (f *fakeAgentClient) ProbeRateLimit(context.Context, *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	return &bossanovav1.ProbeRateLimitResponse{}, nil
}

func (f *fakeAgentClient) HasWorkingIndicator(context.Context, *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{}, nil
}
func (f *fakeAgentClient) LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (f *fakeAgentClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}
func (f *fakeAgentClient) ReadTranscript(context.Context, *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return &bossanovav1.ReadTranscriptResponse{}, nil
}
func (f *fakeAgentClient) RotationCapability(context.Context, *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return &bossanovav1.RotationCapabilityResponse{}, nil
}
func (f *fakeAgentClient) MaterializeAccount(context.Context, *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{}, nil
}

func TestPluginRunner_Start_ResolvesLogPath(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	tl := NewTailer(zerolog.Nop())
	pr := NewPluginRunner(fc, tl, t.TempDir(), zerolog.Nop())

	sid, err := pr.Start(context.Background(), "/work", "plan", nil, "explicit-sid", "", nil)
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

func TestPluginRunner_Start_CarriesExtraEnv(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	extra := map[string]string{
		"PROOF_ANTHROPIC_API_KEY": "secret-value",
		"BOSS_PROOF_R2_BUCKET":    "bossanova-proof-production",
	}
	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "sid", "", extra); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.GetExtraEnv()["PROOF_ANTHROPIC_API_KEY"] != "secret-value" {
		t.Errorf("ExtraEnv secret not forwarded: %v", got.GetExtraEnv())
	}
	if got.GetExtraEnv()["BOSS_PROOF_R2_BUCKET"] != "bossanova-proof-production" {
		t.Errorf("ExtraEnv constant not forwarded: %v", got.GetExtraEnv())
	}
}

func TestPluginRunner_StartWithHeadlessCapabilityProfileCarriesProfile(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	if _, err := pr.StartWithHeadlessCapabilityProfile(
		context.Background(), "/work", "plan", nil, "sid", "model-for-preflight", map[string]string{"CODEX_HOME": "/projected/home"},
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	); err != nil {
		t.Fatalf("StartWithHeadlessCapabilityProfile: %v", err)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.GetHeadlessCapabilityProfile() != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("HeadlessCapabilityProfile = %s, want tracker-plan-attachment-v1", got.GetHeadlessCapabilityProfile())
	}
	if got.GetModel() != "model-for-preflight" || got.GetExtraEnv()["CODEX_HOME"] != "/projected/home" {
		t.Fatalf("preflight target fields = model=%q env=%v", got.GetModel(), got.GetExtraEnv())
	}
}

func TestPluginRunner_PreflightHeadlessCapabilityProfileCarriesOnlyTargetInputs(t *testing.T) {
	fc := &fakeAgentClient{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	err := pr.PreflightHeadlessCapabilityProfile(
		context.Background(),
		"model-for-preflight",
		map[string]string{"CODEX_HOME": "/projected/home", "ACCOUNT_TOKEN": "secret"},
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	)
	if err != nil {
		t.Fatalf("PreflightHeadlessCapabilityProfile: %v", err)
	}
	got := fc.preflightReq.Load()
	if got == nil {
		t.Fatal("PreflightHeadlessRun req not recorded")
	}
	if got.GetModel() != "model-for-preflight" {
		t.Fatalf("Model = %q, want model-for-preflight", got.GetModel())
	}
	if got.GetExtraEnv()["CODEX_HOME"] != "/projected/home" || got.GetExtraEnv()["ACCOUNT_TOKEN"] != "secret" {
		t.Fatalf("ExtraEnv = %v, want managed account env", got.GetExtraEnv())
	}
	if got.GetHeadlessCapabilityProfile() != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("HeadlessCapabilityProfile = %s, want tracker-plan-attachment-v1", got.GetHeadlessCapabilityProfile())
	}
}

func TestPluginRunner_PreflightHeadlessCapabilityProfilePropagatesError(t *testing.T) {
	fc := &fakeAgentClient{preflightErr: errors.New("capability unavailable")}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	err := pr.PreflightHeadlessCapabilityProfile(
		context.Background(), "model", nil,
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	)
	if !errors.Is(err, fc.preflightErr) {
		t.Fatalf("PreflightHeadlessCapabilityProfile error = %v, want wrapped %v", err, fc.preflightErr)
	}
}

func TestPluginRunner_Start_PropagatesError(t *testing.T) {
	fc := &fakeAgentClient{startErr: errors.New("boom")}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())
	_, err := pr.Start(context.Background(), "/w", "p", nil, "sid", "", nil)
	if err == nil || !errors.Is(err, fc.startErr) && err.Error() != "boom" {
		t.Errorf("expected wrapped err, got %v", err)
	}
}
