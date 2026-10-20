package plugin

import (
	"context"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
)

// AgentRunner is the host-side interface for AgentRunnerService plugins.
// It mirrors every RPC in AgentRunnerService (plugin.proto).
type AgentRunner interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	PreflightHeadlessRun(ctx context.Context, req *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error)
	StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error)
	StopRun(ctx context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error)
	IsRunning(ctx context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error)
	ExitStatus(ctx context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error)
	ConfigureFinalizeHook(ctx context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error)
	RemoveAgentRunHook(ctx context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error)
	BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error)
	ResolveInteractiveSessionID(ctx context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error)
	ListIgnoredDirtyFiles(ctx context.Context, req *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error)
	GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error)
	SuggestPRTitle(ctx context.Context, req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error)
	HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error)
	DetectUsageLimit(ctx context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error)
	ProbeRateLimit(ctx context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error)
	HasWorkingIndicator(ctx context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error)
	LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error)
	TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error)
	ReadTranscript(ctx context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error)
	RotationCapability(ctx context.Context, req *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error)
	MaterializeAccount(ctx context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error)
	ProbeProgressLiveness(ctx context.Context, req *bossanovav1.ProbeProgressLivenessRequest) (*bossanovav1.ProbeProgressLivenessResponse, error)
	DescribeMCPSurface(ctx context.Context, req *bossanovav1.DescribeMCPSurfaceRequest) (*bossanovav1.DescribeMCPSurfaceResponse, error)
}

func NewAgentRunnerGRPCPlugin(hostService *HostServiceServer) *sharedplugin.AgentRunnerPlugin {
	return &sharedplugin.AgentRunnerPlugin{HostService: optionalHostService(hostService)}
}

// agentRunnerGRPCClient implements AgentRunner by forwarding calls to the
// plugin subprocess over the shared gRPC connection.
type agentRunnerGRPCClient struct {
	client bossanovav1.AgentRunnerServiceClient
}

func (c *agentRunnerGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp, err := invokePluginUnary(ctx, c.client.GetInfo, &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *agentRunnerGRPCClient) PreflightHeadlessRun(ctx context.Context, req *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.PreflightHeadlessRun, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.StartAgentRun, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) StopRun(ctx context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.StopRun, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) IsRunning(ctx context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.IsRunning, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ExitStatus(ctx context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ExitStatus, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ConfigureFinalizeHook(ctx context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ConfigureFinalizeHook, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) RemoveAgentRunHook(ctx context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.RemoveAgentRunHook, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.BuildInteractiveCommand, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ResolveInteractiveSessionID(ctx context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ResolveInteractiveSessionID, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ListIgnoredDirtyFiles(ctx context.Context, req *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ListIgnoredDirtyFiles, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.GetChatTitle, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) SuggestPRTitle(ctx context.Context, req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.SuggestPRTitle, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.HasQuestionPrompt, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) DetectUsageLimit(ctx context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.DetectUsageLimit, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ProbeRateLimit(ctx context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ProbeRateLimit, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) HasWorkingIndicator(ctx context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.HasWorkingIndicator, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.LastTurnIsUser, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.TranscriptExists, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ReadTranscript(ctx context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ReadTranscript, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// degradeRotationCapability applies the D8 status-only degrade: a plugin that
// does not implement RotationCapability (codes.Unimplemented) is treated as
// non-supporting (supports_rotation=false), NOT as an error. Any other error
// propagates unchanged.
func degradeRotationCapability(resp *bossanovav1.RotationCapabilityResponse, err error) (*bossanovav1.RotationCapabilityResponse, error) {
	if err != nil {
		if grpcstatus.Code(err) == codes.Unimplemented {
			return &bossanovav1.RotationCapabilityResponse{SupportsRotation: false}, nil
		}
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) RotationCapability(ctx context.Context, req *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.RotationCapability, req)
	return degradeRotationCapability(resp, err)
}

// MaterializeAccount forwards to the plugin. Unlike RotationCapability it does
// NOT degrade Unimplemented — it is only called for agents already known to
// support rotation, so an Unimplemented here is a real programming error.
func (c *agentRunnerGRPCClient) MaterializeAccount(ctx context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.MaterializeAccount, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ProbeProgressLiveness forwards to the plugin (BOS-667). The error is returned
// verbatim — including Unimplemented from a runner binary built before the RPC
// existed — because the only caller (internal/status) fails open on any error,
// so degrading here would just hide which plugins lack the capability.
func (c *agentRunnerGRPCClient) ProbeProgressLiveness(ctx context.Context, req *bossanovav1.ProbeProgressLivenessRequest) (*bossanovav1.ProbeProgressLivenessResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.ProbeProgressLiveness, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// DescribeMCPSurface forwards to the plugin (BOS-867). The error is returned
// verbatim — including Unimplemented from a runner binary that predates the RPC
// or an agent that simply cannot answer — because the caller
// (server.DescribeChatMCP) is what turns Unimplemented into a typed
// is_supported=false answer. Degrading here would hide WHICH plugins lack the
// capability from every other caller.
//
// req.probe_env is secret-bearing and is never logged on this path.
func (c *agentRunnerGRPCClient) DescribeMCPSurface(ctx context.Context, req *bossanovav1.DescribeMCPSurfaceRequest) (*bossanovav1.DescribeMCPSurfaceResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.DescribeMCPSurface, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// NewAgentRunnerClient adapts a generated client to the daemon consumer interface.
func NewAgentRunnerClient(client bossanovav1.AgentRunnerServiceClient) AgentRunner {
	return &agentRunnerGRPCClient{client: client}
}
