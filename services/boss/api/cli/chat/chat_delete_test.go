package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// chatDeleteRecorder is a daemon that knows one session and records every
// chat delete that reaches it, which is the only observable effect of
// `boss chat delete`: DeleteChatResponse is an empty message.
type chatDeleteRecorder struct {
	Client
	deletes   []string
	reasons   []pb.DeleteChatRequest_DeletionReason
	deleteErr error
}

const deleteSessionID = "sess-delete-1"

func (c *chatDeleteRecorder) GetSession(_ context.Context, id string, _ rpc.SessionReadOptions) (*pb.Session, error) {
	if id == deleteSessionID {
		primary := "primary-chat"
		return &pb.Session{Id: id, AgentSessionId: &primary}, nil
	}
	return nil, rpcerror.WrapError(connect.NewError(connect.CodeNotFound, errors.New("not found")))
}

func (c *chatDeleteRecorder) DeleteChat(_ context.Context, agentSessionID string, reason pb.DeleteChatRequest_DeletionReason) error {
	c.deletes = append(c.deletes, agentSessionID)
	c.reasons = append(c.reasons, reason)
	return c.deleteErr
}

func runChatDelete(t *testing.T, c *chatDeleteRecorder, args ...string) (string, error) {
	t.Helper()
	cmd := New(func(*cobra.Command) (Client, error) { return c, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"delete"}, args...))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

func TestChatDeleteDeletesTheNamedChat(t *testing.T) {
	c := &chatDeleteRecorder{}
	out, err := runChatDelete(t, c, "question-chat", "--json")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(c.deletes) != 1 || c.deletes[0] != "question-chat" {
		t.Fatalf("deletes = %v, want [question-chat]", c.deletes)
	}
	// A CLI delete is a direct request, never the cleanup reason the daemon gates.
	if c.reasons[0] != pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED {
		t.Fatalf("reason = %v, want USER_REQUESTED", c.reasons[0])
	}
	var env struct {
		AgentSessionID string `json:"agent_session_id"`
		Deleted        bool   `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("--json output %q: %v", out, err)
	}
	if env.AgentSessionID != "question-chat" || !env.Deleted {
		t.Fatalf("envelope = %+v, want the deleted chat", env)
	}
}

// Every other chat verb resolves a session id to its primary chat. Delete must
// not: one wrong id would stop the session's own agent.
func TestChatDeleteRefusesASessionID(t *testing.T) {
	c := &chatDeleteRecorder{}
	_, err := runChatDelete(t, c, deleteSessionID)
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("err = %v, want a refusal naming the session", err)
	}
	if len(c.deletes) != 0 {
		t.Fatalf("deletes = %v, want none", c.deletes)
	}
}

func TestChatDeleteReportsADaemonFailure(t *testing.T) {
	c := &chatDeleteRecorder{deleteErr: errors.New("daemon unreachable")}
	out, err := runChatDelete(t, c, "question-chat", "--json")
	if err == nil {
		t.Fatal("want the daemon's failure")
	}
	if !strings.Contains(out, "daemon unreachable") {
		t.Fatalf("--json failure envelope %q does not carry the cause", out)
	}
}
