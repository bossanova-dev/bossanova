package plugin

import (
	"context"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestSharedGRPCPlugins(t *testing.T) {
	for _, kind := range []string{"agent-runner", "task-source", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			brokers := make(chan *goplugin.GRPCBroker, 1)
			host := &grpcTestHost{}
			var p goplugin.Plugin
			var hostID uint32
			switch kind {
			case "agent-runner":
				hostID = BrokerIDAgentRunnerHostService
				p = &AgentRunnerPlugin{HostService: host, NewServer: func(b *goplugin.GRPCBroker) bossanovav1.AgentRunnerServiceServer {
					brokers <- b
					return &grpcTestAgentRunner{}
				}}
			case "task-source":
				hostID = BrokerIDTaskSourceHostService
				p = &TaskSourcePlugin{HostService: host, NewServer: func(b *goplugin.GRPCBroker) bossanovav1.TaskSourceServiceServer {
					brokers <- b
					return &grpcTestTaskSource{}
				}}
			case "workflow":
				hostID = BrokerIDWorkflowHostService
				p = &WorkflowPlugin{HostService: host, NewServer: func(b *goplugin.GRPCBroker) bossanovav1.WorkflowServiceServer {
					brokers <- b
					return &grpcTestWorkflow{}
				}}
			}
			client, _ := goplugin.TestPluginGRPCConn(t, false, map[string]goplugin.Plugin{"test": p})
			t.Cleanup(func() { _ = client.Close() })
			raw, err := client.Dispense("test")
			if err != nil {
				t.Fatalf("Dispense: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var info *bossanovav1.PluginInfo
			switch kind {
			case "agent-runner":
				generated, ok := raw.(bossanovav1.AgentRunnerServiceClient)
				if !ok {
					t.Fatalf("Dispense returned %T, want generated AgentRunnerServiceClient", raw)
				}
				response, err := generated.GetInfo(ctx, &bossanovav1.AgentRunnerServiceGetInfoRequest{})
				if err != nil {
					t.Fatalf("GetInfo: %v", err)
				}
				info = response.GetInfo()
			case "task-source":
				generated, ok := raw.(bossanovav1.TaskSourceServiceClient)
				if !ok {
					t.Fatalf("Dispense returned %T, want generated TaskSourceServiceClient", raw)
				}
				response, err := generated.GetInfo(ctx, &bossanovav1.TaskSourceServiceGetInfoRequest{})
				if err != nil {
					t.Fatalf("GetInfo: %v", err)
				}
				info = response.GetInfo()
			case "workflow":
				generated, ok := raw.(bossanovav1.WorkflowServiceClient)
				if !ok {
					t.Fatalf("Dispense returned %T, want generated WorkflowServiceClient", raw)
				}
				response, err := generated.GetInfo(ctx, &bossanovav1.WorkflowServiceGetInfoRequest{})
				if err != nil {
					t.Fatalf("GetInfo: %v", err)
				}
				info = response.GetInfo()
			}
			if info.GetName() != kind {
				t.Fatalf("GetInfo name = %q, want %q", info.GetName(), kind)
			}

			// Dial from the plugin back to the host on the kind's existing wire ID.
			var broker *goplugin.GRPCBroker
			select {
			case broker = <-brokers:
			case <-ctx.Done():
				t.Fatal("NewServer did not receive a broker")
			}
			conn, err := broker.Dial(hostID)
			if err != nil {
				t.Fatalf("dial HostService on broker ID %d: %v", hostID, err)
			}
			defer func() { _ = conn.Close() }()
			sessions, err := bossanovav1.NewHostServiceClient(conn).ListSessions(ctx, &bossanovav1.HostServiceListSessionsRequest{})
			if err != nil {
				t.Fatalf("HostService.ListSessions: %v", err)
			}
			if len(sessions.GetSessions()) != 1 || sessions.GetSessions()[0].GetId() != "host-round-trip" {
				t.Fatalf("HostService.ListSessions = %v, want host-round-trip session", sessions)
			}
		})
	}
}

type grpcTestHost struct {
	bossanovav1.UnimplementedHostServiceServer
}

func (*grpcTestHost) ListSessions(context.Context, *bossanovav1.HostServiceListSessionsRequest) (*bossanovav1.HostServiceListSessionsResponse, error) {
	return &bossanovav1.HostServiceListSessionsResponse{Sessions: []*bossanovav1.Session{{Id: "host-round-trip"}}}, nil
}

type grpcTestAgentRunner struct {
	bossanovav1.UnimplementedAgentRunnerServiceServer
}

func (*grpcTestAgentRunner) GetInfo(context.Context, *bossanovav1.AgentRunnerServiceGetInfoRequest) (*bossanovav1.AgentRunnerServiceGetInfoResponse, error) {
	return &bossanovav1.AgentRunnerServiceGetInfoResponse{Info: &bossanovav1.PluginInfo{Name: "agent-runner"}}, nil
}

type grpcTestTaskSource struct {
	bossanovav1.UnimplementedTaskSourceServiceServer
}

func (*grpcTestTaskSource) GetInfo(context.Context, *bossanovav1.TaskSourceServiceGetInfoRequest) (*bossanovav1.TaskSourceServiceGetInfoResponse, error) {
	return &bossanovav1.TaskSourceServiceGetInfoResponse{Info: &bossanovav1.PluginInfo{Name: "task-source"}}, nil
}

type grpcTestWorkflow struct {
	bossanovav1.UnimplementedWorkflowServiceServer
}

func (*grpcTestWorkflow) GetInfo(context.Context, *bossanovav1.WorkflowServiceGetInfoRequest) (*bossanovav1.WorkflowServiceGetInfoResponse, error) {
	return &bossanovav1.WorkflowServiceGetInfoResponse{Info: &bossanovav1.PluginInfo{Name: "workflow"}}, nil
}
