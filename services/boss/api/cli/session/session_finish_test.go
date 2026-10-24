package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type finishClient struct {
	Client
	calls int
	req   *pb.FinishUnattendedRunRequest
	resp  *pb.FinishUnattendedRunResponse
	err   error
}

func (c *finishClient) FinishUnattendedRun(_ context.Context, req *pb.FinishUnattendedRunRequest) (*pb.FinishUnattendedRunResponse, error) {
	c.calls++
	c.req = req
	if c.err != nil {
		return nil, c.err
	}
	return c.resp, nil
}

func TestSessionFinishCommand(t *testing.T) {
	pending := &pb.FinishUnattendedRunResponse{IsDeferred: true, BlockingAgentSessionId: "chat-env"}
	envIDs := map[string]string{"BOSS_SESSION_ID": "session-env", "BOSS_AGENT_SESSION_ID": "chat-env"}
	for _, tt := range []struct {
		name                  string
		args                  []string
		env                   map[string]string
		resp                  *pb.FinishUnattendedRunResponse
		daemonErr             error
		wantSession, wantChat string
		wantBlocked           bool
		wantReason            string
		wantOutput, wantErr   string
		wantJSON              *sessionFinishJSON
	}{
		{name: "blocked with reason", args: []string{"--blocked", "--reason", "  unresolved P1  "}, env: envIDs, resp: pending, wantSession: "session-env", wantChat: "chat-env", wantBlocked: true, wantReason: "unresolved P1", wantOutput: "finish pending (waiting on chat-env)\n"},
		{name: "blocked without reason", args: []string{"--blocked"}, env: envIDs, resp: pending, wantSession: "session-env", wantChat: "chat-env", wantBlocked: true, wantOutput: "finish pending (waiting on chat-env)\n"},
		{name: "reason without blocked", args: []string{"--reason", "why"}, env: envIDs, wantErr: "--reason is only used with --blocked"},
		{name: "environment defaults", env: envIDs, resp: pending, wantSession: "session-env", wantChat: "chat-env", wantOutput: "finish pending (waiting on chat-env)\n"},
		{name: "flags override", args: []string{"--session", "session-flag", "--chat", "chat-flag"}, env: envIDs, resp: &pb.FinishUnattendedRunResponse{IsDeferred: true, BlockingAgentSessionId: "chat-flag"}, wantSession: "session-flag", wantChat: "chat-flag", wantOutput: "finish pending (waiting on chat-flag)\n"},
		{name: "finished synchronously", env: envIDs, resp: &pb.FinishUnattendedRunResponse{}, wantSession: "session-env", wantChat: "chat-env", wantOutput: "finished\n"},
		{name: "json pending", args: []string{"--json"}, env: envIDs, resp: pending, wantSession: "session-env", wantChat: "chat-env", wantJSON: &sessionFinishJSON{Outcome: "pending", BlockingAgentSessionID: "chat-env"}},
		{name: "json finished", args: []string{"--json"}, env: envIDs, resp: &pb.FinishUnattendedRunResponse{}, wantSession: "session-env", wantChat: "chat-env", wantJSON: &sessionFinishJSON{Outcome: "finished"}},
		{name: "missing session", env: map[string]string{"BOSS_AGENT_SESSION_ID": "chat-env"}, wantErr: "--session or BOSS_SESSION_ID"},
		{name: "missing chat", env: map[string]string{"BOSS_SESSION_ID": "session-env"}, wantErr: "--chat or BOSS_AGENT_SESSION_ID"},
		{name: "blank flag does not fall back to env", args: []string{"--chat", " "}, env: envIDs, wantErr: "--chat or BOSS_AGENT_SESSION_ID"},
		{name: "positional args refused", args: []string{"extra"}, env: envIDs, wantErr: "unknown command"},
		{name: "daemon error", env: envIDs, daemonErr: errors.New("permission denied: not a chat of the session"), wantSession: "session-env", wantChat: "chat-env", wantErr: "permission denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubEnv(t, tt.env)
			c := &finishClient{resp: tt.resp, err: tt.daemonErr}
			factories := 0
			cmd := New(func(*cobra.Command) (Client, error) { factories++; return c, nil }, Options{}).sessionFinishCmd()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if tt.wantErr != "" && tt.daemonErr == nil {
				wantCalls = 0
			}
			if c.calls != wantCalls || factories != wantCalls {
				t.Fatalf("calls=%d factories=%d, want %d", c.calls, factories, wantCalls)
			}
			if wantCalls == 1 && (c.req.GetSessionId() != tt.wantSession || c.req.GetRequesterAgentSessionId() != tt.wantChat) {
				t.Fatalf("request = (%q,%q), want (%q,%q)", c.req.GetSessionId(), c.req.GetRequesterAgentSessionId(), tt.wantSession, tt.wantChat)
			}
			if wantCalls == 1 && (c.req.GetIsBlocked() != tt.wantBlocked || c.req.GetBlockedReason() != tt.wantReason) {
				t.Fatalf("blocked = (%v,%q), want (%v,%q)", c.req.GetIsBlocked(), c.req.GetBlockedReason(), tt.wantBlocked, tt.wantReason)
			}
			if tt.wantJSON != nil {
				var got map[string]any
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
				}
				if got["outcome"] != tt.wantJSON.Outcome || got["blocking_agent_session_id"] != tt.wantJSON.BlockingAgentSessionID {
					t.Fatalf("json = %v, want %+v", got, *tt.wantJSON)
				}
				if len(got) != 2 {
					t.Fatalf("json keys = %v, want exactly outcome and blocking_agent_session_id", got)
				}
				return
			}
			if tt.wantErr == "" && out.String() != tt.wantOutput {
				t.Fatalf("output = %q, want %q", out.String(), tt.wantOutput)
			}
		})
	}
}

func TestSessionFinishRegistered(t *testing.T) {
	cmd, _, err := sessionCmd().Find([]string{"finish"})
	if err != nil || cmd.Name() != "finish" {
		t.Fatalf("finish command missing: %v", err)
	}
}

// TestSessionFinishRemoteClientIsLocalOnly: finishing a run is a local-daemon
// operation; a remote client refuses it rather than reaching bosso.
func TestSessionFinishRemoteClientIsLocalOnly(t *testing.T) {
	var rc cloudrpc.RemoteClient
	_, err := rc.FinishUnattendedRun(context.Background(), &pb.FinishUnattendedRunRequest{SessionId: "s", RequesterAgentSessionId: "c"})
	if connect.CodeOf(err) != connect.CodeUnimplemented || !strings.Contains(err.Error(), "finish unattended run") {
		t.Fatalf("RemoteClient.FinishUnattendedRun err = %v, want a local-only error", err)
	}
}
