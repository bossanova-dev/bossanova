package vcs

import (
	"errors"
	"testing"
)

func TestEvaluateChecks(t *testing.T) {
	success := CheckConclusionSuccess
	failure := CheckConclusionFailure
	cancelled := CheckConclusionCancelled
	timedOut := CheckConclusionTimedOut
	skipped := CheckConclusionSkipped
	neutral := CheckConclusionNeutral

	tests := []struct {
		name             string
		checks           []CheckResult
		readErr          error
		wantState        CheckVerdictState
		wantReason       string
		wantPassed       int
		wantFailed       int
		wantPending      int
		wantSkipped      int
		wantUnclassified int
	}{
		{
			name:       "no checks",
			wantState:  CheckVerdictUnknown,
			wantReason: CheckVerdictReasonNoChecks,
		},
		{
			name:       "read error is unreadable",
			readErr:    errors.New("gh failed"),
			wantState:  CheckVerdictUnknown,
			wantReason: CheckVerdictReasonUnreadable,
		},
		{
			name:        "in progress with nil conclusion is pending",
			checks:      []CheckResult{{Name: "build", Status: CheckStatusInProgress}},
			wantState:   CheckVerdictPending,
			wantReason:  CheckVerdictReasonPending,
			wantPending: 1,
		},
		{
			name:             "completed nil conclusion is unclassified",
			checks:           []CheckResult{{Name: "build", Status: CheckStatusCompleted}},
			wantState:        CheckVerdictUnknown,
			wantReason:       CheckVerdictReasonUnclassified,
			wantUnclassified: 1,
		},
		{
			name:        "failure wins over pending",
			checks:      []CheckResult{{Name: "build", Status: CheckStatusCompleted, Conclusion: &failure}, {Name: "lint", Status: CheckStatusQueued}},
			wantState:   CheckVerdictFailing,
			wantReason:  CheckVerdictReasonFailed,
			wantFailed:  1,
			wantPending: 1,
		},
		{
			name:       "cancelled is failing",
			checks:     []CheckResult{{Name: "build", Status: CheckStatusCompleted, Conclusion: &cancelled}},
			wantState:  CheckVerdictFailing,
			wantReason: CheckVerdictReasonFailed,
			wantFailed: 1,
		},
		{
			name:       "timed out is failing",
			checks:     []CheckResult{{Name: "build", Status: CheckStatusCompleted, Conclusion: &timedOut}},
			wantState:  CheckVerdictFailing,
			wantReason: CheckVerdictReasonFailed,
			wantFailed: 1,
		},
		{
			name:        "success with skipped decoy",
			checks:      []CheckResult{{Name: "go-test", Status: CheckStatusCompleted, Conclusion: &success}, {Name: "test-go", Status: CheckStatusCompleted, Conclusion: &skipped}},
			wantState:   CheckVerdictGreen,
			wantReason:  CheckVerdictReasonOK,
			wantPassed:  1,
			wantSkipped: 1,
		},
		{
			name:        "all skipped or neutral means no gate ran",
			checks:      []CheckResult{{Name: "docs", Status: CheckStatusCompleted, Conclusion: &skipped}, {Name: "noop", Status: CheckStatusCompleted, Conclusion: &neutral}},
			wantState:   CheckVerdictGreen,
			wantReason:  CheckVerdictReasonNoGateRan,
			wantSkipped: 2,
		},
		{
			name:             "unclassified flag is unknown",
			checks:           []CheckResult{{Name: "build", Status: CheckStatusCompleted, Unclassified: true}},
			wantState:        CheckVerdictUnknown,
			wantReason:       CheckVerdictReasonUnclassified,
			wantUnclassified: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateChecks("sha", tt.checks, tt.readErr)
			if got.State != tt.wantState {
				t.Fatalf("State = %s, want %s", got.State, tt.wantState)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.HeadSHA != "sha" {
				t.Errorf("HeadSHA = %q, want sha", got.HeadSHA)
			}
			if got.Total != len(tt.checks) {
				t.Errorf("Total = %d, want %d", got.Total, len(tt.checks))
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Passed = %d, want %d", got.Passed, tt.wantPassed)
			}
			if got.Failed != tt.wantFailed {
				t.Errorf("Failed = %d, want %d", got.Failed, tt.wantFailed)
			}
			if got.Pending != tt.wantPending {
				t.Errorf("Pending = %d, want %d", got.Pending, tt.wantPending)
			}
			if got.Skipped != tt.wantSkipped {
				t.Errorf("Skipped = %d, want %d", got.Skipped, tt.wantSkipped)
			}
			if got.Unclassified != tt.wantUnclassified {
				t.Errorf("Unclassified = %d, want %d", got.Unclassified, tt.wantUnclassified)
			}
			if got.IsGreen() != (tt.wantState == CheckVerdictGreen) {
				t.Errorf("IsGreen() = %v, want %v", got.IsGreen(), tt.wantState == CheckVerdictGreen)
			}
			if got.DemonstratedPass() != (tt.wantState == CheckVerdictGreen && tt.wantPassed > 0) {
				t.Errorf("DemonstratedPass() = %v", got.DemonstratedPass())
			}
		})
	}
}

func TestCheckVerdictAt(t *testing.T) {
	success := CheckConclusionSuccess
	verdict := EvaluateChecks("old-sha", []CheckResult{{Status: CheckStatusCompleted, Conclusion: &success}}, nil)

	if got := verdict.At("old-sha"); got.State != CheckVerdictGreen || got.Reason != CheckVerdictReasonOK {
		t.Fatalf("At(old-sha) = %s/%s, want Green/ok", got.State, got.Reason)
	}
	if got := verdict.At("new-sha"); got.State != CheckVerdictUnknown || got.Reason != CheckVerdictReasonStaleSHA {
		t.Fatalf("At(new-sha) = %s/%s, want Unknown/stale-sha", got.State, got.Reason)
	}
	if got := verdict.At(""); got.State != CheckVerdictUnknown || got.Reason != CheckVerdictReasonStaleSHA {
		t.Fatalf("At(empty) = %s/%s, want Unknown/stale-sha", got.State, got.Reason)
	}
}
