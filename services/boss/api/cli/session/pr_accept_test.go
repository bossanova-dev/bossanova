package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/adapter/cloudrpc"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpc"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type acceptCallerFake struct {
	Client
	*prFake
	req    *pb.AcceptSessionPRBlockerRequest
	result *pb.AcceptSessionPRBlockerResponse
	err    error
}

func (f *acceptCallerFake) ApproveSessionPR(ctx context.Context, id string, dry bool) (*pb.VerifyApproveResult, error) {
	return f.prFake.ApproveSessionPR(ctx, id, dry)
}
func (f *acceptCallerFake) ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts rpc.SessionReadOptions) ([]*pb.Session, error) {
	return f.prFake.ListSessions(ctx, req, opts)
}
func (f *acceptCallerFake) AcceptSessionPRBlocker(_ context.Context, req *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	f.req = req
	return f.result, f.err
}

func TestPrAccept_PinsDisplayedHead_Sessions_AC213301(t *testing.T) {
	for _, mode := range []string{"text", "json", "dry-run", "dry-json", "refusal", "refusal-json", "timeout", "dry-timeout", "unknown-head", "empty-reason", "older-daemon"} {
		t.Run(mode, func(t *testing.T) {
			s := prSession("s-a", 7, originA, "/wt/a")
			s.PrDisplayHeadSha = "abcdef0123456789abcdef0123456789abcdef01"
			f := &acceptCallerFake{prFake: &prFake{sessions: []*pb.Session{s}}, result: &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, HeadSha: s.PrDisplayHeadSha, BlockerId: "review:012345abcdef", TicketIdentifier: "TASK-1", TicketUrl: "https://ticket/1", CommentUrl: "https://comment/1"}}
			reason := "  exact reason\nwith detail  "
			if mode == "unknown-head" {
				s.PrDisplayHeadSha = ""
			}
			if mode == "empty-reason" {
				reason = " "
			}
			if strings.HasPrefix(mode, "refusal") {
				f.result = &pb.AcceptSessionPRBlockerResponse{Reason: "repair-active"}
			}
			if strings.Contains(mode, "timeout") {
				f.err = rpcerror.WrapError(connect.NewError(connect.CodeDeadlineExceeded, errors.New("timeout")))
			}
			if mode == "older-daemon" {
				f.err = rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, errors.New("unknown procedure")))
			}
			dry := strings.HasPrefix(mode, "dry")
			if mode == "dry-run" || mode == "dry-json" {
				f.result = &pb.AcceptSessionPRBlockerResponse{IsDryRun: true, PlannedWrites: []string{"create ticket", "post record"}}
			}
			g := New(func(*cobra.Command) (Client, error) { return f, nil }, Options{Getenv: func(string) string { return "" }})
			cmd := g.PRCmd()
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			var out strings.Builder
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			args := []string{"accept", "7", "review:012345abcdef", "--reason", reason}
			if strings.Contains(mode, "json") {
				args = append(args, "--json")
			}
			if dry {
				args = append(args, "--dry-run")
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			switch mode {
			case "empty-reason", "unknown-head":
				if err == nil || f.req != nil {
					t.Fatalf("err=%v request=%v", err, f.req)
				}
				if mode == "unknown-head" && rpcerr.KindOf(err) != rpcerr.FailedPrecondition {
					t.Fatalf("err=%v", err)
				}
				if mode == "empty-reason" && cliutil.ErrorCodeFor(err) != cliutil.CodeInvalidArgument {
					t.Fatalf("err=%v", err)
				}
			case "refusal", "refusal-json":
				if err == nil || !strings.Contains(err.Error(), "repair") {
					t.Fatalf("err=%v", err)
				}
				if mode == "refusal-json" {
					var env struct {
						Result map[string]any `json:"result"`
					}
					if e := json.Unmarshal([]byte(out.String()), &env); e != nil || env.Result["reason"] != "repair-active" || env.Result["accepted"] != false {
						t.Fatalf("json=%s err=%v", out.String(), e)
					}
				}
			case "timeout", "dry-timeout":
				if rpcerr.KindOf(err) != rpcerr.DeadlineExceeded || strings.Contains(err.Error(), "rerunning is safe") == dry {
					t.Fatalf("err=%v", err)
				}
			case "older-daemon":
				if err == nil || !strings.Contains(err.Error(), "upgrade") {
					t.Fatalf("err=%v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(mode, "json") {
					var env struct {
						PR        int            `json:"pr"`
						Repo      string         `json:"repo"`
						SessionID string         `json:"session_id"`
						Result    map[string]any `json:"result"`
					}
					if e := json.Unmarshal([]byte(out.String()), &env); e != nil || env.PR != 7 || env.Repo != "acme/app" || env.SessionID != "s-a" || env.Result["accepted"] != (mode == "json") {
						t.Fatalf("json=%s err=%v", out.String(), e)
					}
				} else if mode == "dry-run" || mode == "dry-json" {
					if !strings.Contains(out.String(), "create ticket") || !strings.Contains(out.String(), "post record") {
						t.Fatal(out.String())
					}
				} else if !strings.Contains(out.String(), "TASK-1") || !strings.Contains(out.String(), "https://comment/1") {
					t.Fatal(out.String())
				}
			}
			if f.req != nil && (f.req.GetId() != "s-a" || f.req.GetExpectedHeadSha() != s.GetPrDisplayHeadSha() || f.req.GetBlockerId() != "review:012345abcdef" || f.req.GetReason() != reason || f.req.GetIsDryRun() != dry) {
				t.Fatalf("request=%+v", f.req)
			}
		})
	}
}
func TestPrAccept_RefusedInAgentChatAndRemote_Sessions_AC213303(t *testing.T) {
	t.Setenv("BOSS_AGENT_SESSION_ID", "chat-123")
	f := &acceptCallerFake{prFake: &prFake{sessions: []*pb.Session{{Id: "s-a", PrNumber: ptrPR(7), PrDisplayHeadSha: strings.Repeat("a", 40)}}}, err: rpcerror.WrapError(connect.NewError(connect.CodePermissionDenied, errors.New("agent chat; run from your own terminal")))}
	g := New(func(*cobra.Command) (Client, error) { t.Fatal("agent chat built client"); return nil, nil }, Options{})
	cmd := g.PRCmd()
	cmd.SetArgs([]string{"accept", "7", "review:012345abcdef", "--reason", "follow up"})
	if err := cmd.Execute(); rpcerr.KindOf(err) != rpcerr.PermissionDenied {
		t.Fatalf("err=%v", err)
	}
	t.Setenv("BOSS_AGENT_SESSION_ID", "")
	g = New(func(*cobra.Command) (Client, error) { return f, nil }, Options{})
	cmd = g.PRCmd()
	cmd.SetArgs([]string{"accept", "7", "review:012345abcdef", "--reason", "follow up"})
	if err := cmd.Execute(); rpcerr.KindOf(err) != rpcerr.PermissionDenied || f.req == nil {
		t.Fatalf("daemon auth err=%v req=%v", err, f.req)
	}
	c := &cloudrpc.RemoteClient{}
	if _, err := c.AcceptSessionPRBlocker(context.Background(), &pb.AcceptSessionPRBlockerRequest{}); rpcerr.KindOf(err) != rpcerr.Unimplemented || !strings.Contains(err.Error(), "only available on a local daemon") {
		t.Fatalf("err=%v", err)
	}
	if _, err := c.ListSessionPRBlockers(context.Background(), "s-a"); rpcerr.KindOf(err) != rpcerr.Unimplemented {
		t.Fatalf("err=%v", err)
	}
}
func ptrPR(n int32) *int32 { return &n }

func TestPrAccept_InvalidUsageBuildsNoClient(t *testing.T) {
	for _, args := range [][]string{
		{"accept", "7", "review:012345abcdef"},
		{"accept", "7", "review:012345abcdef", "--reason", ""},
		{"accept", "7", "review:012345abcdef", "--reason", " \n\t"},
		{"accept", "7", "--reason", "reason"},
		{"accept", "7", "review:012345abcdef", "extra", "--reason", "reason"},
	} {
		g := New(func(*cobra.Command) (Client, error) { t.Fatal("invalid usage built client"); return nil, nil }, Options{Getenv: func(string) string { return "" }})
		cmd := g.PRCmd()
		cmd.SetArgs(args)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		if err := cmd.Execute(); err == nil {
			t.Fatalf("args=%v succeeded", args)
		}
	}
}

func TestAcceptResultJSONPreservesVerbFields(t *testing.T) {
	r := &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, IsAlreadyAccepted: true, IsExistingTicket: true, HeadSha: strings.Repeat("a", 40), BlockerId: "review:012345abcdef", TicketIdentifier: "TASK-1", TicketUrl: "https://tickets/1", CommentUrl: "https://comments/1"}
	encoded, err := json.Marshal(acceptResultJSON(r))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"accepted", "already", "existingTicket"} {
		if result[key] != true {
			t.Fatalf("field %s=%v", key, result[key])
		}
	}
	ticket, ok := result["ticket"].(map[string]any)
	if !ok || ticket["identifier"] != "TASK-1" || ticket["url"] != "https://tickets/1" || result["commentUrl"] != "https://comments/1" || result["head"] != r.HeadSha || result["blocker"] != r.BlockerId {
		t.Fatalf("json=%s", encoded)
	}
}
