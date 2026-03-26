package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

func TestParsePRNumberFromURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    int
		wantErr bool
	}{
		{
			name: "standard PR URL",
			url:  "https://github.com/owner/repo/pull/42",
			want: 42,
		},
		{
			name: "trailing slash",
			url:  "https://github.com/owner/repo/pull/7/",
			want: 7,
		},
		{
			name:    "not a PR URL",
			url:     "https://github.com/owner/repo/issues/5",
			wantErr: true,
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePRNumberFromURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePRNumberFromURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parsePRNumberFromURL(%q) = %d, want %d", tt.url, got, tt.want)
			}
		})
	}
}

func TestParsePRState(t *testing.T) {
	tests := []struct {
		input string
		want  vcs.PRState
	}{
		{"OPEN", vcs.PRStateOpen},
		{"open", vcs.PRStateOpen},
		{"CLOSED", vcs.PRStateClosed},
		{"closed", vcs.PRStateClosed},
		{"MERGED", vcs.PRStateMerged},
		{"merged", vcs.PRStateMerged},
		{"unknown", vcs.PRStateOpen},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parsePRState(tt.input); got != tt.want {
				t.Errorf("parsePRState(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCheckState(t *testing.T) {
	tests := []struct {
		input          string
		wantStatus     vcs.CheckStatus
		wantConclusion *vcs.CheckConclusion
	}{
		// Terminal states — completed with conclusion.
		{"SUCCESS", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionSuccess)},
		{"FAILURE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure)},
		{"STARTUP_FAILURE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure)},
		{"STALE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure)},
		{"NEUTRAL", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionNeutral)},
		{"CANCELLED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionCancelled)},
		{"SKIPPED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionSkipped)},
		{"TIMED_OUT", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionTimedOut)},
		// In-progress states — no conclusion.
		{"IN_PROGRESS", vcs.CheckStatusInProgress, nil},
		{"QUEUED", vcs.CheckStatusQueued, nil},
		{"PENDING", vcs.CheckStatusQueued, nil},
		{"WAITING", vcs.CheckStatusQueued, nil},
		// Unknown defaults to queued.
		{"unknown", vcs.CheckStatusQueued, nil},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotStatus, gotConclusion := parseCheckState(tt.input)
			if gotStatus != tt.wantStatus {
				t.Errorf("parseCheckState(%q) status = %v, want %v", tt.input, gotStatus, tt.wantStatus)
			}
			if (gotConclusion == nil) != (tt.wantConclusion == nil) {
				t.Errorf("parseCheckState(%q) conclusion = %v, want %v", tt.input, gotConclusion, tt.wantConclusion)
				return
			}
			if gotConclusion != nil && *gotConclusion != *tt.wantConclusion {
				t.Errorf("parseCheckState(%q) conclusion = %v, want %v", tt.input, *gotConclusion, *tt.wantConclusion)
			}
		})
	}
}

func TestParseReviewState(t *testing.T) {
	tests := []struct {
		input string
		want  vcs.ReviewState
	}{
		{"APPROVED", vcs.ReviewStateApproved},
		{"CHANGES_REQUESTED", vcs.ReviewStateChangesRequested},
		{"COMMENTED", vcs.ReviewStateCommented},
		{"DISMISSED", vcs.ReviewStateDismissed},
		{"unknown", vcs.ReviewStateCommented},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseReviewState(tt.input); got != tt.want {
				t.Errorf("parseReviewState(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsRepoNotReady(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("network timeout"),
			want: false,
		},
		{
			name: "head sha blank",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: Head sha can't be blank (createPullRequest)"),
			want: true,
		},
		{
			name: "base sha blank",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: Base sha can't be blank (createPullRequest)"),
			want: true,
		},
		{
			name: "no commits between branches",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: No commits between main and my-branch (createPullRequest)"),
			want: true,
		},
		{
			name: "combined GitHub error",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: Head sha can't be blank, Base sha can't be blank, No commits between main and plan-meta-ads-automation, Head ref must be a branch (createPullRequest)"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRepoNotReady(tt.err); got != tt.want {
				t.Errorf("isRepoNotReady(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// testPROpts returns a CreatePROpts suitable for testing.
func testPROpts() vcs.CreatePROpts {
	return vcs.CreatePROpts{
		RepoPath:   "https://github.com/owner/repo",
		HeadBranch: "feature",
		BaseBranch: "main",
		Title:      "Test PR",
		Body:       "body",
		Draft:      true,
	}
}

func TestCreateDraftPR_RetrySuccess(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "", fmt.Errorf("gh pr create: GraphQL: Head sha can't be blank")
		}
		return "https://github.com/owner/repo/pull/1\n", nil
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	info, err := p.CreateDraftPR(context.Background(), testPROpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Number != 1 {
		t.Errorf("got PR number %d, want 1", info.Number)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d calls, want 2", got)
	}
}

func TestCreateDraftPR_RetriesExhausted(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("gh pr create: GraphQL: Head sha can't be blank")
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	_, err := p.CreateDraftPR(context.Background(), testPROpts())
	if !errors.Is(err, vcs.ErrRepoNotReady) {
		t.Errorf("got error %v, want ErrRepoNotReady", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("got %d calls, want 3", got)
	}
}

func TestCreateDraftPR_NoRetryForOtherErrors(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("gh pr create: exit status 1: HTTP 422")
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	_, err := p.CreateDraftPR(context.Background(), testPROpts())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, vcs.ErrRepoNotReady) {
		t.Error("should not be ErrRepoNotReady")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d calls, want 1", got)
	}
}

func TestCreateDraftPR_RespectsContextCancellation(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("gh pr create: GraphQL: Head sha can't be blank")
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {
			cancel() // cancel during sleep
		}),
	)

	_, err := p.CreateDraftPR(ctx, testPROpts())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got error %v, want context.Canceled", err)
	}
	// Should have made 1 call, then attempted to sleep, then seen cancellation.
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d calls, want 1", got)
	}
}

// graphqlThreadsResponse builds a fake GraphQL response with the given threads.
// Each thread is (isResolved bool, authorLogin string).
func graphqlThreadsResponse(threads ...struct {
	resolved bool
	author   string
}) string {
	nodes := make([]string, len(threads))
	for i, t := range threads {
		nodes[i] = fmt.Sprintf(
			`{"isResolved":%t,"comments":{"nodes":[{"author":{"login":%q}}]}}`,
			t.resolved, t.author,
		)
	}
	return fmt.Sprintf(
		`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s]}}}}}`,
		strings.Join(nodes, ","),
	)
}

func TestGetReviewComments_BotWithUnresolvedThreads(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			// Verify variables are passed as individual flags, not as a
			// single JSON "variables=..." string. gh expects -f for strings
			// and -F for non-string JSON values.
			argsStr := strings.Join(args, " ")
			if strings.Contains(argsStr, "variables=") {
				t.Error("GraphQL variables must be passed as individual -f/-F flags, not as variables=JSON")
			}
			if !strings.Contains(argsStr, "owner=owner") {
				t.Error("expected -f owner=owner in GraphQL args")
			}
			if !strings.Contains(argsStr, "repo=repo") {
				t.Error("expected -f repo=repo in GraphQL args")
			}
			if !strings.Contains(argsStr, "pr=1") {
				t.Error("expected -F pr=1 in GraphQL args")
			}
			return graphqlThreadsResponse(
				struct {
					resolved bool
					author   string
				}{false, "cursor"},
				struct {
					resolved bool
					author   string
				}{true, "cursor"},
			), nil
		}
		return `[
			{"user":{"login":"cursor[bot]"},"body":"found issues","state":"COMMENTED"},
			{"user":{"login":"human-reviewer"},"body":"looks good","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].State != vcs.ReviewStateChangesRequested {
		t.Errorf("bot comment state = %v, want ChangesRequested", comments[0].State)
	}
	if comments[1].State != vcs.ReviewStateCommented {
		t.Errorf("human comment state = %v, want Commented", comments[1].State)
	}
}

func TestGetReviewComments_BotAllThreadsResolved(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			return graphqlThreadsResponse(
				struct {
					resolved bool
					author   string
				}{true, "cursor"},
				struct {
					resolved bool
					author   string
				}{true, "cursor"},
			), nil
		}
		return `[
			{"user":{"login":"cursor[bot]"},"body":"found issues","state":"COMMENTED"},
			{"user":{"login":"human-reviewer"},"body":"lgtm","state":"APPROVED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	// All threads resolved — should NOT be promoted.
	if comments[0].State != vcs.ReviewStateCommented {
		t.Errorf("bot comment state = %v, want Commented (all threads resolved)", comments[0].State)
	}
	if comments[1].State != vcs.ReviewStateApproved {
		t.Errorf("human comment state = %v, want Approved", comments[1].State)
	}
}

func TestGetReviewComments_BotNoThreads(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			// No threads at all.
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`, nil
		}
		return `[
			{"user":{"login":"cursor[bot]"},"body":"no issues found","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	// No threads — should NOT be promoted.
	if comments[0].State != vcs.ReviewStateCommented {
		t.Errorf("bot comment state = %v, want Commented (no threads)", comments[0].State)
	}
}

func TestGetReviewComments_GraphQLFailure(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			return "", fmt.Errorf("gh api graphql: exit status 1: network error")
		}
		return `[
			{"user":{"login":"cursor[bot]"},"body":"found issues","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	// GraphQL failed — fail-open means promote.
	if comments[0].State != vcs.ReviewStateChangesRequested {
		t.Errorf("bot comment state = %v, want ChangesRequested (fail-open)", comments[0].State)
	}
}

func TestGetReviewComments_NoBotReviews(t *testing.T) {
	var graphQLCalled bool
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			graphQLCalled = true
			return "", fmt.Errorf("should not be called")
		}
		return `[
			{"user":{"login":"human-reviewer"},"body":"looks good","state":"APPROVED"},
			{"user":{"login":"another-human"},"body":"nit","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if graphQLCalled {
		t.Error("GraphQL should not be called when there are no bot reviews")
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].State != vcs.ReviewStateApproved {
		t.Errorf("comment[0] state = %v, want Approved", comments[0].State)
	}
	if comments[1].State != vcs.ReviewStateCommented {
		t.Errorf("comment[1] state = %v, want Commented", comments[1].State)
	}
}

func TestGetReviewComments_MultipleBotsMixed(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			// cursor has unresolved thread, cubic-dev-ai all resolved.
			return graphqlThreadsResponse(
				struct {
					resolved bool
					author   string
				}{false, "cursor"},
				struct {
					resolved bool
					author   string
				}{true, "cubic-dev-ai"},
			), nil
		}
		return `[
			{"user":{"login":"cursor[bot]"},"body":"issue found","state":"COMMENTED"},
			{"user":{"login":"cubic-dev-ai[bot]"},"body":"issue found","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	// cursor[bot] has unresolved threads — promoted.
	if comments[0].State != vcs.ReviewStateChangesRequested {
		t.Errorf("cursor[bot] state = %v, want ChangesRequested", comments[0].State)
	}
	// cubic-dev-ai[bot] all resolved — NOT promoted.
	if comments[1].State != vcs.ReviewStateCommented {
		t.Errorf("cubic-dev-ai[bot] state = %v, want Commented", comments[1].State)
	}
}

func TestSplitNWO(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"standard", "owner/repo", "owner", "repo", true},
		{"with org", "my-org/my-repo", "my-org", "my-repo", true},
		{"empty string", "", "", "", false},
		{"no slash", "ownerrepo", "", "", false},
		{"empty owner", "/repo", "", "", false},
		{"empty repo", "owner/", "", "", false},
		{"multiple slashes", "owner/repo/extra", "owner", "repo/extra", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, ok := splitNWO(tt.input)
			if ok != tt.wantOK {
				t.Errorf("splitNWO(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
				return
			}
			if owner != tt.wantOwner {
				t.Errorf("splitNWO(%q) owner = %q, want %q", tt.input, owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("splitNWO(%q) repo = %q, want %q", tt.input, repo, tt.wantRepo)
			}
		})
	}
}
