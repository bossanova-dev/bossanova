package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/adapter/cloudrpc"
	"github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Both real daemon clients serve `boss pr state`.
var (
	_ prStateClient = (*daemonrpc.LocalClient)(nil)
	_ prStateClient = (*cloudrpc.RemoteClient)(nil)
)

// prStateFake is the daemon behind `boss pr state`. Unused Client methods
// panic through the nil embedded interface.
type prStateFake struct {
	Client
	origin      string
	resolveErr  error
	resp        *pb.GetPRStateResponse
	err         error
	resolveDirs []string
	reqs        []*pb.GetPRStateRequest
}

func (f *prStateFake) ResolveContext(_ context.Context, wd string) (*pb.ResolveContextResponse, error) {
	f.resolveDirs = append(f.resolveDirs, wd)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if f.origin == "" {
		return &pb.ResolveContextResponse{}, nil
	}
	return &pb.ResolveContextResponse{Repo: &pb.Repo{OriginUrl: f.origin}}, nil
}

func (f *prStateFake) GetPRState(_ context.Context, req *pb.GetPRStateRequest) (*pb.GetPRStateResponse, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &pb.GetPRStateResponse{Repo: req.GetRepo(), PrNumber: req.GetPrNumber()}, nil
}

func runPRStateCmd(t *testing.T, f *prStateFake, args ...string) (string, error) {
	t.Helper()
	g := New(func(*cobra.Command) (Client, error) { return f, nil }, Options{})
	root := &cobra.Command{Use: "boss", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(g.PRCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"pr", "state"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func failureCode(t *testing.T, out string) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("failure envelope %q: %v", out, err)
	}
	return body.Error.Code
}

func TestPRStateResolvesRefAndRepo(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		origin      string
		wantRepo    string
		wantPR      int32
		wantResolve bool
	}{
		{name: "--repo wins over the working directory", args: []string{"7", "--repo", "Acme/Widgets"}, origin: "git@github.com:acme/other.git", wantRepo: "acme/widgets", wantPR: 7},
		{name: "#n with --repo", args: []string{"#7", "--repo", "acme/widgets"}, wantRepo: "acme/widgets", wantPR: 7},
		{name: "PR URL carries its repo", args: []string{"https://github.com/Acme/Widgets/pull/9"}, origin: "git@github.com:acme/other.git", wantRepo: "acme/widgets", wantPR: 9},
		{name: "bare number uses the working directory's repo", args: []string{"12"}, origin: "git@github.com:Acme/Widgets.git", wantRepo: "acme/widgets", wantPR: 12, wantResolve: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &prStateFake{origin: tc.origin}
			if _, err := runPRStateCmd(t, f, append(tc.args, "--json")...); err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(f.reqs) != 1 || f.reqs[0].GetRepo() != tc.wantRepo || f.reqs[0].GetPrNumber() != tc.wantPR {
				t.Fatalf("requests = %v, want %s#%d", f.reqs, tc.wantRepo, tc.wantPR)
			}
			if got := len(f.resolveDirs) > 0; got != tc.wantResolve {
				t.Fatalf("working-directory lookup = %v, want %v", got, tc.wantResolve)
			}
		})
	}
}

func TestPRStatePassesCacheAndCommentFlags(t *testing.T) {
	f := &prStateFake{}
	if _, err := runPRStateCmd(t, f, "7", "--repo", "acme/widgets", "--fresh", "--with-review-comments", "--with-workflow-runs", "--json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !f.reqs[0].GetShouldBypassCache() || !f.reqs[0].GetShouldIncludeReviewComments() || !f.reqs[0].GetShouldIncludeWorkflowRuns() {
		t.Fatalf("request = %v, want bypass, comments and runs", f.reqs[0])
	}
	f = &prStateFake{}
	if _, err := runPRStateCmd(t, f, "7", "--repo", "acme/widgets", "--json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.reqs[0].GetShouldBypassCache() || f.reqs[0].GetShouldIncludeReviewComments() || f.reqs[0].GetShouldIncludeWorkflowRuns() {
		t.Fatalf("request = %v, want cached without comments or runs by default", f.reqs[0])
	}
}

func TestPRStateFailureCodes_Callbacks_AC230302(t *testing.T) {
	daemonErr := func(code connect.Code, msg string) error {
		return rpcerror.WrapError(connect.NewError(code, errors.New(msg)))
	}
	tests := []struct {
		name string
		f    *prStateFake
		args []string
		want string
		read bool
	}{
		{name: "zero PR", f: &prStateFake{}, args: []string{"0", "--repo", "acme/widgets"}, want: "INVALID_ARGUMENT"},
		{name: "negative PR", f: &prStateFake{origin: "https://github.com/acme/widgets"}, args: []string{"--", "-3"}, want: "INVALID_ARGUMENT"},
		{name: "bad --repo", f: &prStateFake{}, args: []string{"7", "--repo", "widgets"}, want: "INVALID_ARGUMENT"},
		{name: "URL disagrees with --repo", f: &prStateFake{}, args: []string{"https://github.com/acme/widgets/pull/7", "--repo", "acme/other"}, want: "INVALID_ARGUMENT"},
		{name: "no repo context", f: &prStateFake{}, args: []string{"7"}, want: "INVALID_ARGUMENT"},
		{name: "remote has no working-directory lookup", f: &prStateFake{resolveErr: daemonErr(connect.CodeUnimplemented, "ResolveContext is only available on a local daemon")}, args: []string{"7"}, want: "INVALID_ARGUMENT"},
		{name: "unregistered repo", f: &prStateFake{err: daemonErr(connect.CodeNotFound, "repo acme/other is not registered on this daemon")}, args: []string{"7", "--repo", "acme/other"}, want: "NOT_FOUND", read: true},
		{name: "daemon down", f: &prStateFake{err: daemonErr(connect.CodeUnavailable, "connection refused")}, args: []string{"7", "--repo", "acme/widgets"}, want: "UNAVAILABLE", read: true},
		{name: "old daemon or remote", f: &prStateFake{err: daemonErr(connect.CodeUnimplemented, "PR state is only available on a local daemon")}, args: []string{"7", "--repo", "acme/widgets"}, want: "UNIMPLEMENTED", read: true},
		{name: "rate limited", f: &prStateFake{err: daemonErr(connect.CodeResourceExhausted, "rate limit")}, args: []string{"7", "--repo", "acme/widgets"}, want: "RESOURCE_EXHAUSTED", read: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--json"}, tc.args...)
			out, err := runPRStateCmd(t, tc.f, args...)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := failureCode(t, out); got != tc.want {
				t.Fatalf("code = %q, want %q (%s)", got, tc.want, out)
			}
			if (len(tc.f.reqs) > 0) != tc.read {
				t.Fatalf("daemon reads = %v, want read=%v", tc.f.reqs, tc.read)
			}
		})
	}
}

func prStateFixture() *pb.GetPRStateResponse {
	mergeable, settled := true, false
	success, failure := pb.CheckConclusion_CHECK_CONCLUSION_SUCCESS, pb.CheckConclusion_CHECK_CONCLUSION_FAILURE
	return &pb.GetPRStateResponse{
		Repo: "acme/widgets", PrNumber: 7, State: pb.PRState_PR_STATE_OPEN, IsDraft: true,
		HeadSha: "abc123", HeadBranch: "feat", BaseBranch: "main", IsMergeable: &mergeable,
		MergeStateStatus: "blocked", ReviewDecision: pb.ReviewState_REVIEW_STATE_REQUIRED,
		LatestReviewState: pb.ReviewState_REVIEW_STATE_CHANGES_REQUESTED,
		Checks: []*pb.PRCheck{
			{Name: "ci", Status: pb.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: &success},
			{Name: "lint", Status: pb.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: &failure},
			{Name: "boss/verify", Status: pb.CheckStatus_CHECK_STATUS_IN_PROGRESS, Description: "needs human: AC-UNPROVEN"},
		},
		CheckVerdict:           &pb.PRCheckVerdict{State: pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_FAILING, Reason: "failed", TotalCount: 2, PassedCount: 1, FailedCount: 1},
		VerifyPhase:            &pb.PRVerifyPhase{Kind: pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_NEEDS_HUMAN, Reason: "AC-UNPROVEN"},
		HasBuildReceipt:        true,
		HasSettledWorkflowRuns: &settled,
		HeldWorkflowRuns:       []*pb.PRHeldWorkflowRun{{Name: "deploy", Status: "queued", Event: "push", Class: "in-flight"}},
		ObservedAt:             timestamppb.New(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)),
	}
}

func TestPRStateJSONEnvelope_Callbacks_AC230301(t *testing.T) {
	out, err := runPRStateCmd(t, &prStateFake{resp: prStateFixture()}, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope %q: %v", out, err)
	}
	want := map[string]any{
		"repo": "acme/widgets", "pr_number": float64(7), "state": "open", "is_draft": true,
		"head_sha": "abc123", "head_branch": "feat", "base_branch": "main", "is_mergeable": true,
		"merge_state_status": "blocked", "review_decision": "required", "latest_review_state": "changes_requested",
		"has_build_receipt": true, "has_settled_workflow_runs": false, "observed_at": "2026-11-02T09:00:00Z",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %#v, want %#v", k, env[k], v)
		}
	}
	for _, k := range []string{"checks", "check_verdict", "verify_phase", "held_workflow_runs", "review_comments"} {
		if _, ok := env[k]; !ok {
			t.Errorf("envelope lacks %q", k)
		}
	}
	verdict := env["check_verdict"].(map[string]any)
	if verdict["state"] != "failing" || verdict["reason"] != "failed" || verdict["passed_count"] != float64(1) || verdict["failed_count"] != float64(1) || verdict["total_count"] != float64(2) {
		t.Errorf("check_verdict = %v", verdict)
	}
	for _, k := range []string{"pending_count", "skipped_count", "unclassified_count"} {
		if _, ok := verdict[k]; !ok {
			t.Errorf("check_verdict lacks %q", k)
		}
	}
	if phase := env["verify_phase"].(map[string]any); phase["kind"] != "needs_human" || phase["reason"] != "AC-UNPROVEN" {
		t.Errorf("verify_phase = %v", phase)
	}
	checks := env["checks"].([]any)
	verify := checks[2].(map[string]any)
	if checks[1].(map[string]any)["conclusion"] != "failure" || verify["status"] != "in_progress" || verify["conclusion"] != "" || verify["description"] != "needs human: AC-UNPROVEN" {
		t.Errorf("checks = %v", checks)
	}
}

func TestPRStateJSONCarriesWorkflowRuns_Callbacks_AC231201(t *testing.T) {
	resp := prStateFixture()
	resp.IsWorkflowRunsRead = true
	resp.WorkflowRuns = []*pb.PRWorkflowRun{{Name: "CI", Status: "completed", Conclusion: "success", Event: "pull_request", HeadSha: "abc123"}}
	out, err := runPRStateCmd(t, &prStateFake{resp: resp}, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var env struct {
		WorkflowRuns       []map[string]string `json:"workflow_runs"`
		IsWorkflowRunsRead bool                `json:"is_workflow_runs_read"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope %q: %v", out, err)
	}
	want := map[string]string{"name": "CI", "status": "completed", "conclusion": "success", "event": "pull_request", "head_sha": "abc123"}
	if !env.IsWorkflowRunsRead || len(env.WorkflowRuns) != 1 || len(env.WorkflowRuns[0]) != len(want) {
		t.Fatalf("workflow runs = %v (read %v)", env.WorkflowRuns, env.IsWorkflowRunsRead)
	}
	for k, v := range want {
		if env.WorkflowRuns[0][k] != v {
			t.Errorf("workflow_runs[0].%s = %q, want %q", k, env.WorkflowRuns[0][k], v)
		}
	}
	out, err = runPRStateCmd(t, &prStateFake{resp: prStateFixture()}, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var unread map[string]any
	if err := json.Unmarshal([]byte(out), &unread); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if runs, ok := unread["workflow_runs"].([]any); !ok || len(runs) != 0 || unread["is_workflow_runs_read"] != false {
		t.Fatalf("unread runs = %#v / %#v, want an empty list and false", unread["workflow_runs"], unread["is_workflow_runs_read"])
	}
}

func TestPRStateVerifyPark_Callbacks_AC235301(t *testing.T) {
	resp := prStateFixture()
	success := pb.CheckConclusion_CHECK_CONCLUSION_SUCCESS
	verify := resp.Checks[2]
	verify.Status = pb.CheckStatus_CHECK_STATUS_COMPLETED
	verify.Conclusion = &success
	verify.Description = "needs human: human-review:ui · moderate: UI"
	resp.VerifyPhase.Reason = "human-review:ui"
	resp.VerifyPhase.Label = "moderate: UI"
	fake := &prStateFake{resp: resp}

	out, err := runPRStateCmd(t, fake, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope %q: %v", out, err)
	}
	phase := env["verify_phase"].(map[string]any)
	check := env["checks"].([]any)[2].(map[string]any)
	if env["is_verify_passed"] != false || phase["kind"] != "needs_human" || check["conclusion"] != "success" {
		t.Errorf("park verdict = %v, phase = %v, check = %v", env["is_verify_passed"], phase, check)
	}

	out, err = runPRStateCmd(t, fake, "7", "--repo", "acme/widgets")
	if err != nil || !strings.Contains(out, "verify: needs_human (human-review:ui · moderate: UI)") {
		t.Fatalf("park summary = %q, err = %v", out, err)
	}
}

func TestPRStateVerifyPass_Callbacks_AC235301(t *testing.T) {
	resp := &pb.GetPRStateResponse{
		Repo: "acme/widgets", PrNumber: 7, IsVerifyPassed: true,
		VerifyPhase: &pb.PRVerifyPhase{Kind: pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_NONE},
	}
	fake := &prStateFake{resp: resp}
	out, err := runPRStateCmd(t, fake, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope %q: %v", out, err)
	}
	phase := env["verify_phase"].(map[string]any)
	if env["is_verify_passed"] != true || phase["kind"] != "none" {
		t.Errorf("pass verdict = %v, phase = %v", env["is_verify_passed"], phase)
	}

	out, err = runPRStateCmd(t, fake, "7", "--repo", "acme/widgets")
	if err != nil || !strings.Contains(out, "verify: passed") {
		t.Fatalf("pass summary = %q, err = %v", out, err)
	}
}

func TestPRStateJSONNullsUncomputedFields(t *testing.T) {
	out, err := runPRStateCmd(t, &prStateFake{resp: &pb.GetPRStateResponse{Repo: "acme/widgets", PrNumber: 7}}, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	for _, k := range []string{"is_mergeable", "has_settled_workflow_runs"} {
		if v, ok := env[k]; !ok || v != nil {
			t.Errorf("%s = %#v (present=%v), want null", k, v, ok)
		}
	}
	if env["state"] != "" || env["review_decision"] != "" {
		t.Errorf("unset enums = %q / %q, want empty", env["state"], env["review_decision"])
	}
}

func TestPRStateHumanSummary(t *testing.T) {
	out, err := runPRStateCmd(t, &prStateFake{resp: prStateFixture()}, "7", "--repo", "acme/widgets")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"acme/widgets#7: open (draft)", "abc123", "merge state: blocked", "failing (failed)", "needs_human (AC-UNPROVEN)", "failing: lint", "pending: boss/verify", "settled: false (1 held)"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

func TestPRStateDegradedJSON_Callbacks_AC245501(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		fixture := prStateFixture()
		fixture.IsDegraded = degraded
		out, err := runPRStateCmd(t, &prStateFake{resp: fixture}, "7", "--repo", "acme/widgets", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatal(err)
		}
		if env["is_degraded"] != degraded {
			t.Fatalf("is_degraded=%v, want %v", env["is_degraded"], degraded)
		}
	}
}
