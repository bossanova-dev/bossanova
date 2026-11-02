package taskorchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// autoMergeRun is the observable outcome of one AUTO_MERGE task.
type autoMergeRun struct {
	provider   *mockProvider
	status     domain.TaskMappingStatus
	lastError  string
	repairRuns int32
}

// runAutoMerge routes one AUTO_MERGE task through an orchestrator whose
// provider is p, recording the final mapping status, its last error and how
// many (repair) sessions were started.
func runAutoMerge(t *testing.T, p *mockProvider, strategy string) autoMergeRun {
	t.Helper()
	run := autoMergeRun{provider: p}
	var creates atomic.Int32
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = p
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(context.Context, CreateSessionOpts) (*models.Session, error) {
				creates.Add(1)
				return &models.Session{ID: "repair-session"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*domain.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
				if params.Status != nil {
					run.status = *params.Status
				}
				if params.LastError != nil && *params.LastError != nil {
					run.lastError = **params.LastError
				}
				return &domain.TaskMapping{}, nil
			},
		}
	})
	orch.routeTask(t.Context(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:https://github.com/org/repo:42",
		Title:          "Bump lodash",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/lodash-4.17.21",
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo", mergeStrategy: strategy}, "dependabot")
	run.repairRuns = creates.Load()
	return run
}

func check(status vcs.CheckStatus, conclusion vcs.CheckConclusion) vcs.CheckResult {
	c := vcs.CheckResult{Name: "ci", Status: status}
	if status == vcs.CheckStatusCompleted {
		c.Conclusion = &conclusion
	}
	return c
}

// TestAutoMergeFreshReadNotReadyFailsWithoutMerging_Sessions_AC230101 pins that the plugin's
// AUTO_MERGE verdict alone never merges: when the merge-time read of the head
// is pending, failing or not mergeable, MergePR is not called and the mapping
// is Failed with the fresh verdict named in its details.
func TestAutoMergeFreshReadNotReadyFailsWithoutMerging_Sessions_AC230101(t *testing.T) {
	cases := map[string]struct {
		provider *mockProvider
		want     string
	}{
		"checks pending": {
			provider: &mockProvider{checks: []vcs.CheckResult{check(vcs.CheckStatusInProgress, 0)}},
			want:     "Pending",
		},
		"checks failing": {
			provider: &mockProvider{checks: []vcs.CheckResult{check(vcs.CheckStatusCompleted, vcs.CheckConclusionFailure)}},
			want:     "Failing",
		},
		"not mergeable": {
			provider: &mockProvider{status: &vcs.PRStatus{HeadSHA: testHeadSHA, Mergeable: new(false)}},
			want:     "not mergeable",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run := runAutoMerge(t, tc.provider, "merge")
			if len(run.provider.mergeOpts) != 0 {
				t.Fatalf("MergePR called %d times on a not-ready fresh read", len(run.provider.mergeOpts))
			}
			if run.status != domain.TaskMappingStatusFailed {
				t.Fatalf("mapping status = %v, want Failed", run.status)
			}
			if !strings.Contains(run.lastError, tc.want) || !strings.Contains(run.lastError, testHeadSHA) {
				t.Fatalf("last error = %q, want the fresh verdict %q on head %s", run.lastError, tc.want, testHeadSHA)
			}
		})
	}
}

// TestAutoMergePinsTheFreshHead_Sessions_AC230102 pins that a green, mergeable fresh read merges
// with ExpectedHeadSHA set to that read's head, normalized to lowercase.
func TestAutoMergePinsTheFreshHead_Sessions_AC230102(t *testing.T) {
	p := &mockProvider{status: &vcs.PRStatus{HeadSHA: " " + strings.ToUpper(testHeadSHA) + " ", Mergeable: new(true)}}
	run := runAutoMerge(t, p, "squash")
	if len(p.mergeOpts) != 1 {
		t.Fatalf("MergePR calls = %d, want 1", len(p.mergeOpts))
	}
	if got := p.mergeOpts[0]; got.ExpectedHeadSHA != testHeadSHA || got.Strategy != "squash" {
		t.Fatalf("MergePR opts = %+v, want ExpectedHeadSHA %s and strategy squash", got, testHeadSHA)
	}
	if run.status != domain.TaskMappingStatusCompleted {
		t.Fatalf("mapping status = %v, want Completed", run.status)
	}
}

// TestAutoMergeHeadMismatchFailsWithoutRepair_Sessions_AC230102 pins that a merge refused because
// the head moved after the fresh read fails the mapping and starts no rebase
// repair session, even under the rebase strategy whose refusals are repaired.
func TestAutoMergeHeadMismatchFailsWithoutRepair_Sessions_AC230102(t *testing.T) {
	p := &mockProvider{mergeFn: func(context.Context, string, int) error {
		return fmt.Errorf("%w: pull request can't be rebased: head moved", vcs.ErrHeadMismatch)
	}}
	run := runAutoMerge(t, p, "rebase")
	if len(p.mergeOpts) != 1 || p.mergeOpts[0].ExpectedHeadSHA != testHeadSHA {
		t.Fatalf("MergePR opts = %+v, want one call pinned to %s", p.mergeOpts, testHeadSHA)
	}
	if run.status != domain.TaskMappingStatusFailed {
		t.Fatalf("mapping status = %v, want Failed", run.status)
	}
	if run.repairRuns != 0 {
		t.Fatalf("repair sessions started = %d, want 0", run.repairRuns)
	}
}

// TestAutoMergeUnpinnedOnlyWhenHeadSHAIsNotFull pins the fallback: a fresh
// head that is not a full commit SHA still merges on a green verdict, unpinned.
func TestAutoMergeUnpinnedOnlyWhenHeadSHAIsNotFull(t *testing.T) {
	p := &mockProvider{status: &vcs.PRStatus{HeadSHA: "abc123"}}
	runAutoMerge(t, p, "merge")
	if len(p.mergeOpts) != 1 || p.mergeOpts[0].ExpectedHeadSHA != "" {
		t.Fatalf("MergePR opts = %+v, want one unpinned call", p.mergeOpts)
	}
}
