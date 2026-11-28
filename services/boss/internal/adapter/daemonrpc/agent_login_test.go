package daemonrpc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/domain/rpc"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type checkAgentLoginsRPC struct {
	bossanovav1connect.DaemonServiceClient
	resp *pb.CheckAgentLoginsResponse
	err  error
}

func (f *checkAgentLoginsRPC) CheckAgentLogins(
	_ context.Context,
	_ *connect.Request[pb.CheckAgentLoginsRequest],
) (*connect.Response[pb.CheckAgentLoginsResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.resp), nil
}

func TestLocalClientCheckAgentLogins(t *testing.T) {
	t.Parallel()

	f := &checkAgentLoginsRPC{
		resp: &pb.CheckAgentLoginsResponse{
			Agents: []*pb.AgentLogin{
				{
					AgentName: "claude",
					Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
				},
				{
					AgentName: "codex",
					Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
					Remedy:    "run codex login",
				},
				{
					AgentName:         "gemini",
					Status:            pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
					HasManagedAccount: true,
				},
				{
					AgentName: "opencode",
					Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
				},
				{
					AgentName: "zebra",
					Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNSPECIFIED,
				},
			},
		},
	}
	c := &LocalClient{rpc: f}

	got, err := c.CheckAgentLogins(context.Background())
	if err != nil {
		t.Fatalf("CheckAgentLogins: %v", err)
	}
	want := []rpc.AgentLogin{
		{AgentName: "claude", LoggedOut: false},
		{AgentName: "codex", LoggedOut: true, Remedy: "run codex login"},
		{AgentName: "gemini", LoggedOut: false},
		{AgentName: "opencode", LoggedOut: false},
		{AgentName: "zebra", LoggedOut: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CheckAgentLogins = %+v, want %+v", got, want)
	}

	f.err = connect.NewError(connect.CodeUnimplemented, errors.New("older bossd"))
	if _, err := c.CheckAgentLogins(context.Background()); rpcerr.KindOf(err) != rpcerr.Unimplemented {
		t.Fatalf("err = %v (kind=%v), want Unimplemented", err, rpcerr.KindOf(err))
	}
}
