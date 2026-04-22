package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// fixture loads a testdata file into memory. Tests use this to drive the
// fake gh runner below: each assertion about the provider's behaviour is
// ultimately an assertion about how the provider transforms these fixtures.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// ghCall captures a single gh CLI invocation for post-hoc assertions.
type ghCall struct {
	Args []string
}

// fakeGH builds a ghFunc that routes requests to responses based on a simple
// arg-matcher. The first matching responder wins. If none match, the call
// fails the test — better than silently returning empty JSON and masking a
// missing expectation.
type ghResponder struct {
	match func(args []string) bool
	// Either stdout or err; if err is non-nil it is returned directly.
	stdout string
	err    error
}

type fakeGH struct {
	mu         sync.Mutex
	t          *testing.T
	responders []ghResponder
	calls      []ghCall
}

func newFakeGH(t *testing.T) *fakeGH {
	return &fakeGH{t: t}
}

func (f *fakeGH) expect(r ghResponder) {
	f.responders = append(f.responders, r)
}

func (f *fakeGH) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ghCall{Args: append([]string{}, args...)})
	for _, r := range f.responders {
		if r.match(args) {
			return r.stdout, r.err
		}
	}
	f.t.Fatalf("unexpected gh call: %v", args)
	return "", nil
}

func (f *fakeGH) callsContaining(s string) []ghCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ghCall
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.Args, " "), s) {
			out = append(out, c)
		}
	}
	return out
}

// argsStartWith returns a matcher that compares the first N args.
func argsStartWith(want ...string) func([]string) bool {
	return func(args []string) bool {
		if len(args) < len(want) {
			return false
		}
		for i, w := range want {
			if args[i] != w {
				return false
			}
		}
		return true
	}
}

func newProvider(f *fakeGH) *Provider {
	return New(zerolog.Nop(), WithRunGH(f.run))
}

const testRepo = "owner/repo"

func TestE2E_GitHub_CreateDraftPR(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match:  argsStartWith("pr", "create"),
		stdout: fixture(t, "pr_create_response.txt"),
	})
	p := newProvider(f)

	info, err := p.CreateDraftPR(context.Background(), vcs.CreatePROpts{
		RepoPath:   testRepo,
		HeadBranch: "feature/dark-mode",
		BaseBranch: "main",
		Title:      "Add dark mode support",
		Body:       "This PR adds...",
		Draft:      true,
	})
	if err != nil {
		t.Fatalf("CreateDraftPR: %v", err)
	}

	if info.Number != 42 {
		t.Errorf("expected PR #42, got %d", info.Number)
	}
	if !strings.HasSuffix(info.URL, "/pull/42") {
		t.Errorf("expected URL to end with /pull/42, got %s", info.URL)
	}

	// Confirm --draft actually made it into the request args.
	if len(f.callsContaining("--draft")) != 1 {
		t.Errorf("expected exactly one gh call with --draft, got %d", len(f.callsContaining("--draft")))
	}
}

func TestE2E_GitHub_GetPRStatus_AllStates(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		wantState vcs.PRState
		wantDraft bool
		// nil = Mergeable unset (UNKNOWN); true/false = expected value.
		wantMergeable *bool
	}{
		{"draft", "pr_status_draft.json", vcs.PRStateOpen, true, nil},
		{"open-checks-running", "pr_status_open_checks_running.json", vcs.PRStateOpen, false, nil},
		{"mergeable", "pr_status_mergeable.json", vcs.PRStateOpen, false, ptrBool(true)},
		{"conflict", "pr_status_conflict.json", vcs.PRStateOpen, false, ptrBool(false)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.expect(ghResponder{
				match:  argsStartWith("pr", "view"),
				stdout: fixture(t, tc.fixture),
			})
			p := newProvider(f)

			status, err := p.GetPRStatus(context.Background(), testRepo, 42)
			if err != nil {
				t.Fatalf("GetPRStatus: %v", err)
			}
			if status.State != tc.wantState {
				t.Errorf("State: got %v, want %v", status.State, tc.wantState)
			}
			if status.Draft != tc.wantDraft {
				t.Errorf("Draft: got %v, want %v", status.Draft, tc.wantDraft)
			}
			if tc.wantMergeable == nil {
				if status.Mergeable != nil {
					t.Errorf("Mergeable: got %v, want nil", *status.Mergeable)
				}
			} else {
				if status.Mergeable == nil {
					t.Errorf("Mergeable: got nil, want %v", *tc.wantMergeable)
				} else if *status.Mergeable != *tc.wantMergeable {
					t.Errorf("Mergeable: got %v, want %v", *status.Mergeable, *tc.wantMergeable)
				}
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

func TestE2E_GitHub_GetCheckResults_Passing(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match:  argsStartWith("pr", "checks"),
		stdout: fixture(t, "check_runs_passing.json"),
	})
	p := newProvider(f)

	results, err := p.GetCheckResults(context.Background(), testRepo, 42)
	if err != nil {
		t.Fatalf("GetCheckResults: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 check results, got %d", len(results))
	}
	for _, r := range results {
		if r.Status != vcs.CheckStatusCompleted {
			t.Errorf("%s: expected completed, got %v", r.Name, r.Status)
		}
		if r.Conclusion == nil || *r.Conclusion != vcs.CheckConclusionSuccess {
			t.Errorf("%s: expected success, got %v", r.Name, r.Conclusion)
		}
	}
}

func TestE2E_GitHub_GetCheckResults_Failing_WithLogs(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match:  argsStartWith("pr", "checks"),
		stdout: fixture(t, "check_runs_failing.json"),
	})
	f.expect(ghResponder{
		match: func(args []string) bool {
			return len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/logs")
		},
		stdout: fixture(t, "check_run_log.txt"),
	})
	p := newProvider(f)

	results, err := p.GetCheckResults(context.Background(), testRepo, 42)
	if err != nil {
		t.Fatalf("GetCheckResults: %v", err)
	}

	// Locate the failing check and fetch its logs.
	var failed *vcs.CheckResult
	for i := range results {
		if results[i].Conclusion != nil && *results[i].Conclusion == vcs.CheckConclusionFailure {
			failed = &results[i]
			break
		}
	}
	if failed == nil {
		t.Fatal("no failing check found in results")
	}
	logs, err := p.GetFailedCheckLogs(context.Background(), testRepo, failed.ID)
	if err != nil {
		t.Fatalf("GetFailedCheckLogs: %v", err)
	}
	if !strings.Contains(logs, "FAIL: TestSomething") {
		t.Errorf("expected log to contain 'FAIL: TestSomething', got: %q", logs)
	}
}

func TestE2E_GitHub_GetReviewComments(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match: func(args []string) bool {
			return len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/reviews")
		},
		stdout: fixture(t, "reviews_with_comments.json"),
	})
	p := newProvider(f)

	comments, err := p.GetReviewComments(context.Background(), testRepo, 42)
	if err != nil {
		t.Fatalf("GetReviewComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("expected 3 review comments, got %d", len(comments))
	}

	wantStates := []vcs.ReviewState{
		vcs.ReviewStateApproved,
		vcs.ReviewStateChangesRequested,
		vcs.ReviewStateCommented,
	}
	for i, c := range comments {
		if c.State != wantStates[i] {
			t.Errorf("comment %d: state=%v, want %v", i, c.State, wantStates[i])
		}
	}
	if comments[0].Author != "alice" {
		t.Errorf("first comment author: got %q, want alice", comments[0].Author)
	}
}

func TestE2E_GitHub_MergePR_AllStrategies(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		wantFlag string
	}{
		{"merge", "merge", "--merge"},
		{"rebase", "rebase", "--rebase"},
		{"squash", "squash", "--squash"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.expect(ghResponder{
				match:  argsStartWith("pr", "merge"),
				stdout: "",
			})
			p := newProvider(f)

			if err := p.MergePR(context.Background(), testRepo, 42, tc.strategy); err != nil {
				t.Fatalf("MergePR: %v", err)
			}

			// The strategy flag must appear in the gh arg list so a regression in
			// flag mapping is caught even if the merge stubbing passes.
			if len(f.callsContaining(tc.wantFlag)) != 1 {
				t.Errorf("expected exactly one gh call containing %q, got calls=%v",
					tc.wantFlag, f.calls)
			}
		})
	}
}

func TestE2E_GitHub_MergePR_ConflictErrorParsed(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match: argsStartWith("pr", "merge"),
		err:   fmt.Errorf("gh pr merge: Pull Request is not mergeable: the merge commit cannot be cleanly created"),
	})
	p := newProvider(f)

	err := p.MergePR(context.Background(), testRepo, 42, "merge")
	if err == nil {
		t.Fatal("expected error on conflict, got nil")
	}
	if !strings.Contains(err.Error(), "not mergeable") {
		t.Errorf("expected error to mention 'not mergeable', got: %v", err)
	}
}

func TestE2E_GitHub_MarkReadyForReview(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match:  argsStartWith("pr", "ready"),
		stdout: "",
	})
	p := newProvider(f)

	if err := p.MarkReadyForReview(context.Background(), testRepo, 42); err != nil {
		t.Fatalf("MarkReadyForReview: %v", err)
	}
	if len(f.callsContaining("ready")) != 1 {
		t.Errorf("expected exactly one gh call with 'ready', got %d", len(f.callsContaining("ready")))
	}
}

func TestE2E_GitHub_UpdatePRTitle(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match:  argsStartWith("pr", "edit"),
		stdout: "",
	})
	p := newProvider(f)

	newTitle := "Updated: Add dark mode"
	if err := p.UpdatePRTitle(context.Background(), testRepo, 42, newTitle); err != nil {
		t.Fatalf("UpdatePRTitle: %v", err)
	}
	if len(f.callsContaining(newTitle)) != 1 {
		t.Errorf("expected exactly one gh call containing the new title, got %d", len(f.callsContaining(newTitle)))
	}
}

func TestE2E_GitHub_ListOpenPRs(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match: func(args []string) bool {
			if len(args) < 2 || args[0] != "pr" || args[1] != "list" {
				return false
			}
			for _, a := range args {
				if a == "open" {
					return true
				}
			}
			return false
		},
		stdout: fixture(t, "list_open_prs.json"),
	})
	p := newProvider(f)

	prs, err := p.ListOpenPRs(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %d", len(prs))
	}
	if prs[0].Number != 42 || prs[1].Number != 43 {
		t.Errorf("unexpected PR numbers: %+v", prs)
	}
	for _, pr := range prs {
		if pr.State != vcs.PRStateOpen {
			t.Errorf("PR %d: state=%v, want Open", pr.Number, pr.State)
		}
	}
}

func TestE2E_GitHub_ListClosedPRs(t *testing.T) {
	f := newFakeGH(t)
	f.expect(ghResponder{
		match: func(args []string) bool {
			if len(args) < 2 || args[0] != "pr" || args[1] != "list" {
				return false
			}
			for _, a := range args {
				if a == "closed" {
					return true
				}
			}
			return false
		},
		stdout: fixture(t, "list_closed_prs.json"),
	})
	p := newProvider(f)

	prs, err := p.ListClosedPRs(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("ListClosedPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 closed PR, got %d", len(prs))
	}
	if prs[0].State != vcs.PRStateClosed {
		t.Errorf("expected Closed state, got %v", prs[0].State)
	}
}

// Sanity check: ensure the provider surfaces fixture-driven errors correctly.
func TestE2E_GitHub_ErrorPropagation(t *testing.T) {
	f := newFakeGH(t)
	wantErr := errors.New("gh: auth token missing")
	f.expect(ghResponder{
		match: argsStartWith("pr", "view"),
		err:   wantErr,
	})
	p := newProvider(f)

	_, err := p.GetPRStatus(context.Background(), testRepo, 42)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "auth token missing") {
		t.Errorf("error should surface gh message, got: %v", err)
	}
}
