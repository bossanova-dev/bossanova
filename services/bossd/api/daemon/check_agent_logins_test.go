package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/plugin"
)

func TestCheckAgentLoginsMapsEachState_DaemonLifecycle_AC274701(t *testing.T) {
	srv, accts := newAccountServer(t, newFakeCredStore(), nil)
	mustAddClaude(t, srv, "work", []byte("blob"))
	srv.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	managedProbe := &loginProbeRunnerClient{
		resp: &pb.CheckLoginResponse{
			Status: pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: "should not be probed",
		},
	}
	srv.agentClients = map[string]agent.RunnerClient{
		"zebra": &loginProbeRunnerClient{
			err: grpcstatus.Error(codes.Unavailable, "secret-backend-diagnostic-xyz"),
		},
		"opencode": &loginProbeRunnerClient{
			resp: &pb.CheckLoginResponse{
				Status: pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
			},
		},
		"codex": &loginProbeRunnerClient{
			resp: &pb.CheckLoginResponse{
				Status: pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
				Remedy: "run `codex login`",
			},
		},
		"gemini": &loginProbeRunnerClient{
			resp: &pb.CheckLoginResponse{
				Status: pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
				Remedy: "",
			},
		},
		"claude": managedProbe,
		"amp": &loginProbeRunnerClient{
			resp: &pb.CheckLoginResponse{
				Status: pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
			},
		},
	}

	resp, err := srv.CheckAgentLogins(context.Background(), connect.NewRequest(&pb.CheckAgentLoginsRequest{}))
	if err != nil {
		t.Fatalf("CheckAgentLogins: %v", err)
	}
	if got := managedProbe.calls.Load(); got != 0 {
		t.Fatalf("managed agent CheckLogin calls = %d, want 0", got)
	}

	want := &pb.CheckAgentLoginsResponse{
		Agents: []*pb.AgentLogin{
			{
				AgentName: "amp",
				Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
			},
			{
				AgentName:         "claude",
				Status:            pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
				HasManagedAccount: true,
			},
			{
				AgentName: "codex",
				Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
				Remedy:    "run `codex login`",
			},
			{
				AgentName: "gemini",
				Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
				Remedy:    "log in to the gemini CLI",
			},
			{
				AgentName: "opencode",
				Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
			},
			{
				AgentName: "zebra",
				Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
			},
		},
	}

	if !proto.Equal(resp.Msg, want) {
		t.Fatalf("CheckAgentLogins response mismatch:\ngot:  %v\nwant: %v", resp.Msg, want)
	}
}

func TestCheckAgentLoginsNoAgentsLoaded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		srv  *Server
	}{
		{name: "nil plugin host", srv: &Server{logger: zerolog.Nop()}},
		{name: "empty plugin host", srv: &Server{logger: zerolog.Nop(), pluginHost: &plugin.Host{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := tc.srv.CheckAgentLogins(context.Background(), connect.NewRequest(&pb.CheckAgentLoginsRequest{}))
			if err != nil {
				t.Fatalf("CheckAgentLogins: %v", err)
			}
			want := &pb.CheckAgentLoginsResponse{}
			if !proto.Equal(resp.Msg, want) {
				t.Fatalf("got %v, want %v", resp.Msg, want)
			}
			if len(resp.Msg.GetAgents()) != 0 {
				t.Fatalf("len(Agents) = %d, want 0", len(resp.Msg.GetAgents()))
			}
		})
	}
}
