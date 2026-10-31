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

func TestEvaluateChecks_PendingVerifyExcluded(t *testing.T) {
	success := CheckConclusionSuccess
	failure := CheckConclusionFailure

	tests := []struct {
		name        string
		checks      []CheckResult
		wantState   CheckVerdictState
		wantReason  string
		wantTotal   int
		wantPassed  int
		wantFailed  int
		wantPending int
	}{
		{
			name: "pending verify claim on green CI is green",
			checks: []CheckResult{
				{Name: "build", Status: CheckStatusCompleted, Conclusion: &success},
				{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: "verifying… tok"},
			},
			wantState:  CheckVerdictGreen,
			wantReason: CheckVerdictReasonOK,
			wantTotal:  1,
			wantPassed: 1,
		},
		{
			name: "pending verify park on green CI is green",
			checks: []CheckResult{
				{Name: "build", Status: CheckStatusCompleted, Conclusion: &success},
				{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: "needs human: ledger-open"},
			},
			wantState:  CheckVerdictGreen,
			wantReason: CheckVerdictReasonOK,
			wantTotal:  1,
			wantPassed: 1,
		},
		{
			name: "ordinary pending still pending beside pending verify",
			checks: []CheckResult{
				{Name: "build", Status: CheckStatusInProgress},
				{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: "waiting: checks-pending"},
			},
			wantState:   CheckVerdictPending,
			wantReason:  CheckVerdictReasonPending,
			wantTotal:   1,
			wantPending: 1,
		},
		{
			name: "lone pending verify judges nothing",
			checks: []CheckResult{
				{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: "verifying… tok"},
			},
			wantState:  CheckVerdictUnknown,
			wantReason: CheckVerdictReasonNoChecks,
		},
		{
			name: "completed verify failure is still failing",
			checks: []CheckResult{
				{Name: "build", Status: CheckStatusCompleted, Conclusion: &success},
				{Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &failure, Description: "defect: tests red"},
			},
			wantState:  CheckVerdictFailing,
			wantReason: CheckVerdictReasonFailed,
			wantTotal:  2,
			wantPassed: 1,
			wantFailed: 1,
		},
		{
			name: "completed verify success counts as a pass",
			checks: []CheckResult{
				{Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &success, Description: "verified"},
			},
			wantState:  CheckVerdictGreen,
			wantReason: CheckVerdictReasonOK,
			wantTotal:  1,
			wantPassed: 1,
		},
		{
			name: "differently cased context is ordinary pending",
			checks: []CheckResult{
				{Name: "build", Status: CheckStatusCompleted, Conclusion: &success},
				{Name: "Boss/Verify", Status: CheckStatusQueued, Description: "verifying…"},
			},
			wantState:   CheckVerdictPending,
			wantReason:  CheckVerdictReasonPending,
			wantTotal:   2,
			wantPassed:  1,
			wantPending: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateChecks("sha", tt.checks, nil)
			if got.State != tt.wantState || got.Reason != tt.wantReason {
				t.Fatalf("verdict = %s/%s, want %s/%s", got.State, got.Reason, tt.wantState, tt.wantReason)
			}
			if got.Total != tt.wantTotal {
				t.Errorf("Total = %d, want %d", got.Total, tt.wantTotal)
			}
			if got.Passed != tt.wantPassed || got.Failed != tt.wantFailed || got.Pending != tt.wantPending {
				t.Errorf("passed/failed/pending = %d/%d/%d, want %d/%d/%d",
					got.Passed, got.Failed, got.Pending, tt.wantPassed, tt.wantFailed, tt.wantPending)
			}
		})
	}
}

func TestClassifyVerify(t *testing.T) {
	success := CheckConclusionSuccess
	failure := CheckConclusionFailure
	pending := func(desc string) CheckResult {
		return CheckResult{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: desc}
	}

	tests := []struct {
		name       string
		checks     []CheckResult
		wantKind   VerifyPhaseKind
		wantReason string
		wantLabel  string
	}{
		{name: "no checks", wantKind: VerifyPhaseNone},
		{name: "verifying claim", checks: []CheckResult{pending("verifying… 3f2a")}, wantKind: VerifyPhaseVerifying},
		{name: "waiting unclaimed head", checks: []CheckResult{pending("waiting: checks-pending")}, wantKind: VerifyPhaseVerifying},
		{name: "needs human with reason", checks: []CheckResult{pending("needs human: always-human-path")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "always-human-path"},
		{name: "needs human set reason is opaque", checks: []CheckResult{pending("needs human: always-human-path,human-review:ui")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "always-human-path,human-review:ui"},
		{name: "needs human colon no reason", checks: []CheckResult{pending("needs human:")}, wantKind: VerifyPhaseNeedsHuman},
		{name: "needs human no colon", checks: []CheckResult{pending("needs human")}, wantKind: VerifyPhaseNeedsHuman},
		{name: "needs human space then reason", checks: []CheckResult{pending("needs human ledger-open")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "ledger-open"},
		{name: "upper case and leading whitespace", checks: []CheckResult{pending("  NEEDS HUMAN:  repair-exhausted  ")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "repair-exhausted"},
		{name: "mixed case", checks: []CheckResult{pending("Needs Human: no-receipt")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "no-receipt"},
		{name: "needs humans is not a park", checks: []CheckResult{pending("needs humans")}, wantKind: VerifyPhaseVerifying},
		{name: "empty description", checks: []CheckResult{pending("")}, wantKind: VerifyPhaseVerifying},
		{name: "unknown description", checks: []CheckResult{pending("something else")}, wantKind: VerifyPhaseVerifying},
		{name: "in-progress status counts as pending", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusInProgress, Description: "needs human: x"}}, wantKind: VerifyPhaseNeedsHuman, wantReason: "x"},
		{name: "completed success ignored", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &success, Description: "verified"}}, wantKind: VerifyPhaseNone},
		{name: "completed failure ignored", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &failure, Description: "defect: x, needs human"}}, wantKind: VerifyPhaseNone},
		{name: "other context ignored", checks: []CheckResult{{Name: "boss/build", Status: CheckStatusQueued, Description: "needs human: x"}}, wantKind: VerifyPhaseNone},
		{name: "case-variant context ignored", checks: []CheckResult{{Name: "BOSS/VERIFY", Status: CheckStatusQueued, Description: "needs human: x"}}, wantKind: VerifyPhaseNone},
		{name: "labelled park splits codes from label", checks: []CheckResult{pending("needs human: human-review:security · verify gate rules")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "human-review:security", wantLabel: "verify gate rules"},
		{name: "labelled set park keeps every code", checks: []CheckResult{pending("needs human: always-human-path,human-review:security · verify gate rules, CI workflows")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "always-human-path,human-review:security", wantLabel: "verify gate rules, CI workflows"},
		{name: "label splits on the first separator only", checks: []CheckResult{pending("needs human: ledger-open · a · b")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "ledger-open", wantLabel: "a · b"},
		{name: "truncated label is kept as written", checks: []CheckResult{pending("needs human: ledger-open · verify gate ru…")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "ledger-open", wantLabel: "verify gate ru…"},
		{name: "middle dot without spaces is not a separator", checks: []CheckResult{pending("needs human: a·b")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "a·b"},
		{name: "needs human wins over verifying", checks: []CheckResult{pending("verifying…"), pending("needs human: park")}, wantKind: VerifyPhaseNeedsHuman, wantReason: "park"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyVerify(tt.checks)
			if got.Kind != tt.wantKind || got.Reason != tt.wantReason || got.Label != tt.wantLabel {
				t.Fatalf("ClassifyVerify = %+v, want {Kind:%d Reason:%q Label:%q}", got, tt.wantKind, tt.wantReason, tt.wantLabel)
			}
		})
	}
}

func TestVerifyPassed(t *testing.T) {
	success := CheckConclusionSuccess
	failure := CheckConclusionFailure
	ci := CheckResult{Name: "ci", Status: CheckStatusCompleted, Conclusion: &success}
	tests := []struct {
		name   string
		checks []CheckResult
		want   bool
	}{
		{name: "no checks"},
		{name: "green CI alone is not verified", checks: []CheckResult{ci}},
		{name: "verify passed", checks: []CheckResult{ci, {Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &success, Description: "verified"}}, want: true},
		{name: "operator approved the park", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &success, Description: "verified (approved)"}}, want: true},
		{name: "verify found a defect", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusCompleted, Conclusion: &failure, Description: "defect: x"}}},
		{name: "verify still verifying", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: "verifying… 3f2a"}}},
		{name: "verify parked for a human", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusQueued, Description: "needs human: human-review:ui"}}},
		{name: "completed without a conclusion", checks: []CheckResult{{Name: VerifyStatusContext, Status: CheckStatusCompleted}}},
		{name: "a build receipt is not a verdict", checks: []CheckResult{{Name: BuildReceiptContext, Status: CheckStatusCompleted, Conclusion: &success}}},
		{name: "case-variant context is not verify", checks: []CheckResult{{Name: "BOSS/VERIFY", Status: CheckStatusCompleted, Conclusion: &success}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyPassed(tt.checks); got != tt.want {
				t.Errorf("VerifyPassed = %v, want %v", got, tt.want)
			}
		})
	}
}

// Both wire representations must preserve the same hold across CI, display and pass readers.
func TestVerifyParkIndependentOfState_Sessions_AC147(t *testing.T) {
	success := CheckConclusionSuccess
	for _, status := range []CheckStatus{CheckStatusQueued, CheckStatusCompleted} {
		t.Run(map[bool]string{true: "success", false: "pending"}[status == CheckStatusCompleted], func(t *testing.T) {
			park := CheckResult{Name: VerifyStatusContext, Status: status, Conclusion: &success, Description: "needs human: human-review:security · verify rules"}
			checks := []CheckResult{{Name: "ci", Status: CheckStatusCompleted, Conclusion: &success}, park}
			phase := ClassifyVerify(checks)
			if phase.Kind != VerifyPhaseNeedsHuman || phase.Reason != "human-review:security" || phase.Label != "verify rules" {
				t.Fatalf("phase = %+v", phase)
			}
			if VerifyPassed(checks) {
				t.Fatal("park passed verify")
			}
			verdict := EvaluateChecks("head", checks, nil)
			if verdict.Total != 1 || verdict.Passed != 1 || !verdict.DemonstratedPass() {
				t.Fatalf("CI verdict = %+v", verdict)
			}
			lone := EvaluateChecks("head", []CheckResult{park}, nil)
			if lone.Total != 0 || lone.Passed != 0 || lone.DemonstratedPass() {
				t.Fatalf("lone park verdict = %+v", lone)
			}
			display := ComputeDisplayStatus(&PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)}, checks, nil)
			if display.Status != DisplayStatusNeedsHuman || display.VerifyReason != phase.Reason || display.VerifyLabel != phase.Label {
				t.Fatalf("display = %+v", display)
			}
		})
	}
}

func TestIsVerifyRepairing(t *testing.T) {
	tests := []struct {
		name, context, description string
		status                     CheckStatus
		conclusion                 *CheckConclusion
		want                       bool
	}{
		{"defect", VerifyStatusContext, "defect: 2 finding(s)", CheckStatusCompleted, conclusionPtr(CheckConclusionFailure), true},
		{"blocked", VerifyStatusContext, "blocked: 1 P1, no review", CheckStatusCompleted, conclusionPtr(CheckConclusionFailure), true},
		{"parked defect", VerifyStatusContext, "defect: 2 finding(s), needs human", CheckStatusCompleted, conclusionPtr(CheckConclusionFailure), false},
		{"parked blocked", VerifyStatusContext, "blocked: 1 P1, needs human", CheckStatusCompleted, conclusionPtr(CheckConclusionFailure), false},
		{"human park", VerifyStatusContext, "needs human: review", CheckStatusCompleted, conclusionPtr(CheckConclusionFailure), false},
		{"verifying", VerifyStatusContext, "verifying…", CheckStatusInProgress, nil, false},
		{"verified", VerifyStatusContext, "verified", CheckStatusCompleted, conclusionPtr(CheckConclusionSuccess), false},
		{"other context", "build", "defect: 2 finding(s)", CheckStatusCompleted, conclusionPtr(CheckConclusionFailure), false},
		{"pending defect", VerifyStatusContext, "defect: 2 finding(s)", CheckStatusQueued, nil, false},
		{"error defect", VerifyStatusContext, "defect: 2 finding(s)", CheckStatusCompleted, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CheckResult{Name: tt.context, Description: tt.description, Status: tt.status, Conclusion: tt.conclusion}
			if got := IsVerifyRepairing(c); got != tt.want {
				t.Errorf("IsVerifyRepairing() = %v, want %v", got, tt.want)
			}
		})
	}
}
