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

func (p *taskSourcePlugin) GRPCServer(_ *goplugin.GRPCBroker, srv *grpc.Server) error { //nolint:unparam // interface implementation
	// Unlike the Linear plugin, the Sentry plugin does not call back into the
	// host: Sentry issues are mapped directly to TrackerIssues without matching
	// against open PRs, so there is no broker dial here.
	srv.RegisterService(&taskSourceServiceDesc, newServer(p.logger))
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
		{
			MethodName: "ListAvailableIssues",
			Handler:    taskSourceListAvailableIssuesHandler,
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
	ListAvailableIssues(context.Context, *bossanovav1.ListAvailableIssuesRequest) (*bossanovav1.ListAvailableIssuesResponse, error)
}

func taskSourceGetInfoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.TaskSourceServiceGetInfoRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).GetInfo(ctx, req) //nolint:forcetypeassert // srv/req types are guaranteed by the gRPC ServiceDesc registration and message decoder; mirrors protoc-gen-go-grpc dispatch
}

func taskSourcePollTasksHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.PollTasksRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).PollTasks(ctx, req) //nolint:forcetypeassert // srv/req types are guaranteed by the gRPC ServiceDesc registration and message decoder; mirrors protoc-gen-go-grpc dispatch
}

func taskSourceUpdateTaskStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.UpdateTaskStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).UpdateTaskStatus(ctx, req) //nolint:forcetypeassert // srv/req types are guaranteed by the gRPC ServiceDesc registration and message decoder; mirrors protoc-gen-go-grpc dispatch
}

func taskSourceListAvailableIssuesHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListAvailableIssuesRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(taskSourceServiceHandler).ListAvailableIssues(ctx, req) //nolint:forcetypeassert // srv/req types are guaranteed by the gRPC ServiceDesc registration and message decoder; mirrors protoc-gen-go-grpc dispatch
}
