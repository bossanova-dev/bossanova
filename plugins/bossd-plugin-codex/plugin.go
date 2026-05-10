package main

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agentRunnerPlugin implements go-plugin's GRPCPlugin interface for the
// plugin-side. GRPCServer registers the AgentRunnerService implementation.
type agentRunnerPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	logger     zerolog.Logger
	runnerOpts []Option
}

func (p *agentRunnerPlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error { //nolint:unparam // interface implementation
	hostClient := newEagerHostServiceClient(broker, p.logger)
	server := newServer(hostClient, p.logger, p.runnerOpts...)
	srv.RegisterService(&agentRunnerServiceDesc, server)
	return nil
}

func (p *agentRunnerPlugin) GRPCClient(context.Context, *goplugin.GRPCBroker, *grpc.ClientConn) (any, error) {
	return nil, nil
}

// agentRunnerServiceDesc is the gRPC service descriptor. Built manually
// because the project uses connect-go for public APIs but plain gRPC
// for plugin RPCs (matches linear plugin's pattern).
var agentRunnerServiceDesc = grpc.ServiceDesc{
	ServiceName: "bossanova.v1.AgentRunnerService",
	HandlerType: (*agentRunnerServiceHandler)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "GetInfo", Handler: agentGetInfoHandler},
		{MethodName: "StartRun", Handler: agentStartRunHandler},
		{MethodName: "StopRun", Handler: agentStopRunHandler},
		{MethodName: "IsRunning", Handler: agentIsRunningHandler},
		{MethodName: "ExitStatus", Handler: agentExitStatusHandler},
		{MethodName: "ConfigureFinalizeHook", Handler: agentConfigureFinalizeHookHandler},
		{MethodName: "BuildInteractiveCommand", Handler: agentBuildInteractiveCommandHandler},
		{MethodName: "ListIgnoredDirtyFiles", Handler: agentListIgnoredDirtyFilesHandler},
		{MethodName: "GetChatTitle", Handler: agentGetChatTitleHandler},
		{MethodName: "HasQuestionPrompt", Handler: agentHasQuestionPromptHandler},
		{MethodName: "LastTurnIsUser", Handler: agentLastTurnIsUserHandler},
		{MethodName: "TranscriptExists", Handler: agentTranscriptExistsHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "bossanova/v1/plugin.proto",
}

type agentRunnerServiceHandler interface {
	GetInfo(context.Context, *bossanovav1.AgentRunnerServiceGetInfoRequest) (*bossanovav1.AgentRunnerServiceGetInfoResponse, error)
	StartRun(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error)
	StopRun(context.Context, *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error)
	IsRunning(context.Context, *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error)
	ExitStatus(context.Context, *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error)
	ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error)
	BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error)
	ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error)
	GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error)
	HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error)
	LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error)
	TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error)
}

func agentGetInfoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.AgentRunnerServiceGetInfoRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).GetInfo(ctx, req)
}

func agentStartRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartAgentRunRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).StartRun(ctx, req)
}

func agentStopRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StopAgentRunRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).StopRun(ctx, req)
}

func agentIsRunningHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.IsAgentRunningRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).IsRunning(ctx, req)
}

func agentExitStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.AgentExitStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).ExitStatus(ctx, req)
}

func agentConfigureFinalizeHookHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ConfigureFinalizeHookRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).ConfigureFinalizeHook(ctx, req)
}

func agentBuildInteractiveCommandHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.BuildInteractiveCommandRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).BuildInteractiveCommand(ctx, req)
}

func agentListIgnoredDirtyFilesHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListIgnoredDirtyFilesRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).ListIgnoredDirtyFiles(ctx, req)
}

func agentGetChatTitleHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetChatTitleRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).GetChatTitle(ctx, req)
}

func agentHasQuestionPromptHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.HasQuestionPromptRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).HasQuestionPrompt(ctx, req)
}

func agentLastTurnIsUserHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.LastTurnIsUserRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).LastTurnIsUser(ctx, req)
}

func agentTranscriptExistsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.TranscriptExistsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(agentRunnerServiceHandler).TranscriptExists(ctx, req)
}
