package main

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// workflowPlugin implements go-plugin's GRPCPlugin interface for the
// plugin (server) side. GRPCServer registers the WorkflowService
// implementation on the gRPC server. GRPCClient is unused on this side.
type workflowPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	logger zerolog.Logger
}

func (p *workflowPlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error { //nolint:unparam // interface implementation
	// Create a host client that eagerly starts broker.Dial(1) in a background
	// goroutine. The go-plugin broker cleans up pending connection info after
	// 5 seconds, so we must start the Dial immediately rather than deferring
	// to the first RPC call. The goroutine blocks until the host calls
	// AcceptAndServe on broker ID 1 (which happens during Dispense).
	hostClient := newEagerHostServiceClient(broker, p.logger)
	server := newOrchestrator(hostClient, p.logger)

	srv.RegisterService(&workflowServiceDesc, server)
	return nil
}

func (p *workflowPlugin) GRPCClient(context.Context, *goplugin.GRPCBroker, *grpc.ClientConn) (any, error) {
	// Plugin side does not use GRPCClient.
	return nil, nil
}

// workflowServiceDesc is a manually-built gRPC service descriptor for
// WorkflowService. Like the host's HostService descriptor, we build this
// manually because the project uses connect-go (not protoc-gen-go-grpc).
var workflowServiceDesc = grpc.ServiceDesc{
	ServiceName: "bossanova.v1.WorkflowService",
	HandlerType: (*workflowServiceHandler)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetInfo",
			Handler:    workflowGetInfoHandler,
		},
		{
			MethodName: "StartWorkflow",
			Handler:    workflowStartWorkflowHandler,
		},
		{
			MethodName: "PauseWorkflow",
			Handler:    workflowPauseWorkflowHandler,
		},
		{
			MethodName: "ResumeWorkflow",
			Handler:    workflowResumeWorkflowHandler,
		},
		{
			MethodName: "CancelWorkflow",
			Handler:    workflowCancelWorkflowHandler,
		},
		{
			MethodName: "GetWorkflowStatus",
			Handler:    workflowGetWorkflowStatusHandler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "bossanova/v1/plugin.proto",
}

// workflowServiceHandler is the interface that the gRPC service descriptor
// expects. The orchestrator type implements it.
type workflowServiceHandler interface {
	GetInfo(context.Context, *bossanovav1.WorkflowServiceGetInfoRequest) (*bossanovav1.WorkflowServiceGetInfoResponse, error)
	StartWorkflow(context.Context, *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error)
	PauseWorkflow(context.Context, *bossanovav1.PauseWorkflowRequest) (*bossanovav1.PauseWorkflowResponse, error)
	ResumeWorkflow(context.Context, *bossanovav1.ResumeWorkflowRequest) (*bossanovav1.ResumeWorkflowResponse, error)
	CancelWorkflow(context.Context, *bossanovav1.CancelWorkflowRequest) (*bossanovav1.CancelWorkflowResponse, error)
	GetWorkflowStatus(context.Context, *bossanovav1.GetWorkflowStatusRequest) (*bossanovav1.GetWorkflowStatusResponse, error)
}

func workflowGetInfoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.WorkflowServiceGetInfoRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(workflowServiceHandler).GetInfo(ctx, req)
}

func workflowStartWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(workflowServiceHandler).StartWorkflow(ctx, req)
}

func workflowPauseWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.PauseWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(workflowServiceHandler).PauseWorkflow(ctx, req)
}

func workflowResumeWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ResumeWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(workflowServiceHandler).ResumeWorkflow(ctx, req)
}

func workflowCancelWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.CancelWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(workflowServiceHandler).CancelWorkflow(ctx, req)
}

func workflowGetWorkflowStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetWorkflowStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(workflowServiceHandler).GetWorkflowStatus(ctx, req)
}
