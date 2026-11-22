package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain/mergepolicy"
	"github.com/rs/zerolog"
)

func TestMergePR_RetriesBadGateway(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) > 1 && args[1] != "-X" {
			return "{}", nil
		}
		n := calls.Add(1)
		if n == 1 {
			return "", fmt.Errorf("gh pr merge: non-200 OK status code: 502 Bad Gateway")
		}
		return "", nil
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	if err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{Strategy: "rebase"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d calls, want 2", got)
	}
}

func TestMergePR_DoesNotRetryPermanentError(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("gh pr merge: GraphQL: Pull request is not mergeable")
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{Strategy: "rebase"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d calls, want 1", got)
	}
}

func TestMergePR_ClassifiesWorkflowScopeError(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		return "", fmt.Errorf("gh api: exit status 1: refusing to allow an OAuth App to create or update workflow `.github/workflows/_quality-gate.yml` without `workflow` scope (HTTP 403)")
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	err := p.MergePR(context.Background(), "freshclaim/marketing", 4, vcs.MergePROpts{Strategy: "rebase"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var actionable *vcs.ActionableError
	if !errors.As(err, &actionable) {
		t.Fatalf("expected actionable error, got %T: %v", err, err)
	}
	if actionable.Code != vcs.ErrorCodeGitHubWorkflowScopeRequired {
		t.Errorf("got code %q, want %q", actionable.Code, vcs.ErrorCodeGitHubWorkflowScopeRequired)
	}

	message := actionable.OperatorMessage()
	for _, want := range []string{
		"Auto-merge blocked: GitHub token lacks workflow permission",
		"PR #4 in freshclaim/marketing changes a file under .github/workflows",
		"gh auth refresh -h github.com -s workflow",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("operator message missing %q:\n%s", want, message)
		}
	}
}

// testPin is a valid 40-hex head SHA used by the head-pinned merge tests.
var testPin = strings.Repeat("0a1b", 10)

func TestMergePR_APIRequest(t *testing.T) {
	for _, strategy := range []string{"merge", "squash", "rebase", "", "unknown"} {
		t.Run(strategy, func(t *testing.T) {
			var calls [][]string
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				calls = append(calls, slices.Clone(args))
				return "{}", nil
			}))
			for _, pin := range []string{strings.ToUpper(testPin), ""} {
				calls = nil
				if err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{Strategy: strategy, ExpectedHeadSHA: pin}); err != nil {
					t.Fatal(err)
				}
				method := strategy
				if method != "rebase" && method != "squash" {
					method = "merge"
				}
				want := []string{"api", "-X", "PUT", "repos/owner/repo/pulls/42/merge", "-f", "merge_method=" + method}
				if pin != "" {
					want = append(want, "-f", "sha="+testPin)
				}
				if len(calls) != 2 || !slices.Equal(calls[0], want) {
					t.Fatalf("calls=%v want merge %v plus PR read", calls, want)
				}
				if !slices.Equal(calls[1], []string{"api", "repos/owner/repo/pulls/42", "-i"}) {
					t.Fatalf("read=%v", calls[1])
				}
			}
		})
	}
}

func TestMergePR_HeadBranchModifiedIsHeadMismatch(t *testing.T) {
	tests := []struct {
		name    string
		ghErr   string
		wantHit bool
	}{
		{"head moved", "gh pr merge 42: exit status 1: GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)", true},
		{"REST head moved", "gh: Head branch was modified. Review and try the merge again. (HTTP 409)", true},
		{"REST base 405", "gh: Base branch was modified (HTTP 405)", false},
		{"REST base 409", "gh: Base branch was modified (HTTP 409)", false},
		{"bare 409", "gh: Conflict (HTTP 409)", false},
		{"base moved", "gh pr merge 42: exit status 1: GraphQL: Base branch was modified. Review and try the merge again. (mergePullRequest)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			p := New(zerolog.Nop(),
				WithRunGH(func(_ context.Context, _ ...string) (string, error) {
					calls.Add(1)
					return "", errors.New(tt.ghErr)
				}),
				WithSleepFunc(func(time.Duration) {}),
			)

			err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{Strategy: "squash", ExpectedHeadSHA: testPin})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := errors.Is(err, vcs.ErrHeadMismatch); got != tt.wantHit {
				t.Errorf("errors.Is(err, ErrHeadMismatch) = %v, want %v (err: %v)", got, tt.wantHit, err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("got %d gh calls, want exactly 1 (no transient retry)", got)
			}
		})
	}
}

func TestMergePR_MalformedPinMakesNoGHCall(t *testing.T) {
	for _, pin := range []string{"abc", testPin[:39], testPin + "a", strings.Repeat("z", 40)} {
		t.Run(pin, func(t *testing.T) {
			var calls atomic.Int32
			p := New(zerolog.Nop(),
				WithRunGH(func(_ context.Context, _ ...string) (string, error) {
					calls.Add(1)
					return "", nil
				}),
				WithSleepFunc(func(time.Duration) {}),
			)
			if err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{ExpectedHeadSHA: pin}); err == nil {
				t.Fatal("expected error for malformed pin, got nil")
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("got %d gh calls, want 0", got)
			}
		})
	}
}

func TestMergePR_BranchCleanup(t *testing.T) {
	for _, tc := range []struct{ name, body, readError, deleteError, path string }{
		{name: "same repo escaped ref", body: `{"head":{"ref":"feat/a #?","repo":{"full_name":"OWNER/REPO"}},"base":{"repo":{"full_name":"owner/repo"}}}`, path: "repos/owner/repo/git/refs/heads/feat/a%20%23%3F"},
		{name: "fork", body: `{"head":{"ref":"work","repo":{"full_name":"fork/repo"}},"base":{"repo":{"full_name":"owner/repo"}}}`},
		{name: "missing head repo", body: `{"head":{"ref":"work"},"base":{"repo":{"full_name":"owner/repo"}}}`},
		{name: "missing base repo", body: `{"head":{"ref":"work","repo":{"full_name":"owner/repo"}}}`},
		{name: "empty ref", body: `{"head":{"repo":{"full_name":"owner/repo"}},"base":{"repo":{"full_name":"owner/repo"}}}`},
		{name: "read fails", readError: "read failed"},
		{name: "delete fails", body: `{"head":{"ref":"work","repo":{"full_name":"owner/repo"}},"base":{"repo":{"full_name":"owner/repo"}}}`, deleteError: "gh: Internal Server Error (HTTP 500)", path: "repos/owner/repo/git/refs/heads/work"},
		{name: "already deleted", body: `{"head":{"ref":"work","repo":{"full_name":"owner/repo"}},"base":{"repo":{"full_name":"owner/repo"}}}`, deleteError: "gh: Reference does not exist (HTTP 422)", path: "repos/owner/repo/git/refs/heads/work"},
		{name: "not found", body: `{"head":{"ref":"work","repo":{"full_name":"owner/repo"}},"base":{"repo":{"full_name":"owner/repo"}}}`, deleteError: "gh: Not Found (HTTP 404)", path: "repos/owner/repo/git/refs/heads/work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deleted []string
			p := New(zerolog.Nop(), WithRunGH(func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 2 && args[1] == "-X" {
					switch args[2] {
					case "PUT":
						return "{}", nil
					case "DELETE":
						deleted = slices.Clone(args)
						if tc.deleteError != "" {
							return "", errors.New(tc.deleteError)
						}
						return "", nil
					}
				}
				if vcs.CachedReadsAllowed(ctx) || vcs.ReserveQuotaReads(ctx) {
					t.Error("cleanup read kept background marks")
				}
				if tc.readError != "" {
					return "", errors.New(tc.readError)
				}
				return tc.body, nil
			}))
			ctx := vcs.WithReserveQuotaReads(vcs.WithCachedReads(context.Background()))
			if err := p.MergePR(ctx, "owner/repo", 42, vcs.MergePROpts{}); err != nil {
				t.Fatal(err)
			}
			if tc.path == "" {
				if len(deleted) != 0 {
					t.Fatalf("unexpected delete %v", deleted)
				}
				return
			}
			if want := []string{"api", "-X", "DELETE", tc.path}; !slices.Equal(deleted, want) {
				t.Fatalf("delete=%v want=%v", deleted, want)
			}
		})
	}
}

func TestMergePR_ClassifiesResponseOnly(t *testing.T) {
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		return "", &ghError{args: []string{"head branch was modified", "refusing to allow an OAuth App to create or update workflow without `workflow` scope"}, stderr: "gh: unrelated refusal (HTTP 409)", err: errors.New("exit status 1")}
	}))
	err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{})
	var actionable *vcs.ActionableError
	if err == nil || errors.Is(err, vcs.ErrHeadMismatch) || errors.As(err, &actionable) {
		t.Fatalf("argv steered classification: %v", err)
	}
}

func TestMergePR_PreservesRebaseRefusal(t *testing.T) {
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		return "", errors.New("gh: This branch can't be rebased (HTTP 405)")
	}))
	err := p.MergePR(context.Background(), "owner/repo", 42, vcs.MergePROpts{Strategy: "rebase"})
	if !mergepolicy.IsRebaseRefusal("rebase", err) {
		t.Fatalf("lost rebase refusal: %v", err)
	}
}

func TestMarkReadyForReview_API(t *testing.T) {
	for _, tc := range []struct {
		name, body              string
		wantMutation, wantError bool
	}{
		{"draft", `{"state":"open","draft":true,"node_id":"PR_node"}`, true, false},
		{"ready", `{"state":"open","draft":false}`, false, false},
		{"closed", `{"state":"closed","draft":true,"node_id":"PR_node"}`, false, true},
		{"merged", `{"state":"open","merged":true,"node_id":"PR_node"}`, false, true},
		{"missing node", `{"state":"open","draft":true}`, false, true},
		{"bad response", `not json`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				calls = append(calls, slices.Clone(args))
				if len(args) > 1 && args[1] == "graphql" {
					return "{}", nil
				}
				return tc.body, nil
			}))
			err := p.MarkReadyForReview(context.Background(), "owner/repo", 42)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v", err)
			}
			count := 1
			if tc.wantMutation {
				count = 2
			}
			if len(calls) != count {
				t.Fatalf("calls=%v", calls)
			}
			if !slices.Equal(calls[0], []string{"api", "repos/owner/repo/pulls/42", "-i"}) {
				t.Fatalf("read=%v", calls[0])
			}
			if tc.wantMutation {
				want := []string{"api", "graphql", "-f", "query=mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}", "-f", "id=PR_node"}
				if !slices.Equal(calls[1], want) {
					t.Fatalf("mutation=%v want=%v", calls[1], want)
				}
			}
		})
	}
}

func TestMarkReadyForReview_ForegroundReadBelowFloor(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(strings.Replace(graphQLAt(clock, 1, 20*time.Minute), `"remaining":4999`, `"limit":5000,"remaining":1`, 1))
	prior := gh.reply
	gh.reply = func(args []string) (string, error) {
		if slices.Contains(args, "repos/o/r/pulls/7") {
			return `{"state":"open","draft":true,"node_id":"PR_node"}`, nil
		}
		if len(args) > 1 && args[1] == "graphql" {
			return "{}", nil
		}
		return prior(args)
	}
	p := newFloorProvider(gh, clock)
	ctx := vcs.WithReserveQuotaReads(vcs.WithCachedReads(context.Background()))
	if _, err := p.readRESTPullRequest(ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("precondition: background read=%v", err)
	}
	if err := p.MarkReadyForReview(ctx, "o/r", 7); err != nil {
		t.Fatal(err)
	}
	if got := gh.count(func(args []string) bool { return slices.Contains(args, "repos/o/r/pulls/7") }); got != 1 {
		t.Fatalf("PR reads=%d", got)
	}
	if got := gh.count(func(args []string) bool { return len(args) > 1 && args[1] == "graphql" }); got != 1 {
		t.Fatalf("mutations=%d", got)
	}
}
