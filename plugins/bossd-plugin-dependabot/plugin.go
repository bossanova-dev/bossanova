package main

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// taskSourcePlugin implements go-plugin's GRPCPlugin interface for the
// plugin (server) side. GRPCServer registers the TaskSourceService
// implementation on the gRPC server. GRPCClient is unused on this side.
type taskSourcePlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	logger zerolog.Logger
}

func (p *taskSourcePlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error { //nolint:unparam // interface implementation
	// Create an eager host client that starts broker.Dial(1) immediately in a
	// background goroutine. The go-plugin broker cleans up ConnInfo after 5s,
	// so we must begin dialing now rather than deferring to the first RPC call
	// (which may arrive ~60s later on the first PollTasks cycle).
	hostClient := newEagerHostServiceClient(broker, p.logger)
	server := newServer(hostClient, p.logger)

	srv.RegisterService(&taskSourceServiceDesc, server)
	return nil
}

func (p *taskSourcePlugin) GRPCClient(context.Context, *goplugin.GRPCBroker, *grpc.ClientConn) (any, error) {
	// Plugin side does not use GRPCClient.
	return nil, nil
}

// taskSourceServiceDesc is a manually-built gRPC service descriptor for
// TaskSourceService. Like the host's HostService descriptor, we build this
// manually because the project uses connect-go (not protoc-gen-go-grpc).
var taskSourceServiceDesc = grpc.ServiceDesc{
	ServiceName: "bossanova.v1.TaskSourceService",
	HandlerType: (*taskSourceServiceHandler)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetInfo",
			Handler:    taskSourceGetInfoHandler,
		},
		{
			MethodName: "PollTasks",
			Handler:    taskSourcePollTasksHandler,
		},
		{
			MethodName: "UpdateTaskStatus",
			Handler:    taskSourceUpdateTaskStatusHandler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "bossanova/v1/plugin.proto",
}

// taskSourceServiceHandler is the interface that the gRPC service descriptor
// expects. The server type implements it.
type taskSourceServiceHandler interface {
	GetInfo(context.Context, *bossanovav1.TaskSourceServiceGetInfoRequest) (*bossanovav1.TaskSourceServiceGetInfoResponse, error)
	PollTasks(context.Context, *bossanovav1.PollTasksRequest) (*bossanovav1.PollTasksResponse, error)
	UpdateTaskStatus(context.Context, *bossanovav1.UpdateTaskStatusRequest) (*bossanovav1.UpdateTaskStatusResponse, error)
}

func taskSourceGetInfoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.TaskSourceServiceGetInfoRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).GetInfo(ctx, req)
}

func taskSourcePollTasksHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.PollTasksRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).PollTasks(ctx, req)
}

func taskSourceUpdateTaskStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.UpdateTaskStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).UpdateTaskStatus(ctx, req)
}
