package chat

import (
	"bytes"
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type chatActionClient struct {
	Client
	records        []*pb.RecordChatRequest
	sends          []*pb.SendChatMessageRequest
	recordResponse *pb.AgentChat
	status         *pb.ChatStatusEntry
	transcript     *pb.GetChatTranscriptResponse
}

func (c *chatActionClient) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return []*pb.Session{{Id: "sess-aaa-111"}, {Id: "sess-bbb-222"}, {Id: waitSessionID}}, nil
}
func (c *chatActionClient) GetSession(_ context.Context, id string, _ rpc.SessionReadOptions) (*pb.Session, error) {
	if id == waitSessionID {
		chat := waitChatID
		return &pb.Session{Id: id, AgentSessionId: &chat}, nil
	}
	return nil, rpcerror.WrapError(connect.NewError(connect.CodeNotFound, errors.New("not found")))
}
func (c *chatActionClient) RecordChat(_ context.Context, sessionID, id, title, agent string, role pb.ChatRole, resume bool) (*pb.AgentChat, error) {
	c.records = append(c.records, &pb.RecordChatRequest{SessionId: sessionID, AgentSessionId: id, Title: title, AgentName: &agent, Role: role, Resume: resume})
	if c.recordResponse != nil {
		return c.recordResponse, nil
	}
	return &pb.AgentChat{SessionId: sessionID, AgentSessionId: id, Title: title, TmuxSessionName: "test-chat"}, nil
}
func (c *chatActionClient) SendChatMessage(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	c.sends = append(c.sends, req)
	return &pb.SendChatMessageResponse{Delivered: true}, nil
}
func (c *chatActionClient) GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	return c.transcript, nil
}
func (c *chatActionClient) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return []*pb.ChatStatusEntry{c.status}, nil
}
func (c *chatActionClient) SetRecordChatResponse(chat *pb.AgentChat)             { c.recordResponse = chat }
func (c *chatActionClient) RecordChatCalls() []*pb.RecordChatRequest             { return c.records }
func (c *chatActionClient) SendChatMessageCalls() []*pb.SendChatMessageRequest   { return c.sends }
func (c *chatActionClient) SetChatTranscript(resp *pb.GetChatTranscriptResponse) { c.transcript = resp }

type chatActionHarness struct{ client *chatActionClient }
type chatActionResult struct {
	Stdout, Stderr string
	ExitCode       int
}

func (h *chatActionHarness) Run(args ...string) chatActionResult {
	cmd := New(func(*cobra.Command) (Client, error) { return h.client, nil })
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args[1:])
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	result := chatActionResult{Stdout: out.String(), Stderr: stderr.String()}
	if err != nil {
		result.ExitCode = 1
		result.Stderr += err.Error()
	}
	return result
}
