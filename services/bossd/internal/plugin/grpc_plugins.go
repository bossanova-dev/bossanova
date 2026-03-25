package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Host-side interfaces ---
// These define what the host can call on each plugin type.

// TaskSource is the host-side interface for TaskSourceService plugins.
type TaskSource interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	PollTasks(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error)
	UpdateTaskStatus(ctx context.Context, externalID string, status bossanovav1.TaskItemStatus, details string) error
}

// EventSource is the host-side interface for EventSourceService plugins.
type EventSource interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
}

// Scheduler is the host-side interface for SchedulerService plugins.
type Scheduler interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	GetSchedule(ctx context.Context) ([]*bossanovav1.ScheduledJob, error)
	ExecuteJob(ctx context.Context, jobID string) (*bossanovav1.JobAction, error)
}

// WorkflowService is the host-side interface for WorkflowService plugins.
type WorkflowService interface {
	GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error)
	StartWorkflow(ctx context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error)
	PauseWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	ResumeWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	CancelWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
	GetWorkflowStatus(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
}

// --- GRPCPlugin implementations ---

// TaskSourceGRPCPlugin implements go-plugin's GRPCPlugin interface for
// the TaskSourceService. When HostService is set, GRPCClient registers it
// on broker ID 1 so the plugin subprocess can call back to the host.
type TaskSourceGRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	HostService *HostServiceServer
}

func (p *TaskSourceGRPCPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return nil
}

func (p *TaskSourceGRPCPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	if p.HostService != nil {
		serverFunc := func(opts []grpc.ServerOption) *grpc.Server {
			srv := grpc.NewServer(opts...)
			p.HostService.Register(srv)
			return srv
		}
		go broker.AcceptAndServe(1, serverFunc)
	}
	return &taskSourceGRPCClient{conn: conn}, nil
}

// NewTaskSourceGRPCPlugin creates a TaskSourceGRPCPlugin with the given
// HostService. Exported for integration testing.
func NewTaskSourceGRPCPlugin(hostService *HostServiceServer) *TaskSourceGRPCPlugin {
	return &TaskSourceGRPCPlugin{HostService: hostService}
}

// EventSourceGRPCPlugin implements go-plugin's GRPCPlugin interface for
// the EventSourceService.
type EventSourceGRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *EventSourceGRPCPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return nil
}

func (p *EventSourceGRPCPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	return &eventSourceGRPCClient{conn: conn}, nil
}

// SchedulerGRPCPlugin implements go-plugin's GRPCPlugin interface for
// the SchedulerService.
type SchedulerGRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *SchedulerGRPCPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return nil
}

func (p *SchedulerGRPCPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	return &schedulerGRPCClient{conn: conn}, nil
}

// WorkflowServiceGRPCPlugin implements go-plugin's GRPCPlugin interface for
// the WorkflowService. When HostService is set, GRPCClient registers it
// on broker ID 1 so the plugin subprocess can call back to the host.
type WorkflowServiceGRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	HostService *HostServiceServer
}

func (p *WorkflowServiceGRPCPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return nil
}

func (p *WorkflowServiceGRPCPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	if p.HostService != nil {
		serverFunc := func(opts []grpc.ServerOption) *grpc.Server {
			srv := grpc.NewServer(opts...)
			p.HostService.Register(srv)
			return srv
		}
		go broker.AcceptAndServe(1, serverFunc)
	}
	return &workflowServiceGRPCClient{conn: conn}, nil
}

// --- gRPC client implementations ---
// These use grpc.ClientConn.Invoke to call the plugin subprocess
// using the protobuf service method paths.

type taskSourceGRPCClient struct {
	conn *grpc.ClientConn
}

func (c *taskSourceGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp := &bossanovav1.TaskSourceServiceGetInfoResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/GetInfo", &bossanovav1.TaskSourceServiceGetInfoRequest{}, resp)
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
	resp := &bossanovav1.PollTasksResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/PollTasks", req, resp)
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
	resp := &bossanovav1.UpdateTaskStatusResponse{}
	return c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/UpdateTaskStatus", req, resp)
}

type eventSourceGRPCClient struct {
	conn *grpc.ClientConn
}

func (c *eventSourceGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp := &bossanovav1.EventSourceServiceGetInfoResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.EventSourceService/GetInfo", &bossanovav1.EventSourceServiceGetInfoRequest{}, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

type schedulerGRPCClient struct {
	conn *grpc.ClientConn
}

func (c *schedulerGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp := &bossanovav1.SchedulerServiceGetInfoResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.SchedulerService/GetInfo", &bossanovav1.SchedulerServiceGetInfoRequest{}, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *schedulerGRPCClient) GetSchedule(ctx context.Context) ([]*bossanovav1.ScheduledJob, error) {
	resp := &bossanovav1.GetScheduleResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.SchedulerService/GetSchedule", &bossanovav1.GetScheduleRequest{}, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetJobs(), nil
}

func (c *schedulerGRPCClient) ExecuteJob(ctx context.Context, jobID string) (*bossanovav1.JobAction, error) {
	req := &bossanovav1.ExecuteJobRequest{JobId: jobID}
	resp := &bossanovav1.ExecuteJobResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.SchedulerService/ExecuteJob", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetAction(), nil
}

type workflowServiceGRPCClient struct {
	conn *grpc.ClientConn
}

func (c *workflowServiceGRPCClient) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp := &bossanovav1.WorkflowServiceGetInfoResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.WorkflowService/GetInfo", &bossanovav1.WorkflowServiceGetInfoRequest{}, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *workflowServiceGRPCClient) StartWorkflow(ctx context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error) {
	resp := &bossanovav1.StartWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.WorkflowService/StartWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *workflowServiceGRPCClient) PauseWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.PauseWorkflowRequest{WorkflowId: workflowID}
	resp := &bossanovav1.PauseWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.WorkflowService/PauseWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) ResumeWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.ResumeWorkflowRequest{WorkflowId: workflowID}
	resp := &bossanovav1.ResumeWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.WorkflowService/ResumeWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) CancelWorkflow(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.CancelWorkflowRequest{WorkflowId: workflowID}
	resp := &bossanovav1.CancelWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.WorkflowService/CancelWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *workflowServiceGRPCClient) GetWorkflowStatus(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error) {
	req := &bossanovav1.GetWorkflowStatusRequest{WorkflowId: workflowID}
	resp := &bossanovav1.GetWorkflowStatusResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.WorkflowService/GetWorkflowStatus", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

// Compile-time interface checks.
var (
	_ TaskSource      = (*taskSourceGRPCClient)(nil)
	_ EventSource     = (*eventSourceGRPCClient)(nil)
	_ Scheduler       = (*schedulerGRPCClient)(nil)
	_ WorkflowService = (*workflowServiceGRPCClient)(nil)
)
