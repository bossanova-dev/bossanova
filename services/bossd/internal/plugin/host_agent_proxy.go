package plugin

import (
	"context"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agentRunnerProxy is a stable, per-plugin-name handle to an agent plugin's
// AgentRunner interface. It holds NO reference to a dispensed gRPC client;
// instead it resolves the current client via host.AgentRunnerByName(name) on
// every call and delegates.
//
// This indirection is what lets the plugin host respawn a dead agent plugin
// without breaking the long-lived registries built at startup. Those registries
// (HostServiceServer.agentClients, agent.Dispatcher.runners, the account
// materializer/smoke runner) capture the values returned by Host.AgentRunners()
// once and never re-read the host. A raw dispensed client is bound to one
// subprocess's unix socket and dies with that subprocess; a proxy follows the
// live plugin across restarts.
//
// When the plugin is momentarily absent (dead and not yet relaunched by the
// health loop), resolve returns a codes.Unavailable error. Unavailable is the
// honest, retryable signal — and RotationCapability's Unimplemented degrade is
// unaffected because Unavailable != Unimplemented, so a restarting agent is
// treated as a transient failure rather than "no rotation support".
type agentRunnerProxy struct {
	host *Host
	name string
}

// resolve returns the live AgentRunner for this proxy's plugin, or a
// codes.Unavailable error if the plugin is not currently loaded (e.g. exited
// and awaiting relaunch by the health loop).
func (p *agentRunnerProxy) resolve() (AgentRunner, error) {
	if r := p.host.AgentRunnerByName(p.name); r != nil {
		return r, nil
	}
	return nil, grpcstatus.Errorf(codes.Unavailable, "agent %q is not currently loaded (plugin restarting?)", p.name)
}

// Compile-time check: the proxy must satisfy the full AgentRunner interface so
// it can stand in for a dispensed client everywhere. AgentRunner is a superset
// of agent.AgentRunnerClient, so this also guarantees the main.go
// `raw.(agent.AgentRunnerClient)` assertion succeeds while allowing newer RPCs
// to remain optional at that compatibility seam.
var _ AgentRunner = (*agentRunnerProxy)(nil)

func (p *agentRunnerProxy) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.GetInfo(ctx)
}

func (p *agentRunnerProxy) PreflightHeadlessRun(ctx context.Context, req *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.PreflightHeadlessRun(ctx, req)
}

func (p *agentRunnerProxy) StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.StartRun(ctx, req)
}

func (p *agentRunnerProxy) StopRun(ctx context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.StopRun(ctx, req)
}

func (p *agentRunnerProxy) IsRunning(ctx context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.IsRunning(ctx, req)
}

func (p *agentRunnerProxy) ExitStatus(ctx context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.ExitStatus(ctx, req)
}

func (p *agentRunnerProxy) ConfigureFinalizeHook(ctx context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.ConfigureFinalizeHook(ctx, req)
}

func (p *agentRunnerProxy) RemoveAgentRunHook(ctx context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.RemoveAgentRunHook(ctx, req)
}

func (p *agentRunnerProxy) BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.BuildInteractiveCommand(ctx, req)
}

func (p *agentRunnerProxy) ResolveInteractiveSessionID(ctx context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.ResolveInteractiveSessionID(ctx, req)
}

func (p *agentRunnerProxy) ListIgnoredDirtyFiles(ctx context.Context, req *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.ListIgnoredDirtyFiles(ctx, req)
}

func (p *agentRunnerProxy) GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.GetChatTitle(ctx, req)
}

func (p *agentRunnerProxy) SuggestPRTitle(ctx context.Context, req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.SuggestPRTitle(ctx, req)
}

func (p *agentRunnerProxy) HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.HasQuestionPrompt(ctx, req)
}

func (p *agentRunnerProxy) DetectUsageLimit(ctx context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.DetectUsageLimit(ctx, req)
}

func (p *agentRunnerProxy) ProbeRateLimit(ctx context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.ProbeRateLimit(ctx, req)
}

func (p *agentRunnerProxy) HasWorkingIndicator(ctx context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.HasWorkingIndicator(ctx, req)
}

func (p *agentRunnerProxy) LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.LastTurnIsUser(ctx, req)
}

func (p *agentRunnerProxy) TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.TranscriptExists(ctx, req)
}

func (p *agentRunnerProxy) ReadTranscript(ctx context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.ReadTranscript(ctx, req)
}

func (p *agentRunnerProxy) RotationCapability(ctx context.Context, req *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.RotationCapability(ctx, req)
}

func (p *agentRunnerProxy) MaterializeAccount(ctx context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	r, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return r.MaterializeAccount(ctx, req)
}
