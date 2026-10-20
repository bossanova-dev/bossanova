package plugin

import (
	"context"
	"time"

	sharedplugin "github.com/recurser/bossalib/plugin"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// defaultPluginRPCTimeout bounds most unary RPCs the daemon dispatches to a
// plugin subprocess. A hung or mis-behaving plugin must not block the daemon
// indefinitely. Callers that need a tighter bound (e.g. NotifyStatusChange at
// 5s) wrap the ctx before calling in; callers that legitimately need a longer
// bound can extend ctx with an explicit deadline — this timeout is applied as
// a ceiling on top of the caller's ctx, so the shorter of the two wins.
//
// No daemon → plugin RPC is server-streaming today. If a server-streaming RPC
// is added, its client method must bypass invokePluginUnary to avoid a
// premature 30s cutoff on the stream.
const defaultPluginRPCTimeout = 30 * time.Second

// pollTasksRPCTimeout is the looser ceiling for TaskSourceService.PollTasks.
// PollTasks is iterative: a single call may classify many PRs/issues, with
// each item triggering a host callback that shells out to the VCS provider
// (e.g. `gh pr checks`). The deadline propagates from the daemon's outbound
// PollTasks RPC into those callbacks, so a 30s ceiling caused repos with
// 20+ open dependabot PRs to fail mid-loop with "context canceled". 90s
// covers a realistic worst-case sequential traversal while still firmly
// under the orchestrator's 2-minute poll interval (avoiding pile-up).
const pollTasksRPCTimeout = 90 * time.Second

// invokePluginUnary applies the daemon's unary deadline to a generated call.
func invokePluginUnary[Req, Resp any](ctx context.Context, call func(context.Context, *Req, ...grpc.CallOption) (*Resp, error), req *Req) (*Resp, error) {
	return invokePluginUnaryWithTimeout(ctx, defaultPluginRPCTimeout, call, req)
}

func invokePluginUnaryWithTimeout[Req, Resp any](ctx context.Context, timeout time.Duration, call func(context.Context, *Req, ...grpc.CallOption) (*Resp, error), req *Req) (*Resp, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return call(ctx, req)
}

// --- Host-side interfaces ---
// These define what the host can call on each plugin type.

// TaskSource is the host-side interface for TaskSourceService plugins.
type TaskSource interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	PollTasks(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error)
	UpdateTaskStatus(ctx context.Context, externalID string, status bossanovav1.TaskItemStatus, details string) error
	ListAvailableIssues(ctx context.Context, repoOriginURL string, query string, config map[string]string) ([]*bossanovav1.TrackerIssue, error)
}

// WorkflowService is the host-side interface for WorkflowService plugins.
type WorkflowService interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	StartWorkflow(ctx context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error)
	PauseWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	ResumeWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	CancelWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	GetWorkflowStatus(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	NotifyStatusChange(ctx context.Context, sessionID string, displayStatus bossanovav1.DisplayStatus, hasFailures bool) error
}

func NewTaskSourceGRPCPlugin(hostService *HostServiceServer) *sharedplugin.TaskSourcePlugin {
	return &sharedplugin.TaskSourcePlugin{HostService: optionalHostService(hostService)}
}

// --- gRPC client implementations ---
// These preserve the daemon interfaces around generated gRPC clients.

type taskSourceGRPCClient struct {
	client bossanovav1.TaskSourceServiceClient
}

func (c *taskSourceGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp, err := invokePluginUnary(ctx, c.client.GetInfo, &bossanovav1.TaskSourceServiceGetInfoRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *taskSourceGRPCClient) PollTasks(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error) {
	req := &bossanovav1.PollTasksRequest{}
	if repoOriginURL != "" {
		req.RepoOriginUrl = &repoOriginURL
	}
	resp, err := invokePluginUnaryWithTimeout(ctx, pollTasksRPCTimeout, c.client.PollTasks, req)
	if err != nil {
		return nil, err
	}
	return resp.GetTasks(), nil
}

func (c *taskSourceGRPCClient) UpdateTaskStatus(ctx context.Context, externalID string, status bossanovav1.TaskItemStatus, details string) error {
	req := &bossanovav1.UpdateTaskStatusRequest{
		ExternalId: externalID,
		Status:     status,
		Details:    details,
	}
	_, err := invokePluginUnary(ctx, c.client.UpdateTaskStatus, req)
	return err
}

func (c *taskSourceGRPCClient) ListAvailableIssues(ctx context.Context, repoOriginURL string, query string, config map[string]string) ([]*bossanovav1.TrackerIssue, error) {
	req := &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: repoOriginURL,
		Config:        config,
		Query:         query,
	}
	resp, err := invokePluginUnary(ctx, c.client.ListAvailableIssues, req)
	if err != nil {
		return nil, err
	}
	return resp.GetIssues(), nil
}

type workflowServiceGRPCClient struct {
	client bossanovav1.WorkflowServiceClient
}

func (c *workflowServiceGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp, err := invokePluginUnary(ctx, c.client.GetInfo, &bossanovav1.WorkflowServiceGetInfoRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *workflowServiceGRPCClient) StartWorkflow(ctx context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error) {
	resp, err := invokePluginUnary(ctx, c.client.StartWorkflow, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *workflowServiceGRPCClient) PauseWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.PauseWorkflowRequest{WorkflowId: workflowID}
	resp, err := invokePluginUnary(ctx, c.client.PauseWorkflow, req)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) ResumeWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.ResumeWorkflowRequest{WorkflowId: workflowID}
	resp, err := invokePluginUnary(ctx, c.client.ResumeWorkflow, req)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) CancelWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.CancelWorkflowRequest{WorkflowId: workflowID}
	resp, err := invokePluginUnary(ctx, c.client.CancelWorkflow, req)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) GetWorkflowStatus(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.GetWorkflowStatusRequest{WorkflowId: workflowID}
	resp, err := invokePluginUnary(ctx, c.client.GetWorkflowStatus, req)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) NotifyStatusChange(ctx context.Context, sessionID string, displayStatus bossanovav1.DisplayStatus, hasFailures bool) error {
	req := &bossanovav1.NotifyStatusChangeRequest{
		SessionId:     sessionID,
		DisplayStatus: displayStatus,
		HasFailures:   hasFailures,
	}
	_, err := invokePluginUnary(ctx, c.client.NotifyStatusChange, req)
	return err
}

// Compile-time interface checks.
var (
	_ TaskSource      = (*taskSourceGRPCClient)(nil)
	_ WorkflowService = (*workflowServiceGRPCClient)(nil)
)

// NewTaskSourceClient adapts a generated client to the daemon consumer interface.
func NewTaskSourceClient(client bossanovav1.TaskSourceServiceClient) TaskSource {
	return &taskSourceGRPCClient{client: client}
}
