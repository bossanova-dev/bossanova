package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
)

// AgentRunner is the host-side interface for AgentRunnerService plugins.
// It mirrors every RPC in AgentRunnerService (plugin.proto).
type AgentRunner interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error)
	StopRun(ctx context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error)
	IsRunning(ctx context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error)
	ExitStatus(ctx context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error)
	ConfigureFinalizeHook(ctx context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error)
	BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error)
	ListIgnoredDirtyFiles(ctx context.Context, req *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error)
	GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error)
	HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error)
	LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error)
	TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error)
}

// AgentRunnerGRPCPlugin implements go-plugin's GRPCPlugin interface for
// the AgentRunnerService. When HostService is set, GRPCClient registers it
// on broker ID 2 (sharedplugin.BrokerIDAgentRunnerHostService) so the plugin
// subprocess can call back to the host without colliding with the TaskSource /
// WorkflowService broker on ID 1.
type AgentRunnerGRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	HostService *HostServiceServer
}

func (p *AgentRunnerGRPCPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return nil
}

func (p *AgentRunnerGRPCPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	if p.HostService != nil {
		serverFunc := func(opts []grpc.ServerOption) *grpc.Server {
			srv := grpc.NewServer(opts...)
			p.HostService.Register(srv)
			return srv
		}
		// 2 == sharedplugin.BrokerIDAgentRunnerHostService; kept numeric to
		// mirror the TaskSource pattern which hardcodes 1.
		go broker.AcceptAndServe(sharedplugin.BrokerIDAgentRunnerHostService, serverFunc)
	}
	return &agentRunnerGRPCClient{conn: conn}, nil
}

// NewAgentRunnerGRPCPlugin creates an AgentRunnerGRPCPlugin with the given
// HostService. Exported for integration testing.
func NewAgentRunnerGRPCPlugin(hostService *HostServiceServer) *AgentRunnerGRPCPlugin {
	return &AgentRunnerGRPCPlugin{HostService: hostService}
}

// agentRunnerGRPCClient implements AgentRunner by forwarding calls to the
// plugin subprocess over the shared gRPC connection.
type agentRunnerGRPCClient struct {
	conn *grpc.ClientConn
}

func (c *agentRunnerGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp := &bossanovav1.AgentRunnerServiceGetInfoResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/GetInfo", &bossanovav1.AgentRunnerServiceGetInfoRequest{}, resp); err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *agentRunnerGRPCClient) StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	resp := &bossanovav1.StartAgentRunResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/StartRun", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) StopRun(ctx context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	resp := &bossanovav1.StopAgentRunResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/StopRun", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) IsRunning(ctx context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	resp := &bossanovav1.IsAgentRunningResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/IsRunning", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ExitStatus(ctx context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	resp := &bossanovav1.AgentExitStatusResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/ExitStatus", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ConfigureFinalizeHook(ctx context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	resp := &bossanovav1.ConfigureFinalizeHookResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/ConfigureFinalizeHook", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	resp := &bossanovav1.BuildInteractiveCommandResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/BuildInteractiveCommand", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) ListIgnoredDirtyFiles(ctx context.Context, req *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	resp := &bossanovav1.ListIgnoredDirtyFilesResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/ListIgnoredDirtyFiles", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) GetChatTitle(ctx context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	resp := &bossanovav1.GetChatTitleResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/GetChatTitle", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) HasQuestionPrompt(ctx context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	resp := &bossanovav1.HasQuestionPromptResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/HasQuestionPrompt", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) LastTurnIsUser(ctx context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	resp := &bossanovav1.LastTurnIsUserResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/LastTurnIsUser", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *agentRunnerGRPCClient) TranscriptExists(ctx context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	resp := &bossanovav1.TranscriptExistsResponse{}
	if err := invokePluginUnary(ctx, c.conn, "/bossanova.v1.AgentRunnerService/TranscriptExists", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}
