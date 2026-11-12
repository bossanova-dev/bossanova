package prstate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/ghusage"
)

const head = "0123456789abcdef0123456789abcdef01234567"

// providerCall is one provider read and what its context carried.
type providerCall struct {
	method string
	cached bool
	label  string
}

type recordingProvider struct {
	status     *vcs.PRStatus
	set        vcs.CheckSet
	runs       []vcs.WorkflowRun
	comments   []vcs.ReviewComment
	statusErr  error
	setErr     error
	runsErr    error
	commentErr error
	calls      []providerCall
	verifyHead string
	verifyPass bool
	verifyErr  error
}

func (f *recordingProvider) record(ctx context.Context, method string) {
	f.calls = append(f.calls, providerCall{method: method, cached: vcs.CachedReadsAllowed(ctx), label: ghusage.LabelFrom(ctx)})
}

func (f *recordingProvider) GetPRStatus(ctx context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	f.record(ctx, "status")
	return f.status, f.statusErr
}

func (f *recordingProvider) GetCheckSet(ctx context.Context, _ string, _ int) (vcs.CheckSet, error) {
	f.record(ctx, "checks")
	return f.set, f.setErr
}

func (f *recordingProvider) VerifyPassedAtHead(ctx context.Context, _, sha string) (bool, error) {
	f.record(ctx, "verify")
	f.verifyHead = sha
	return f.verifyPass, f.verifyErr
}

func (f *recordingProvider) ListWorkflowRuns(ctx context.Context, _, _ string) ([]vcs.WorkflowRun, error) {
	f.record(ctx, "runs")
	return f.runs, f.runsErr
}

func (f *recordingProvider) GetReviewComments(ctx context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	f.record(ctx, "comments")
	return f.comments, f.commentErr
}

func (f *recordingProvider) methods() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

func conclusion(c vcs.CheckConclusion) *vcs.CheckConclusion { return &c }

func passed(name string) vcs.CheckResult {
	return vcs.CheckResult{Name: name, Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionSuccess)}
}

func openStatus() *vcs.PRStatus {
	return &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: head, HeadBranch: "feat", BaseBranch: "main"}
}

var observed = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func read(t *testing.T, p *recordingProvider, opts Options) Snapshot {
	t.Helper()
	snap, err := NewReader(p, func() time.Time { return observed }).Read(context.Background(), "acme/widgets", 7, opts)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return snap
}

func TestReadVerifyPassConfirmsReportedHead_Callbacks_AC235301(t *testing.T) {
	for _, tc := range []struct {
		name string
		pass bool
		err  error
	}{
		{name: "cached pass for another head"},
		{name: "confirmed pass", pass: true},
		{name: "confirmation unavailable", err: errors.New("status read unavailable")},
		{name: "confirmation below quota floor", err: vcs.ErrRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProvider{
				status:     openStatus(),
				set:        vcs.CheckSet{Checks: []vcs.CheckResult{passed(vcs.VerifyStatusContext)}},
				verifyPass: tc.pass,
				verifyErr:  tc.err,
			}
			snap, err := NewReader(p, nil).Read(context.Background(), "acme/widgets", 7, Options{})
			if err != nil {
				t.Fatalf("Read discarded the available snapshot: %v", err)
			}
			if p.verifyHead != head || snap.VerifyPassed != tc.pass || snap.Status.HeadSHA != head {
				t.Fatalf("verified head = %q, pass = %t; want %q, %t", p.verifyHead, snap.VerifyPassed, head, tc.pass)
			}
		})
	}
}

func TestReadMarksEveryCallCachedAndLabelled(t *testing.T) {
	p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci")}}}
	read(t, p, Options{IncludeReviewComments: true})
	if got, want := p.methods(), []string{"status", "checks", "runs", "comments"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for _, c := range p.calls {
		if !c.cached || c.label != ghusage.LabelPRState {
			t.Fatalf("%s read cached=%v label=%q, want cached under %q", c.method, c.cached, c.label, ghusage.LabelPRState)
		}
	}
}

func TestReadFreshBypassesCacheButKeepsLabel(t *testing.T) {
	p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci")}}}
	read(t, p, Options{Fresh: true, IncludeReviewComments: true})
	if len(p.calls) != 4 {
		t.Fatalf("calls = %v, want all four reads", p.methods())
	}
	for _, c := range p.calls {
		if c.cached || c.label != ghusage.LabelPRState {
			t.Fatalf("%s read cached=%v label=%q, want uncached under %q", c.method, c.cached, c.label, ghusage.LabelPRState)
		}
	}
}

func TestReadAttributesReadsToTheRequestedLabel(t *testing.T) {
	p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci")}}}
	read(t, p, Options{Label: ghusage.LabelPRWait})
	if len(p.calls) != 3 {
		t.Fatalf("calls = %v, want status, checks and runs", p.methods())
	}
	for _, c := range p.calls {
		if c.label != ghusage.LabelPRWait {
			t.Fatalf("%s read label=%q, want %q", c.method, c.label, ghusage.LabelPRWait)
		}
	}
}

func TestReadReviewCommentsOnlyWhenAsked(t *testing.T) {
	comments := []vcs.ReviewComment{{Author: "rev", Body: "fix", State: vcs.ReviewStateChangesRequested}}
	for _, include := range []bool{false, true} {
		p := &recordingProvider{status: openStatus(), comments: comments}
		snap := read(t, p, Options{IncludeReviewComments: include})
		count := 0
		for _, m := range p.methods() {
			if m == "comments" {
				count++
			}
		}
		want := 0
		if include {
			want = 1
		}
		if count != want {
			t.Fatalf("include=%v: GetReviewComments called %d times, want %d", include, count, want)
		}
		if include != (len(snap.ReviewComments) == 1) {
			t.Fatalf("include=%v: review comments = %+v", include, snap.ReviewComments)
		}
	}
}

func TestReadRunsSettlement(t *testing.T) {
	pending := vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusInProgress}
	failed := vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionFailure)}
	tests := []struct {
		name        string
		checks      []vcs.CheckResult
		runs        []vcs.WorkflowRun
		wantRunRead bool
		wantSettled *bool
		wantHeld    []HeldWorkflowRun
	}{
		{name: "pending verdict skips runs", checks: []vcs.CheckResult{pending}},
		{name: "failing verdict skips runs", checks: []vcs.CheckResult{failed}},
		{name: "no checks is not green and skips runs"},
		{
			name: "green with every run completed is settled", checks: []vcs.CheckResult{passed("ci")},
			runs:        []vcs.WorkflowRun{{Name: "ci", Status: "completed", HeadSHA: head}, {Name: "old", Status: "queued", HeadSHA: "other"}},
			wantRunRead: true, wantSettled: ptr(true),
		},
		{
			name: "green with a queued run is held", checks: []vcs.CheckResult{passed("ci")},
			runs: []vcs.WorkflowRun{
				{Name: "deploy", Status: "in_progress", Event: "push", HeadSHA: head},
				{Name: "approve", Status: "waiting", Event: "pull_request", HeadSHA: head},
			},
			wantRunRead: true, wantSettled: ptr(false),
			wantHeld: []HeldWorkflowRun{
				{Name: "approve", Status: "waiting", Event: "pull_request", Class: HeldClassAwaitingApproval},
				{Name: "deploy", Status: "in_progress", Event: "push", Class: HeldClassInFlight},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: tc.checks}, runs: tc.runs}
			snap := read(t, p, Options{})
			readRuns := false
			for _, m := range p.methods() {
				readRuns = readRuns || m == "runs"
			}
			if readRuns != tc.wantRunRead {
				t.Fatalf("runs read = %v, want %v", readRuns, tc.wantRunRead)
			}
			if !reflect.DeepEqual(snap.RunsSettled, tc.wantSettled) {
				t.Fatalf("RunsSettled = %v, want %v", deref(snap.RunsSettled), deref(tc.wantSettled))
			}
			if !reflect.DeepEqual(snap.HeldRuns, tc.wantHeld) {
				t.Fatalf("HeldRuns = %+v, want %+v", snap.HeldRuns, tc.wantHeld)
			}
		})
	}
}

func TestReadCarriesEveryHeadRunWhenTheRunsReadHappens(t *testing.T) {
	pending := vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusInProgress}
	runs := []vcs.WorkflowRun{
		{Name: "CI", Status: "in_progress", Event: "pull_request", HeadSHA: head},
		{Name: "Lint", Status: "completed", Conclusion: "success", Event: "push", HeadSHA: head},
	}
	tests := []struct {
		name     string
		checks   []vcs.CheckResult
		include  bool
		wantRead bool
		wantRuns []vcs.WorkflowRun
	}{
		{name: "green verdict reads runs", checks: []vcs.CheckResult{passed("ci")}, wantRead: true, wantRuns: runs},
		{name: "pending verdict skips runs by default", checks: []vcs.CheckResult{pending}},
		{name: "pending verdict reads runs when asked", checks: []vcs.CheckResult{pending}, include: true, wantRead: true, wantRuns: runs},
		{name: "no checks reads runs when asked", include: true, wantRead: true, wantRuns: runs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: tc.checks}, runs: runs}
			snap := read(t, p, Options{IncludeWorkflowRuns: tc.include})
			if snap.RunsRead != tc.wantRead || !reflect.DeepEqual(snap.WorkflowRuns, tc.wantRuns) {
				t.Fatalf("RunsRead = %v, WorkflowRuns = %+v; want %v, %+v", snap.RunsRead, snap.WorkflowRuns, tc.wantRead, tc.wantRuns)
			}
			if !snap.Verdict.IsGreen() && (snap.RunsSettled != nil || snap.HeldRuns != nil) {
				t.Fatalf("a non-green verdict carried settledness %v / %+v", deref(snap.RunsSettled), snap.HeldRuns)
			}
		})
	}
}

func TestReadAskedForRunsOnAnEmptyHeadReadsNone(t *testing.T) {
	status := openStatus()
	status.HeadSHA = ""
	p := &recordingProvider{status: status}
	snap := read(t, p, Options{IncludeWorkflowRuns: true})
	if snap.RunsRead || snap.WorkflowRuns != nil {
		t.Fatalf("RunsRead = %v, WorkflowRuns = %+v, want no read for an empty head", snap.RunsRead, snap.WorkflowRuns)
	}
}

func TestReadMapsVerdictVerifyPhaseAndReceipt(t *testing.T) {
	hold := vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusInProgress, Description: "needs human: AC-UNPROVEN · check it"}
	p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci"), passed("lint"), hold}, HasBuildReceipt: true}}
	snap := read(t, p, Options{})
	if snap.Verdict.State != vcs.CheckVerdictGreen || snap.Verdict.Passed != 2 || snap.Verdict.Total != 2 || snap.Verdict.HeadSHA != head {
		t.Fatalf("verdict = %+v, want green over the two ordinary checks", snap.Verdict)
	}
	if snap.VerifyPhase.Kind != vcs.VerifyPhaseNeedsHuman || snap.VerifyPhase.Reason != "AC-UNPROVEN" || snap.VerifyPhase.Label != "check it" {
		t.Fatalf("verify phase = %+v", snap.VerifyPhase)
	}
	if !snap.HasBuildReceipt || len(snap.Checks) != 3 || snap.Status.HeadSHA != head || !snap.ObservedAt.Equal(observed) {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestReadFailuresReturnErrors(t *testing.T) {
	boom := errors.New("boom")
	green := vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci")}}
	tests := []struct {
		name string
		p    *recordingProvider
		want error
	}{
		{name: "status", p: &recordingProvider{statusErr: vcs.ErrRateLimited}, want: vcs.ErrRateLimited},
		{name: "nil status", p: &recordingProvider{}, want: ErrNoStatus},
		{name: "checks", p: &recordingProvider{status: openStatus(), setErr: boom}, want: boom},
		{name: "runs", p: &recordingProvider{status: openStatus(), set: green, runsErr: boom}, want: boom},
		{name: "comments", p: &recordingProvider{status: openStatus(), commentErr: boom}, want: boom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := NewReader(tc.p, nil).Read(context.Background(), "acme/widgets", 7, Options{IncludeReviewComments: true})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !reflect.DeepEqual(snap, Snapshot{}) {
				t.Fatalf("failed read returned a partial snapshot %+v", snap)
			}
		})
	}
}

func TestReadEmptyHeadIsNotSettled(t *testing.T) {
	status := openStatus()
	status.HeadSHA = ""
	p := &recordingProvider{status: status, set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci")}}}
	snap := read(t, p, Options{})
	if snap.RunsSettled == nil || *snap.RunsSettled {
		t.Fatalf("RunsSettled = %v, want false for an empty head", deref(snap.RunsSettled))
	}
	for _, m := range p.methods() {
		if m == "runs" {
			t.Fatal("an empty head SHA must not read workflow runs")
		}
	}
}

func ptr[T any](v T) *T { return &v }

func deref(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	if *b {
		return "true"
	}
	return "false"
}

// requiredSource is a fixed required-check policy that records the repo it
// was asked about.
type requiredSource struct {
	policy vcs.RequiredChecks
	repos  []string
}

func (s *requiredSource) RequiredChecks(_ context.Context, repoRef string) vcs.RequiredChecks {
	s.repos = append(s.repos, repoRef)
	return s.policy
}

func TestReadJudgesTheVerdictByTheRepoRequiredChecks(t *testing.T) {
	tests := []struct {
		name      string
		checks    []vcs.CheckResult
		wantState vcs.CheckVerdictState
		wantCalls []string
	}{
		{"required check absent reads not-requested and skips the runs read", []vcs.CheckResult{passed("lint")}, vcs.CheckVerdictUnknown, []string{"status", "checks"}},
		{"required check passed reads green", []vcs.CheckResult{passed("lint"), passed("go-test")}, vcs.CheckVerdictGreen, []string{"status", "checks", "runs"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: tt.checks}}
			src := &requiredSource{policy: vcs.RequiredChecks{Names: []string{"go-test"}}}
			r := NewReader(p, func() time.Time { return observed })
			r.SetRequiredChecks(src)
			snap, err := r.Read(context.Background(), "acme/widgets", 7, Options{})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if snap.Verdict.State != tt.wantState {
				t.Fatalf("verdict = %+v, want state %v", snap.Verdict, tt.wantState)
			}
			if tt.wantState != vcs.CheckVerdictGreen && snap.Verdict.Reason != vcs.CheckVerdictReasonNotRequested {
				t.Fatalf("verdict reason = %q, want %q", snap.Verdict.Reason, vcs.CheckVerdictReasonNotRequested)
			}
			if got := p.methods(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", got, tt.wantCalls)
			}
			if !reflect.DeepEqual(src.repos, []string{"acme/widgets"}) {
				t.Fatalf("policy asked about %v, want [acme/widgets]", src.repos)
			}
		})
	}
}

func TestReadPropagatesDegradedSnapshot_Callbacks_AC245501(t *testing.T) {
	for _, tc := range []struct{ status, checks bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Degraded: tc.checks}}
		p.status.Degraded = tc.status
		snap := read(t, p, Options{})
		if snap.Degraded != (tc.status || tc.checks) {
			t.Fatalf("degraded=%v for %+v", snap.Degraded, tc)
		}
	}
}

func TestReadWorkflowRunsQuotaRefusal_Callbacks_AC245701(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checks  []vcs.CheckResult
		include bool
		green   bool
	}{
		{"green", []vcs.CheckResult{passed("ci")}, false, true},
		{"pending requested runs", []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusInProgress}}, true, false},
		{"cancelled", []vcs.CheckResult{{Name: "ci", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionCancelled)}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: tc.checks}, runsErr: fmt.Errorf("quota: %w", vcs.ErrRateLimited)}
			snap := read(t, p, Options{IncludeWorkflowRuns: tc.include})
			if !snap.Degraded || snap.RunsRead || len(snap.HeldRuns) != 0 || len(snap.WorkflowRuns) != 0 {
				t.Fatalf("refused snapshot = %+v", snap)
			}
			if tc.green {
				if snap.RunsSettled == nil || *snap.RunsSettled {
					t.Fatalf("green settlement = %v", snap.RunsSettled)
				}
				if evaluateWait(checksWait(7), snap, head) != waitPending {
					t.Fatal("green head settled without reading runs")
				}
			} else if snap.RunsSettled != nil {
				t.Fatal("non-green verdict carried settlement")
			}
		})
	}
}

func TestReadCancelledChecksAwaitQuotaRecovery_Callbacks_AC245701(t *testing.T) {
	cancelled := vcs.CheckResult{Name: "ci", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionCancelled)}
	for _, tc := range []struct {
		name   string
		checks []vcs.CheckResult
		want   vcs.CheckVerdictState
		wait   WaitOutcome
	}{
		{"cancellation needs reconciliation", []vcs.CheckResult{cancelled}, vcs.CheckVerdictPending, waitPending},
		{"real failure still settles", []vcs.CheckResult{cancelled, {Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionFailure)}}, vcs.CheckVerdictFailing, WaitMet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: tc.checks}, runsErr: vcs.ErrRateLimited}
			snap := read(t, p, Options{})
			if snap.Verdict.State != tc.want || evaluateWait(checksWait(7), snap, head) != tc.wait || !snap.Degraded || snap.RunsRead {
				t.Fatalf("refused cancellation snapshot = %+v", snap)
			}
			if *snap.Checks[0].Conclusion != vcs.CheckConclusionCancelled {
				t.Fatal("reconciliation changed the raw check result")
			}
		})
	}
	p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{cancelled, passed("replacement")}}, runsErr: vcs.ErrRateLimited}
	if snap := read(t, p, Options{}); evaluateWait(checksWait(7), snap, head) != waitPending {
		t.Fatal("cancelled check settled before its replacement runs were readable")
	}
	p.runsErr = nil
	p.runs = []vcs.WorkflowRun{{Name: "CI", HeadSHA: head, Status: "completed", Conclusion: "success"}}
	if snap := read(t, p, Options{}); !snap.Verdict.IsGreen() || evaluateWait(checksWait(7), snap, head) != WaitMet {
		t.Fatalf("recovered cancellation snapshot = %+v", snap)
	}
}
