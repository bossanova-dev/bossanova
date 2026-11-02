package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type prFollowUpFake struct {
	*prCallerFake
	result      *pb.VerifyFollowUpResult
	err         error
	ids         []string
	all, dryRun bool
}

func (f *prFollowUpFake) FollowUpSessionPRNotes(_ context.Context, id string, ids []string, all, dryRun bool) (*pb.VerifyFollowUpResult, error) {
	f.log = append(f.log, "follow-up:"+id)
	f.ids = ids
	f.all = all
	f.dryRun = dryRun
	return f.result, f.err
}
func newPRFollowUpFake() *prFollowUpFake {
	return &prFollowUpFake{prCallerFake: &prCallerFake{prFake: &prFake{sessions: []*pb.Session{prSession("s-a", 7, originA, "/wt/a")}}}}
}
func TestPrFollowUpArchivedSession(t *testing.T) {
	f := newPRFollowUpFake()
	f.sessions[0].ArchivedAt = timestamppb.Now()
	f.result = &pb.VerifyFollowUpResult{IsOk: true, Comment: "current"}
	cmd, _ := prTestCmd(false)
	if err := prFollowUp(t.Context(), cmd, f, prOptions{pr: 7}, nil, true); err != nil {
		t.Fatalf("follow up archived PR session: %v", err)
	}
	if !reflect.DeepEqual(f.log, []string{"list", "follow-up:s-a"}) {
		t.Fatalf("calls = %v", f.log)
	}
	if err := prApprove(t.Context(), cmd, f, prOptions{pr: 7}); cliutil.ErrorCodeFor(err) != cliutil.CodeNotFound {
		t.Fatalf("approve archived PR session: %v", err)
	}
	if len(f.verbs) != 0 {
		t.Fatalf("approved archived session: %v", f.verbs)
	}
}
func TestPrFollowUpOutput_Sessions_AC213801(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[asJSON], func(t *testing.T) {
			f := newPRFollowUpFake()
			f.result = &pb.VerifyFollowUpResult{Comment: "failed", Notes: []*pb.VerifyNoteFollowUp{
				{VerifyNoteId: "note:000000000001", Outcome: "created", TicketIdentifier: "T-1", TicketUrl: "https://x/1", IsLinked: true},
				{VerifyNoteId: "note:000000000002", Outcome: "already-filed", TicketIdentifier: "T-2", TicketUrl: "https://x/2", Reason: "patch failed"},
				{VerifyNoteId: "note:000000000003", Outcome: "indeterminate"},
				{VerifyNoteId: "note:000000000004", Outcome: "failed", Reason: "tracker down"},
			}}
			cmd, out := prTestCmd(asJSON)
			err := prFollowUp(t.Context(), cmd, f, prOptions{pr: 7, asJSON: asJSON}, nil, true)
			if err == nil {
				t.Fatal("partial result must fail")
			}
			if !reflect.DeepEqual(f.log, []string{"list", "follow-up:s-a"}) || !f.all {
				t.Fatalf("calls=%v all=%v", f.log, f.all)
			}
			if asJSON {
				decoder := json.NewDecoder(out)
				var body struct {
					PR      int    `json:"pr"`
					Repo    string `json:"repo"`
					Session string `json:"session_id"`
					Result  struct {
						OK      bool `json:"ok"`
						Results []struct {
							Note   string `json:"note"`
							Linked bool   `json:"linked"`
						} `json:"results"`
					} `json:"result"`
				}
				if e := decoder.Decode(&body); e != nil || body.PR != 7 || body.Repo != "acme/app" || body.Session != "s-a" || body.Result.OK || len(body.Result.Results) != 4 {
					t.Fatalf("envelope=%+v err=%v", body, e)
				}
				if e := decoder.Decode(new(any)); e != io.EOF {
					t.Fatalf("extra JSON: %v", e)
				}
			} else {
				want := "note:000000000001  created T-1 https://x/1\nnote:000000000002  already filed T-2 https://x/2 (not linked: patch failed)\nnote:000000000003  indeterminate: the tracker may have filed it; re-run to settle\nnote:000000000004  failed: tracker down\nnotes comment could not be updated\n"
				if out.String() != want {
					t.Fatalf("output=%q", out.String())
				}
			}
		})
	}
}
func TestPrFollowUpRefusalAndSuccessJSON_Sessions_AC213802(t *testing.T) {
	for _, r := range []*pb.VerifyFollowUpResult{
		{Reason: "unknown-note", UnknownVerifyNoteIds: []string{"note:000000000001"}},
		{IsOk: true, Comment: "current", Notes: []*pb.VerifyNoteFollowUp{{VerifyNoteId: "note:000000000001", Outcome: "already-filed", TicketIdentifier: "T-1", TicketUrl: "https://x/1", IsLinked: true}}},
		{IsOk: true, IsDryRun: true, Comment: "planned", Notes: []*pb.VerifyNoteFollowUp{{VerifyNoteId: "note:000000000001", Outcome: "planned"}}},
	} {
		for _, asJSON := range []bool{false, true} {
			f := newPRFollowUpFake()
			f.result = r
			cmd, out := prTestCmd(asJSON)
			err := prFollowUp(t.Context(), cmd, f, prOptions{pr: 7, asJSON: asJSON, dryRun: r.IsDryRun}, []string{"note:000000000001"}, false)
			if (err == nil) != r.IsOk {
				t.Fatalf("err=%v result=%v", err, r)
			}
			if len(f.ids) != 1 || f.dryRun != r.IsDryRun || f.all {
				t.Fatal("selection not forwarded")
			}
			if asJSON {
				var body map[string]any
				if e := json.Unmarshal(out.Bytes(), &body); e != nil {
					t.Fatal(e)
				}
				result := body["result"].(map[string]any)
				if result["ok"] != r.IsOk {
					t.Fatalf("result=%v", result)
				}
				if r.Reason != "" && (!strings.Contains(out.String(), "unknown-note") || !strings.Contains(out.String(), "note:000000000001")) {
					t.Fatal(out.String())
				}
			} else if r.IsDryRun && !strings.Contains(out.String(), "would file\nnotes comment would be updated") {
				t.Fatal(out.String())
			}
		}
	}
}

func TestPrFollowUpPartialFailureKeepsCreatedTickets(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[asJSON], func(t *testing.T) {
			f := newPRFollowUpFake()
			f.result = &pb.VerifyFollowUpResult{
				Comment: "failed", Reason: "comments-unreadable",
				Notes: []*pb.VerifyNoteFollowUp{{VerifyNoteId: "note:000000000001", Outcome: "created", TicketIdentifier: "T-1", TicketUrl: "https://x/1", Reason: "comments-unreadable"}},
			}
			cmd, out := prTestCmd(asJSON)
			err := prFollowUp(t.Context(), cmd, f, prOptions{pr: 7, asJSON: asJSON}, nil, true)
			if err == nil || strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "did not complete: comments-unreadable") {
				t.Fatalf("partial failure = %v", err)
			}
			if asJSON {
				decoder := json.NewDecoder(out)
				var body prFollowUpJSON
				if err := decoder.Decode(&body); err != nil || body.Result.OK || body.Result.Reason != "comments-unreadable" || len(body.Result.Results) != 1 || body.Result.Results[0].Ticket == nil || body.Result.Results[0].Ticket.Identifier != "T-1" {
					t.Fatalf("partial envelope = %+v, %v", body, err)
				}
				if err := decoder.Decode(new(any)); err != io.EOF {
					t.Fatalf("extra JSON: %v", err)
				}
			} else if !strings.Contains(out.String(), "created T-1 https://x/1") || !strings.Contains(out.String(), "notes comment could not be updated") {
				t.Fatalf("partial output = %q", out.String())
			}
		})
	}
}
func TestPrFollowUpSelectionBeforeClient_Sessions_AC213803(t *testing.T) {
	for _, args := range [][]string{{"7"}, {"7", "--all", "--note", "note:000000000001"}} {
		g := New(func(*cobra.Command) (Client, error) { t.Fatal("created client for invalid flags"); return nil, nil }, Options{})
		cmd := g.PRCmd()
		cmd.SetArgs(append([]string{"follow-up"}, args...))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestPrFollowUpDaemonErrors(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f := newPRFollowUpFake()
		f.err = rpcerror.WrapError(connect.NewError(connect.CodeDeadlineExceeded, errors.New("timeout")))
		cmd, _ := prTestCmd(false)
		err := prFollowUp(t.Context(), cmd, f, prOptions{pr: 7, dryRun: dry}, nil, true)
		if rpcerr.KindOf(err) != rpcerr.DeadlineExceeded || strings.Contains(err.Error(), "some tickets may already be filed") == dry {
			t.Fatalf("err=%v", err)
		}
	}
	f := newPRFollowUpFake()
	f.err = rpcerror.WrapError(connect.NewError(connect.CodePermissionDenied, errors.New("agent chat chat-123; run from your own terminal")))
	g := New(func(*cobra.Command) (Client, error) { return f, nil }, Options{Getenv: func(string) string { t.Fatal("read client environment"); return "" }})
	cmd := g.PRCmd()
	cmd.SetArgs([]string{"follow-up", "7", "--all"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); rpcerr.KindOf(err) != rpcerr.PermissionDenied || !strings.Contains(err.Error(), "chat-123") {
		t.Fatalf("err=%v", err)
	}
}
func TestPrFollowUpUnsupportedClient(t *testing.T) {
	g := New(func(*cobra.Command) (Client, error) { return &prCallerFake{prFake: &prFake{}}, nil }, Options{})
	cmd := g.PRCmd()
	cmd.SetArgs([]string{"follow-up", "7", "--all"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); rpcerr.KindOf(err) != rpcerr.Unimplemented {
		t.Fatalf("err=%v", err)
	}
}
