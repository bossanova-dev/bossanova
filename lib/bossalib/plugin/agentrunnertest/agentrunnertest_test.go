package agentrunnertest

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	v1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// guard-id: agent-runner-conformance

// referenceCore deliberately inherits HasWorkingIndicator so removing the
// referenceRunner implementation exercises the generated Unimplemented fallback.
type referenceCore struct {
	v1.UnimplementedAgentRunnerServiceServer
}

func (*referenceCore) GetInfo(_ context.Context, _ *v1.AgentRunnerServiceGetInfoRequest) (*v1.AgentRunnerServiceGetInfoResponse, error) {
	return &v1.AgentRunnerServiceGetInfoResponse{Info: &v1.PluginInfo{Name: "reference", Version: "1", Capabilities: []string{"agent_runner"}}}, nil
}
func (*referenceCore) PreflightHeadlessRun(_ context.Context, _ *v1.PreflightHeadlessRunRequest) (*v1.PreflightHeadlessRunResponse, error) {
	return &v1.PreflightHeadlessRunResponse{}, nil
}
func (*referenceCore) StartAgentRun(_ context.Context, _ *v1.StartAgentRunRequest) (*v1.StartAgentRunResponse, error) {
	return &v1.StartAgentRunResponse{SessionId: "provider-session"}, nil
}
func (*referenceCore) StopRun(_ context.Context, _ *v1.StopAgentRunRequest) (*v1.StopAgentRunResponse, error) {
	return &v1.StopAgentRunResponse{}, nil
}
func (*referenceCore) IsRunning(_ context.Context, _ *v1.IsAgentRunningRequest) (*v1.IsAgentRunningResponse, error) {
	return &v1.IsAgentRunningResponse{}, nil
}
func (*referenceCore) ExitStatus(_ context.Context, _ *v1.AgentExitStatusRequest) (*v1.AgentExitStatusResponse, error) {
	return &v1.AgentExitStatusResponse{IsComplete: true}, nil
}
func (*referenceCore) ConfigureFinalizeHook(_ context.Context, _ *v1.ConfigureFinalizeHookRequest) (*v1.ConfigureFinalizeHookResponse, error) {
	return &v1.ConfigureFinalizeHookResponse{}, nil
}
func (*referenceCore) RemoveAgentRunHook(_ context.Context, _ *v1.RemoveAgentRunHookRequest) (*v1.RemoveAgentRunHookResponse, error) {
	return &v1.RemoveAgentRunHookResponse{}, nil
}
func (*referenceCore) BuildInteractiveCommand(_ context.Context, _ *v1.BuildInteractiveCommandRequest) (*v1.BuildInteractiveCommandResponse, error) {
	return &v1.BuildInteractiveCommandResponse{Argv: []string{"reference-agent"}}, nil
}
func (*referenceCore) ResolveInteractiveSessionID(_ context.Context, _ *v1.ResolveInteractiveSessionIDRequest) (*v1.ResolveInteractiveSessionIDResponse, error) {
	return &v1.ResolveInteractiveSessionIDResponse{}, nil
}
func (*referenceCore) ListIgnoredDirtyFiles(_ context.Context, _ *v1.ListIgnoredDirtyFilesRequest) (*v1.ListIgnoredDirtyFilesResponse, error) {
	return &v1.ListIgnoredDirtyFilesResponse{}, nil
}
func (*referenceCore) GetChatTitle(_ context.Context, _ *v1.GetChatTitleRequest) (*v1.GetChatTitleResponse, error) {
	return &v1.GetChatTitleResponse{}, nil
}
func (*referenceCore) SuggestPRTitle(_ context.Context, _ *v1.SuggestPRTitleRequest) (*v1.SuggestPRTitleResponse, error) {
	return &v1.SuggestPRTitleResponse{}, nil
}
func (*referenceCore) HasQuestionPrompt(_ context.Context, _ *v1.HasQuestionPromptRequest) (*v1.HasQuestionPromptResponse, error) {
	return &v1.HasQuestionPromptResponse{}, nil
}
func (*referenceCore) LastTurnIsUser(_ context.Context, _ *v1.LastTurnIsUserRequest) (*v1.LastTurnIsUserResponse, error) {
	return &v1.LastTurnIsUserResponse{}, nil
}
func (*referenceCore) DetectUsageLimit(_ context.Context, _ *v1.DetectUsageLimitRequest) (*v1.DetectUsageLimitResponse, error) {
	return &v1.DetectUsageLimitResponse{}, nil
}
func (*referenceCore) ProbeRateLimit(_ context.Context, _ *v1.ProbeRateLimitRequest) (*v1.ProbeRateLimitResponse, error) {
	return &v1.ProbeRateLimitResponse{Status: &v1.RateLimitStatus{Status: v1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED}}, nil
}
func (*referenceCore) TranscriptExists(_ context.Context, _ *v1.TranscriptExistsRequest) (*v1.TranscriptExistsResponse, error) {
	return &v1.TranscriptExistsResponse{}, nil
}
func (*referenceCore) ReadTranscript(_ context.Context, _ *v1.ReadTranscriptRequest) (*v1.ReadTranscriptResponse, error) {
	return &v1.ReadTranscriptResponse{}, nil
}
func (*referenceCore) ProbeProgressLiveness(_ context.Context, _ *v1.ProbeProgressLivenessRequest) (*v1.ProbeProgressLivenessResponse, error) {
	return &v1.ProbeProgressLivenessResponse{Phase: v1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN}, nil
}
func (*referenceCore) RotationCapability(_ context.Context, _ *v1.RotationCapabilityRequest) (*v1.RotationCapabilityResponse, error) {
	return &v1.RotationCapabilityResponse{}, nil
}
func (*referenceCore) MaterializeAgentAccount(_ context.Context, _ *v1.MaterializeAgentAccountRequest) (*v1.MaterializeAgentAccountResponse, error) {
	return &v1.MaterializeAgentAccountResponse{}, nil
}
func (*referenceCore) DescribeMCPSurface(_ context.Context, _ *v1.DescribeMCPSurfaceRequest) (*v1.DescribeMCPSurfaceResponse, error) {
	return &v1.DescribeMCPSurfaceResponse{}, nil
}

type referenceRunner struct{ referenceCore }

func (*referenceRunner) HasWorkingIndicator(context.Context, *v1.HasWorkingIndicatorRequest) (*v1.HasWorkingIndicatorResponse, error) {
	return &v1.HasWorkingIndicatorResponse{}, nil
}

type brokenWorkingRunner struct{ referenceRunner }

func (*brokenWorkingRunner) HasWorkingIndicator(context.Context, *v1.HasWorkingIndicatorRequest) (*v1.HasWorkingIndicatorResponse, error) {
	return &v1.HasWorkingIndicatorResponse{IsWorking: true}, nil
}

type missingStatusRunner struct{ referenceRunner }

func (*missingStatusRunner) ProbeRateLimit(context.Context, *v1.ProbeRateLimitRequest) (*v1.ProbeRateLimitResponse, error) {
	return &v1.ProbeRateLimitResponse{}, nil
}

func testEnv(t *testing.T) Env {
	t.Helper()
	return Env{WorkDir: t.TempDir(), LogPath: filepath.Join(t.TempDir(), "agent.log"), UnknownSessionID: "never-started"}
}

func TestCheckFailsOnSeededViolations(t *testing.T) {
	cases := []struct {
		name        string
		server      v1.AgentRunnerServiceServer
		unsupported []string
		method      string
	}{
		{name: "conforming reference", server: &referenceRunner{}},
		{name: "removed supported RPC", server: &referenceCore{}, method: "HasWorkingIndicator"},
		{name: "broken supported RPC", server: &brokenWorkingRunner{}, method: "HasWorkingIndicator"},
		{name: "falsely declared unsupported", server: &referenceRunner{}, unsupported: []string{v1.AgentRunnerService_HasWorkingIndicator_FullMethodName}, method: "HasWorkingIndicator"},
		{name: "unknown unsupported name", server: &referenceRunner{}, unsupported: []string{fullMethod("DoesNotExist")}, method: fullMethod("DoesNotExist")},
		{name: "missing rate limit status", server: &missingStatusRunner{}, method: "ProbeRateLimit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := Runner{Name: tc.name, Client: Serve(t, tc.server), Unsupported: tc.unsupported}
			violations := Check(t.Context(), runner, testEnv(t))
			if tc.method == "" {
				if len(violations) != 0 {
					t.Fatalf("reference runner failed: %+v", violations)
				}
				return
			}
			if !slices.ContainsFunc(violations, func(v Violation) bool { return v.Method == tc.method }) {
				t.Fatalf("seeded %s violation absent: %+v", tc.method, violations)
			}
		})
	}
}

func TestEveryRPCHasAContract(t *testing.T) {
	descriptor := v1.AgentRunnerService_ServiceDesc
	names := make(map[string]bool, len(contracts))
	for _, row := range contracts {
		if names[row.name] {
			t.Errorf("duplicate contract: %s", row.name)
		}
		names[row.name] = true
	}
	for _, method := range descriptor.Methods {
		if !names[method.MethodName] {
			t.Errorf("RPC %s has no contract", method.MethodName)
		}
		delete(names, method.MethodName)
	}
	for _, stream := range descriptor.Streams {
		if !names[stream.StreamName] {
			t.Errorf("streaming RPC %s has no contract", stream.StreamName)
		}
		delete(names, stream.StreamName)
	}
	for name := range names {
		t.Errorf("contract %s is not an RPC", name)
	}
}

func TestCheckDeclaredUnsupported(t *testing.T) {
	runner := Runner{Name: "intentional gap", Client: Serve(t, &referenceCore{}), Unsupported: []string{v1.AgentRunnerService_HasWorkingIndicator_FullMethodName}}
	if violations := Check(t.Context(), runner, testEnv(t)); len(violations) != 0 {
		t.Fatalf("declared generated fallback failed: %+v", violations)
	}
}

func TestRun(t *testing.T) { Run(t, Runner{Name: "reference", Client: Serve(t, &referenceRunner{})}) }

type cancellationRunner struct {
	referenceRunner
	contexts chan context.Context
}

func (s *cancellationRunner) GetInfo(ctx context.Context, _ *v1.AgentRunnerServiceGetInfoRequest) (*v1.AgentRunnerServiceGetInfoResponse, error) {
	s.contexts <- ctx
	return &v1.AgentRunnerServiceGetInfoResponse{}, nil
}

func TestServeCancelsRPCContextAfterReturn(t *testing.T) {
	server := &cancellationRunner{contexts: make(chan context.Context, 1)}
	client := Serve(t, server)
	if _, err := client.GetInfo(t.Context(), &v1.AgentRunnerServiceGetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx := <-server.contexts
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("server RPC context was not cancelled after return")
	}
}
