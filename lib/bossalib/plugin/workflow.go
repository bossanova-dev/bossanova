// Package plugin provides shared plugin infrastructure used by both the daemon
// (host) and plugin binaries.
package plugin

import (
	"context"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc"
)

// WorkflowServiceHandler is the interface that workflow plugin servers must
// implement. Both the autopilot orchestrator and repair monitor satisfy it.
type WorkflowServiceHandler interface {
	GetInfo(context.Context, *bossanovav1.WorkflowServiceGetInfoRequest) (*bossanovav1.WorkflowServiceGetInfoResponse, error)
	StartWorkflow(context.Context, *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error)
	PauseWorkflow(context.Context, *bossanovav1.PauseWorkflowRequest) (*bossanovav1.PauseWorkflowResponse, error)
	ResumeWorkflow(context.Context, *bossanovav1.ResumeWorkflowRequest) (*bossanovav1.ResumeWorkflowResponse, error)
	CancelWorkflow(context.Context, *bossanovav1.CancelWorkflowRequest) (*bossanovav1.CancelWorkflowResponse, error)
	GetWorkflowStatus(context.Context, *bossanovav1.GetWorkflowStatusRequest) (*bossanovav1.GetWorkflowStatusResponse, error)
	NotifyStatusChange(context.Context, *bossanovav1.NotifyStatusChangeRequest) (*bossanovav1.NotifyStatusChangeResponse, error)
}

// WorkflowServiceDesc is a manually-built gRPC service descriptor for
// WorkflowService. We build this manually because the project uses
// connect-go (not protoc-gen-go-grpc), so no _grpc.pb.go is generated.
var WorkflowServiceDesc = grpc.ServiceDesc{
	ServiceName: "bossanova.v1.WorkflowService",
	HandlerType: (*WorkflowServiceHandler)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "GetInfo", Handler: workflowGetInfoHandler},
		{MethodName: "StartWorkflow", Handler: workflowStartWorkflowHandler},
		{MethodName: "PauseWorkflow", Handler: workflowPauseWorkflowHandler},
		{MethodName: "ResumeWorkflow", Handler: workflowResumeWorkflowHandler},
		{MethodName: "CancelWorkflow", Handler: workflowCancelWorkflowHandler},
		{MethodName: "GetWorkflowStatus", Handler: workflowGetWorkflowStatusHandler},
		{MethodName: "NotifyStatusChange", Handler: workflowNotifyStatusChangeHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "bossanova/v1/plugin.proto",
}

func workflowGetInfoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.WorkflowServiceGetInfoRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).GetInfo(ctx, req)
}

func workflowStartWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).StartWorkflow(ctx, req)
}

func workflowPauseWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.PauseWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).PauseWorkflow(ctx, req)
}

func workflowResumeWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ResumeWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).ResumeWorkflow(ctx, req)
}

func workflowCancelWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.CancelWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).CancelWorkflow(ctx, req)
}

func workflowGetWorkflowStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetWorkflowStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).GetWorkflowStatus(ctx, req)
}

func workflowNotifyStatusChangeHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.NotifyStatusChangeRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(WorkflowServiceHandler).NotifyStatusChange(ctx, req)
}
