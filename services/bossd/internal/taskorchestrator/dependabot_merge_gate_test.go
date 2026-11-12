package taskorchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session/checkpolicy"
)

type mergePolicyFixture struct {
	required vcs.RequiredChecks
	policy   checkpolicy.DependabotPolicy
}

func (p mergePolicyFixture) RequiredChecks(context.Context, string) vcs.RequiredChecks {
	return p.required
}
func (p mergePolicyFixture) DependabotPolicy(context.Context, string) checkpolicy.DependabotPolicy {
	return p.policy
}

type mergeInspectionFixture struct {
	result domain.PRHeadInspection
	err    error
	calls  int
}

func (i *mergeInspectionFixture) InspectPRHead(context.Context, string, string, string, string) (domain.PRHeadInspection, error) {
	i.calls++
	return i.result, i.err
}

type mergeReadCounter struct {
	mockProvider
	statuses, checksRead int
}

func (p *mergeReadCounter) GetPRStatus(ctx context.Context, repo string, id int) (*vcs.PRStatus, error) {
	p.statuses++
	return p.mockProvider.GetPRStatus(ctx, repo, id)
}
func (p *mergeReadCounter) GetCheckResults(ctx context.Context, repo string, id int) ([]vcs.CheckResult, error) {
	p.checksRead++
	return p.mockProvider.GetCheckResults(ctx, repo, id)
}

func TestDependabotMergeRequiredChecks_Sessions_AC248301(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []vcs.CheckResult
		reason string
	}{
		{"missing", []vcs.CheckResult{}, "not-requested"},
		{"skipped", []vcs.CheckResult{check(vcs.CheckStatusCompleted, vcs.CheckConclusionSkipped)}, "not-requested"},
		{"neutral", []vcs.CheckResult{check(vcs.CheckStatusCompleted, vcs.CheckConclusionNeutral)}, "not-requested"},
		{"failing", []vcs.CheckResult{check(vcs.CheckStatusCompleted, vcs.CheckConclusionFailure)}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &mockProvider{checks: tc.checks}
			ins := &mergeInspectionFixture{result: domain.PRHeadInspection{ContainsBase: true}}
			policy := mergePolicyFixture{policy: checkpolicy.ParseDependabot([]byte(`{"ci":{"dependabot":{"required":[{"name":"ci"}]}}}`))}
			o := newTestOrchestrator()
			o.provider = p
			o.SetDependabotGate(policy, ins)
			var lastError string
			var status domain.TaskMappingStatus
			o.taskMappings = &mockTaskMappingStore{updateFn: func(_ context.Context, _ string, p db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
				if p.LastError != nil && *p.LastError != nil {
					lastError = **p.LastError
				}
				if p.Status != nil {
					status = *p.Status
				}
				return &domain.TaskMapping{}, nil
			}}
			if o.mergeConfirmedHead(t.Context(), repoInfo{baseBranch: "main"}, &domain.TaskMapping{ID: "m"}, 1, "merge", func(error, string) { t.Fatal("unexpected refresh") }) || len(p.mergeOpts) != 0 || status != domain.TaskMappingStatusFailed || !strings.Contains(lastError, tc.reason) || !strings.Contains(lastError, "ci") {
				t.Fatalf("merged=%+v status=%v error=%s", p.mergeOpts, status, lastError)
			}
		})
	}
}

func TestDependabotMergePathScoped_Sessions_AC248302(t *testing.T) {
	policy := mergePolicyFixture{policy: checkpolicy.ParseDependabot([]byte(`{"ci":{"dependabot":{"required":[{"name":"lock","paths":["pnpm-lock.yaml","**/package.json"]}]}}}`))}
	for _, file := range []string{"go.mod", "pnpm-lock.yaml", "app/package.json"} {
		t.Run(file, func(t *testing.T) {
			p := &mockProvider{}
			o := newTestOrchestrator()
			o.provider = p
			o.SetDependabotGate(policy, &mergeInspectionFixture{result: domain.PRHeadInspection{ContainsBase: true, ChangedFiles: []string{file}}})
			_, err := o.confirmAutoMergeHead(t.Context(), repoInfo{baseBranch: "main"}, 1, "merge")
			if (err == nil) != (file == "go.mod") {
				t.Fatalf("%s: %v", file, err)
			}
		})
	}
}

func TestDependabotMergeStaleBase_Sessions_AC248303(t *testing.T) {
	p := &mockProvider{status: &vcs.PRStatus{HeadSHA: testHeadSHA}, checks: []vcs.CheckResult{}}
	o := newTestOrchestrator()
	o.provider = p
	o.SetDependabotGate(mergePolicyFixture{policy: checkpolicy.ParseDependabot([]byte(`{"ci":{"dependabot":{}}}`))}, &mergeInspectionFixture{})
	refreshes := 0
	merged := o.mergeConfirmedHead(t.Context(), repoInfo{baseBranch: "main"}, &domain.TaskMapping{ID: "m"}, 1, "squash", func(err error, sha string) {
		refreshes++
		if sha != testHeadSHA {
			t.Errorf("refresh SHA=%q", sha)
		}
	})
	if merged || len(p.mergeOpts) != 0 || refreshes != 1 {
		t.Fatalf("merged=%v calls=%d refreshes=%d", merged, len(p.mergeOpts), refreshes)
	}
}

func TestDependabotMergeRequiredAndLegacy_Sessions_AC248304(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		required   vcs.RequiredChecks
		wantError  bool
	}{
		{"legacy", "main", vcs.RequiredChecks{}, false},
		{"required", "main", vcs.RequiredChecks{Names: []string{"missing"}}, true},
		{"exempt", "staging", vcs.RequiredChecks{Names: []string{"missing"}, ExemptBases: []string{"staging"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &mockProvider{}
			ins := &mergeInspectionFixture{}
			o := newTestOrchestrator()
			o.provider = p
			o.SetDependabotGate(mergePolicyFixture{required: tc.required}, ins)
			merged := o.mergeConfirmedHead(t.Context(), repoInfo{baseBranch: tc.base}, &domain.TaskMapping{ID: "m"}, 1, "merge", func(error, string) { t.Fatal("unexpected refresh") })
			if merged == tc.wantError || ins.calls != 0 {
				t.Fatalf("merged=%v inspections=%d", merged, ins.calls)
			}
			if merged && (len(p.mergeOpts) != 1 || p.mergeOpts[0].ExpectedHeadSHA != testHeadSHA) {
				t.Fatalf("merge not pinned: %+v", p.mergeOpts)
			}
			if !merged && len(p.mergeOpts) != 0 {
				t.Fatalf("required check bypassed: %+v", p.mergeOpts)
			}
		})
	}
}

func TestDependabotMergeFailClosed_Sessions_AC248305(t *testing.T) {
	for _, tc := range []struct {
		name, config, sha string
		err               error
	}{
		{"malformed", `{"ci":{"dependabot":false}}`, testHeadSHA, nil},
		{"inspection", `{"ci":{"dependabot":{}}}`, testHeadSHA, errors.New("fetch failed")},
		{"short sha", `{"ci":{"dependabot":{}}}`, "abc", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &mockProvider{status: &vcs.PRStatus{HeadSHA: tc.sha}}
			o := newTestOrchestrator()
			o.provider = p
			o.SetDependabotGate(mergePolicyFixture{policy: checkpolicy.ParseDependabot([]byte(tc.config))}, &mergeInspectionFixture{result: domain.PRHeadInspection{ContainsBase: true}, err: tc.err})
			if _, err := o.confirmAutoMergeHead(t.Context(), repoInfo{baseBranch: "main"}, 1, "merge"); err == nil {
				t.Fatal("merge admitted")
			}
		})
	}
}

func TestDependabotMergeReadBudget_Sessions_AC248306(t *testing.T) {
	p := &mergeReadCounter{}
	o := newTestOrchestrator()
	o.provider = p
	ins := &mergeInspectionFixture{result: domain.PRHeadInspection{ContainsBase: true}}
	o.SetDependabotGate(mergePolicyFixture{policy: checkpolicy.ParseDependabot([]byte(`{"ci":{"dependabot":{"required":[{"name":"ci"}]}}}`))}, ins)
	if !o.mergeConfirmedHead(t.Context(), repoInfo{baseBranch: "main"}, &domain.TaskMapping{ID: "m"}, 1, "merge", func(error, string) { t.Fatal("refresh") }) {
		t.Fatal("green head did not merge")
	}
	if p.statuses != 1 || p.checksRead != 1 || ins.calls != 1 || len(p.mergeOpts) != 1 || p.mergeOpts[0].ExpectedHeadSHA != testHeadSHA {
		t.Fatalf("status=%d checks=%d inspect=%d merge=%+v", p.statuses, p.checksRead, ins.calls, p.mergeOpts)
	}
}
