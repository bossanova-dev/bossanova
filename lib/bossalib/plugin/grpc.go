package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc"
)

func serveHost(broker *goplugin.GRPCBroker, id uint32, host bossanovav1.HostServiceServer) {
	if host == nil {
		return
	}
	go broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
		srv := grpc.NewServer(opts...)
		bossanovav1.RegisterHostServiceServer(srv, host)
		return srv
	})
}

// AgentRunnerPlugin connects a generated AgentRunnerService to the go-plugin transport.
type AgentRunnerPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	NewServer   func(*goplugin.GRPCBroker) bossanovav1.AgentRunnerServiceServer
	HostService bossanovav1.HostServiceServer
}

func (p *AgentRunnerPlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error {
	bossanovav1.RegisterAgentRunnerServiceServer(srv, p.NewServer(broker))
	return nil
}

func (p *AgentRunnerPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	serveHost(broker, BrokerIDAgentRunnerHostService, p.HostService)
	return bossanovav1.NewAgentRunnerServiceClient(conn), nil
}

// TaskSourcePlugin connects a generated TaskSourceService to the go-plugin transport.
type TaskSourcePlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	NewServer   func(*goplugin.GRPCBroker) bossanovav1.TaskSourceServiceServer
	HostService bossanovav1.HostServiceServer
}

func (p *TaskSourcePlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error {
	bossanovav1.RegisterTaskSourceServiceServer(srv, p.NewServer(broker))
	return nil
}

func (p *TaskSourcePlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	serveHost(broker, BrokerIDTaskSourceHostService, p.HostService)
	return bossanovav1.NewTaskSourceServiceClient(conn), nil
}

// WorkflowPlugin connects a generated WorkflowService to the go-plugin transport.
type WorkflowPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	NewServer   func(*goplugin.GRPCBroker) bossanovav1.WorkflowServiceServer
	HostService bossanovav1.HostServiceServer
}

func (p *WorkflowPlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error {
	bossanovav1.RegisterWorkflowServiceServer(srv, p.NewServer(broker))
	return nil
}

func (p *WorkflowPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	serveHost(broker, BrokerIDWorkflowHostService, p.HostService)
	return bossanovav1.NewWorkflowServiceClient(conn), nil
}
