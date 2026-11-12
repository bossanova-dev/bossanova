package prstate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
)

func TestReadCancelledWorkflow_Callbacks_AC246001(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []vcs.WorkflowRun
		want vcs.CheckVerdictState
	}{
		{"success", []vcs.WorkflowRun{{Name: "CI", Status: "completed", Conclusion: "success", HeadSHA: head}}, vcs.CheckVerdictGreen},
		{"pending", []vcs.WorkflowRun{{Name: "CI", Status: "in_progress", HeadSHA: head}}, vcs.CheckVerdictPending},
		{"unmatched", []vcs.WorkflowRun{{Name: "other", Status: "completed", Conclusion: "success"}}, vcs.CheckVerdictFailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := vcs.CheckResult{Name: "notify", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionCancelled)}
			p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("build"), cancelled}}, runs: tc.runs}
			snap := read(t, p, Options{IncludeWorkflowRuns: true})
			if snap.Verdict.State != tc.want {
				t.Fatalf("verdict=%+v, want %v", snap.Verdict, tc.want)
			}
			if !snap.RunsRead || !reflect.DeepEqual(snap.WorkflowRuns, tc.runs) {
				t.Fatalf("runs not retained: %+v", snap)
			}
			if !reflect.DeepEqual(snap.Checks, p.set.Checks) {
				t.Fatal("raw checks changed")
			}
			if got := p.methods(); !reflect.DeepEqual(got, []string{"status", "checks", "runs"}) {
				t.Fatalf("calls=%v", got)
			}
			if tc.want == vcs.CheckVerdictGreen && (snap.RunsSettled == nil || !*snap.RunsSettled) {
				t.Fatal("green replacement not settled")
			}
		})
	}
}

func TestReadCancelledWorkflowRunsError(t *testing.T) {
	p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{{Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionCancelled)}}}, runsErr: errors.New("unreadable")}
	if _, err := NewReader(p, nil).Read(context.Background(), "acme/widgets", 7, Options{}); !errors.Is(err, p.runsErr) {
		t.Fatalf("Read error=%v", err)
	}
}
