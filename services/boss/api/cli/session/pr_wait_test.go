package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/adapter/cloudrpc"
	"github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Both real daemon clients serve `boss pr wait`.
var (
	_ prWaitClient = (*daemonrpc.LocalClient)(nil)
	_ prWaitClient = (*cloudrpc.RemoteClient)(nil)
)

// prWaitFake is the daemon behind `boss pr wait`.
type prWaitFake struct {
	prStateFake
	outcome pb.PRWaitOutcome
	waitErr error
	waits   []*pb.WaitPRStateRequest
}

func (f *prWaitFake) WaitPRState(_ context.Context, req *pb.WaitPRStateRequest) (*pb.WaitPRStateResponse, error) {
	f.waits = append(f.waits, req)
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return &pb.WaitPRStateResponse{Outcome: f.outcome, Snapshot: prStateFixture(), TimeoutSeconds: req.GetTimeoutSeconds()}, nil
}

func runPRWaitCmd(t *testing.T, f *prWaitFake, args ...string) (string, error) {
	t.Helper()
	g := New(func(*cobra.Command) (Client, error) { return f, nil }, Options{})
	root := &cobra.Command{Use: "boss", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(g.PRCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"pr", "wait"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestPRWaitJSONEnvelopeExitsZeroForEveryOutcome_Callbacks_AC231101(t *testing.T) {
	for outcome, want := range map[pb.PRWaitOutcome]string{
		pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET:         "met",
		pb.PRWaitOutcome_PR_WAIT_OUTCOME_UNREACHABLE: "unreachable",
		pb.PRWaitOutcome_PR_WAIT_OUTCOME_TIMED_OUT:   "timed_out",
	} {
		t.Run(want, func(t *testing.T) {
			out, err := runPRWaitCmd(t, &prWaitFake{outcome: outcome}, "7", "--repo", "acme/widgets", "--until", "checks-settled", "--json")
			if err != nil {
				t.Fatalf("run: %v (an answered outcome must exit 0)", err)
			}
			var env struct {
				Outcome        string         `json:"outcome"`
				Condition      string         `json:"condition"`
				TimeoutSeconds int32          `json:"timeout_seconds"`
				Snapshot       map[string]any `json:"snapshot"`
			}
			if err := json.Unmarshal([]byte(out), &env); err != nil {
				t.Fatalf("envelope %q: %v", out, err)
			}
			if env.Outcome != want || env.Condition != "checks-settled" || env.TimeoutSeconds != 300 {
				t.Fatalf("envelope = %+v, want outcome %q, condition checks-settled, 300s", env, want)
			}
			if env.Snapshot["head_sha"] != "abc123" || env.Snapshot["state"] != "open" || env.Snapshot["check_verdict"].(map[string]any)["state"] != "failing" {
				t.Fatalf("snapshot = %v, want the pr state envelope", env.Snapshot)
			}
		})
	}
}

func TestPRWaitSendsTheRequestedWait(t *testing.T) {
	f := &prWaitFake{outcome: pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET}
	_, err := runPRWaitCmd(t, f, "https://github.com/Acme/Widgets/pull/9", "--until", "head-changed", "--since-head", "abc", "--timeout", "90500ms", "--fresh", "--accept-no-gate", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := &pb.WaitPRStateRequest{Repo: "acme/widgets", PrNumber: 9, Condition: pb.PRWaitCondition_PR_WAIT_CONDITION_HEAD_CHANGED, SinceHeadSha: "abc", TimeoutSeconds: 91, ShouldBypassCache: true, ShouldAcceptNoGateRan: true}
	if len(f.waits) != 1 || f.waits[0].String() != want.String() {
		t.Fatalf("waits = %v, want %v", f.waits, want)
	}
}

func TestPRWaitFailureCodes_Callbacks_AC231102(t *testing.T) {
	daemonErr := func(code connect.Code, msg string) error {
		return rpcerror.WrapError(connect.NewError(code, errors.New(msg)))
	}
	tests := []struct {
		name string
		f    *prWaitFake
		args []string
		want string
		wait bool
	}{
		{name: "timeout over the cap", f: &prWaitFake{}, args: []string{"7", "--repo", "acme/widgets", "--until", "checks-settled", "--timeout", "9m"}, want: "INVALID_ARGUMENT"},
		{name: "zero timeout", f: &prWaitFake{}, args: []string{"7", "--repo", "acme/widgets", "--until", "mergeable", "--timeout", "0s"}, want: "INVALID_ARGUMENT"},
		{name: "missing --until", f: &prWaitFake{}, args: []string{"7", "--repo", "acme/widgets"}, want: "INVALID_ARGUMENT"},
		{name: "unknown --until", f: &prWaitFake{}, args: []string{"7", "--repo", "acme/widgets", "--until", "green"}, want: "INVALID_ARGUMENT"},
		{name: "unregistered repo", f: &prWaitFake{waitErr: daemonErr(connect.CodeNotFound, "repo unmanaged/nowhere is not registered on this daemon")}, args: []string{"7", "--repo", "unmanaged/nowhere", "--until", "checks-settled"}, want: "NOT_FOUND", wait: true},
		{name: "daemon shutting down", f: &prWaitFake{waitErr: daemonErr(connect.CodeUnavailable, "daemon shutting down")}, args: []string{"7", "--repo", "acme/widgets", "--until", "checks-settled"}, want: "UNAVAILABLE", wait: true},
		{name: "old daemon or remote", f: &prWaitFake{waitErr: daemonErr(connect.CodeUnimplemented, "PR wait is only available on a local daemon")}, args: []string{"7", "--repo", "acme/widgets", "--until", "checks-settled"}, want: "UNIMPLEMENTED", wait: true},
		{name: "rate limited", f: &prWaitFake{waitErr: daemonErr(connect.CodeResourceExhausted, "rate limit")}, args: []string{"7", "--repo", "acme/widgets", "--until", "checks-settled"}, want: "RESOURCE_EXHAUSTED", wait: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPRWaitCmd(t, tc.f, append([]string{"--json"}, tc.args...)...)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := failureCode(t, out); got != tc.want {
				t.Fatalf("code = %q, want %q (%s)", got, tc.want, out)
			}
			if (len(tc.f.waits) > 0) != tc.wait {
				t.Fatalf("daemon waits = %v, want wait=%v", tc.f.waits, tc.wait)
			}
		})
	}
}

func TestPRWaitHumanSummaryLeadsWithTheOutcome(t *testing.T) {
	out, err := runPRWaitCmd(t, &prWaitFake{outcome: pb.PRWaitOutcome_PR_WAIT_OUTCOME_TIMED_OUT}, "7", "--repo", "acme/widgets", "--until", "mergeable")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], "timed_out") || !strings.Contains(lines[1], "acme/widgets#7") {
		t.Fatalf("summary = %q, want the outcome line then the PR state summary", out)
	}
}
