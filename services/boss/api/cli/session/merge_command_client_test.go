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

type mergeCommandClient struct {
	Client
	session          *pb.Session
	responseState    pb.SessionState
	detail           string
	err              error
	heads            []string
	operatorHeads    []string
	operatorApproves []bool
	operatorWaives   [][]string
	operatorResponse *pb.OperatorMergeSessionResponse
	reads            int
}

func (c *mergeCommandClient) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return []*pb.Session{c.session}, nil
}
func (c *mergeCommandClient) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	c.reads++
	return c.session, nil
}
func (c *mergeCommandClient) MergeSession(_ context.Context, id, head string) (*pb.Session, string, error) {
	c.heads = append(c.heads, head)
	if c.err != nil {
		return nil, "", c.err
	}
	c.session.State = pb.SessionState_SESSION_STATE_MERGED
	response := &pb.Session{Id: id, Title: c.session.Title, State: c.responseState}
	return response, c.detail, nil
}
func (c *mergeCommandClient) OperatorMergeSession(_ context.Context, _ string, head string, approve bool, waives []string) (*pb.OperatorMergeSessionResponse, error) {
	c.operatorHeads = append(c.operatorHeads, head)
	c.operatorApproves = append(c.operatorApproves, approve)
	c.operatorWaives = append(c.operatorWaives, waives)
	if c.operatorResponse != nil || c.err != nil {
		return c.operatorResponse, c.err
	}
	c.session.State = pb.SessionState_SESSION_STATE_MERGED
	return &pb.OperatorMergeSessionResponse{IsMerged: true, Session: c.session, MergeDetail: c.detail}, nil
}
func (c *mergeCommandClient) SetMergeDetail(detail string) { c.detail = detail }
func (c *mergeCommandClient) SetMergeError(code connect.Code, message string) {
	c.err = rpcerror.WrapError(connect.NewError(code, errors.New(message)))
}
func (c *mergeCommandClient) SetMergeResponseState(state pb.SessionState) { c.responseState = state }
func (c *mergeCommandClient) MergeSessionExpectedHeads() []string         { return c.heads }
func (c *mergeCommandClient) MergeSessionCalls() []string {
	var ids []string
	for range c.heads {
		ids = append(ids, c.session.Id)
	}
	return ids
}

type mergeCommandDriver struct {
	t      *testing.T
	client *mergeCommandClient
}

func mergeCommandHarness(t *testing.T) *mergeCommandDriver {
	return &mergeCommandDriver{t: t, client: &mergeCommandClient{session: &pb.Session{Id: "sess-aaa-111", Title: "Add dark mode", State: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN}, responseState: pb.SessionState_SESSION_STATE_MERGED}}
}

type mergeCommandResult struct {
	Stdout, Stderr string
	ExitCode       int
}

func (h *mergeCommandDriver) Run(args ...string) mergeCommandResult {
	cmd := New(func(*cobra.Command) (Client, error) { return h.client, nil }, Options{}).MergeCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.Flags().String("remote", "", "Remote daemon")
	cmd.SetArgs(args[1:])
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var runErr error
	printed := captureStdout(h.t, func() { runErr = cmd.Execute() })
	result := mergeCommandResult{Stdout: printed + out.String(), Stderr: stderr.String()}
	if runErr != nil {
		result.ExitCode = 1
		result.Stderr += runErr.Error()
	}
	return result
}

var matchHeadPin = "0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b"
