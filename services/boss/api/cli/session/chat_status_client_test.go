package session

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type statusCommandClient struct {
	Client
	chats     []*pb.AgentChat
	statuses  []*pb.ChatStatusEntry
	statusErr error
}

func (c *statusCommandClient) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return []*pb.Session{{Id: chatsSessionID}, {Id: "sess-bbb-222"}}, nil
}
func (c *statusCommandClient) GetSession(_ context.Context, id string, _ rpc.SessionReadOptions) (*pb.Session, error) {
	return &pb.Session{Id: id, Title: "Session"}, nil
}
func (c *statusCommandClient) ListChats(_ context.Context, id string) ([]*pb.AgentChat, error) {
	if id != chatsSessionID {
		return nil, nil
	}
	return c.chats, nil
}
func (c *statusCommandClient) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return c.statuses, c.statusErr
}
func (c *statusCommandClient) AddChatStatus(status *pb.ChatStatusEntry) {
	for i, existing := range c.statuses {
		if existing.AgentSessionId == status.AgentSessionId {
			c.statuses[i] = status
			return
		}
	}
	c.statuses = append(c.statuses, status)
}
func (c *statusCommandClient) SetChatStatusesError(code connect.Code, message string) {
	c.statusErr = rpcerror.WrapError(connect.NewError(code, errors.New(message)))
}

type statusCommandHarness struct {
	t      *testing.T
	client *statusCommandClient
}
type statusCommandResult struct {
	Stdout, Stderr string
	ExitCode       int
}

func (h *statusCommandHarness) Run(args ...string) statusCommandResult {
	h.t.Helper()
	g := New(func(*cobra.Command) (Client, error) { return h.client, nil }, Options{})
	var cmd *cobra.Command
	if args[0] == "show" {
		cmd = g.ShowCmd()
	} else {
		cmd = g.ChatsCmd()
	}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args[1:])
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var runErr error
	printed := captureStdout(h.t, func() { runErr = cmd.Execute() })
	result := statusCommandResult{Stdout: printed + out.String(), Stderr: errOut.String()}
	if runErr != nil {
		result.ExitCode = 1
	}
	return result
}
