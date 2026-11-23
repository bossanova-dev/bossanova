package vcs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type workflowRunsCheckProvider struct {
	*receiptCheckProvider
	runs     []WorkflowRun
	runsErr  error
	runCalls int
}

func (p *workflowRunsCheckProvider) ListWorkflowRuns(context.Context, string, string) ([]WorkflowRun, error) {
	p.runCalls++
	return p.runs, p.runsErr
}

func TestReadCheckSet_ReconcilesSupersededCancelled(t *testing.T) {
	cancelled := CheckConclusionCancelled
	success := CheckConclusionSuccess
	cancelledCheck := CheckResult{
		Name:       "Notify on nightly failure",
		Workflow:   "scripts",
		Status:     CheckStatusCompleted,
		Conclusion: &cancelled,
	}
	passedCheck := CheckResult{
		Name:       "go-test",
		Workflow:   "CI",
		Status:     CheckStatusCompleted,
		Conclusion: &success,
	}

	for _, tc := range []struct {
		name         string
		checks       []CheckResult
		runs         []WorkflowRun
		runsErr      error
		noLister     bool
		headSHA      string
		wantVerdict  CheckVerdictState
		wantRunCalls int
		wantRunsRead bool
		wantRunsErr  bool
	}{
		{
			name:   "in-flight replacement computes pending",
			checks: []CheckResult{cancelledCheck, passedCheck},
			runs: []WorkflowRun{
				{Name: "scripts", HeadSHA: "96967f58", Status: "completed", Conclusion: "cancelled"},
				{Name: "scripts", HeadSHA: "96967f58", Status: "in_progress"},
			},
			headSHA:      "96967f58",
			wantVerdict:  CheckVerdictPending,
			wantRunCalls: 1,
			wantRunsRead: true,
		},
		{
			name:   "passed replacement drops cancellation",
			checks: []CheckResult{cancelledCheck, passedCheck},
			runs: []WorkflowRun{
				{Name: "scripts", HeadSHA: "96967f58", Status: "completed", Conclusion: "cancelled"},
				{Name: "scripts", HeadSHA: "96967f58", Status: "completed", Conclusion: "success"},
			},
			headSHA:      "96967f58",
			wantVerdict:  CheckVerdictGreen,
			wantRunCalls: 1,
			wantRunsRead: true,
		},
		{
			name:   "genuine cancellation with no replacement counts as failed",
			checks: []CheckResult{cancelledCheck, passedCheck},
			runs: []WorkflowRun{
				{Name: "scripts", HeadSHA: "96967f58", Status: "completed", Conclusion: "cancelled"},
			},
			headSHA:      "96967f58",
			wantVerdict:  CheckVerdictFailing,
			wantRunCalls: 1,
			wantRunsRead: true,
		},
		{
			name:         "runs-listing error leaves cancellation in place",
			checks:       []CheckResult{cancelledCheck, passedCheck},
			runsErr:      errors.New("github unavailable"),
			headSHA:      "96967f58",
			wantVerdict:  CheckVerdictFailing,
			wantRunCalls: 1,
			wantRunsErr:  true,
		},
		{
			name:        "missing workflow run lister leaves cancellation in place",
			checks:      []CheckResult{cancelledCheck, passedCheck},
			noLister:    true,
			headSHA:     "96967f58",
			wantVerdict: CheckVerdictFailing,
		},
		{
			name:        "no cancelled check skips listing workflow runs",
			checks:      []CheckResult{passedCheck},
			headSHA:     "96967f58",
			wantVerdict: CheckVerdictGreen,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &receiptCheckProvider{
				legacyCheckProvider: &legacyCheckProvider{checks: tc.checks},
				set:                 CheckSet{Checks: tc.checks, HasBuildReceipt: true},
			}
			withRuns := &workflowRunsCheckProvider{
				receiptCheckProvider: base,
				runs:                 tc.runs,
				runsErr:              tc.runsErr,
			}
			var reader CheckResultsReader = withRuns
			if tc.noLister {
				reader = base
			}
			got, err := ReadCheckSet(context.Background(), reader, "owner/repo", 4630, tc.headSHA)
			if err != nil {
				t.Fatalf("ReadCheckSet: %v", err)
			}
			verdict := EvaluateChecks(tc.headSHA, got.Checks, nil)
			if verdict.State != tc.wantVerdict {
				t.Fatalf("verdict=%v (%s) want %v; checks=%+v", verdict.State, verdict.Reason, tc.wantVerdict, got.Checks)
			}
			if withRuns.runCalls != tc.wantRunCalls {
				t.Fatalf("ListWorkflowRuns calls=%d want %d", withRuns.runCalls, tc.wantRunCalls)
			}
			if got.RunsRead != tc.wantRunsRead {
				t.Fatalf("RunsRead=%v want %v", got.RunsRead, tc.wantRunsRead)
			}
			if (got.RunsErr != nil) != tc.wantRunsErr {
				t.Fatalf("RunsErr=%v wantErr=%v", got.RunsErr, tc.wantRunsErr)
			}
		})
	}
}

func TestReadCheckSet_RateLimitLogsOncePerWindow(t *testing.T) {
	cancelled := CheckConclusionCancelled
	checks := []CheckResult{{Name: "notify", Workflow: "scripts", Status: CheckStatusCompleted, Conclusion: &cancelled}}
	p := &workflowRunsCheckProvider{
		receiptCheckProvider: &receiptCheckProvider{
			legacyCheckProvider: &legacyCheckProvider{checks: checks},
			set:                 CheckSet{Checks: checks},
		},
		runsErr: ErrRateLimited,
	}
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	rll := &RateLimitLog{Now: func() time.Time { return now }}
	cfg := CheckReadLog{Logger: &logger, RateLimitLog: rll, Site: "display poller"}

	if _, err := ReadCheckSet(context.Background(), p, "owner/repo", 4630, "96967f58", cfg); err != nil {
		t.Fatalf("first ReadCheckSet: %v", err)
	}
	if _, err := ReadCheckSet(context.Background(), p, "owner/repo", 4630, "96967f58", cfg); err != nil {
		t.Fatalf("second ReadCheckSet: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"level":"warn"`) || !strings.Contains(lines[1], `"level":"debug"`) {
		t.Fatalf("expected warn then debug, got:\n%s", buf.String())
	}
}
