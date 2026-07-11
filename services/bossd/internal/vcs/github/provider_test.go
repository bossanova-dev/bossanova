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
		wantRecognized bool
	}{
		// Terminal states — completed with conclusion.
		{"SUCCESS", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionSuccess), true},
		{"FAILURE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), true},
		{"STARTUP_FAILURE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), true},
		{"STALE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), true},
		// ACTION_REQUIRED and ERROR were previously falling through to
		// the default branch, which silently treated them as "queued" and
		// hid real failures from the repair plugin. They now map to Failure.
		{"ACTION_REQUIRED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), true},
		{"ERROR", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), true},
		{"NEUTRAL", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionNeutral), true},
		{"CANCELLED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionCancelled), true},
		{"SKIPPED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionSkipped), true},
		{"TIMED_OUT", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionTimedOut), true},
		// In-progress states — no conclusion.
		{"IN_PROGRESS", vcs.CheckStatusInProgress, nil, true},
		{"QUEUED", vcs.CheckStatusQueued, nil, true},
		{"PENDING", vcs.CheckStatusQueued, nil, true},
		{"WAITING", vcs.CheckStatusQueued, nil, true},
		// Unknown values fail safe to Failure (recognized=false) so a new
		// gh state we forgot to enumerate doesn't silently mask repairs.
		{"unknown", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), false},
		{"", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure), false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotStatus, gotConclusion, gotRecognized := parseCheckState(tt.input)
			if gotStatus != tt.wantStatus {
				t.Errorf("parseCheckState(%q) status = %v, want %v", tt.input, gotStatus, tt.wantStatus)
			}
			if gotRecognized != tt.wantRecognized {
				t.Errorf("parseCheckState(%q) recognized = %v, want %v", tt.input, gotRecognized, tt.wantRecognized)
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
		{"PENDING", vcs.ReviewStateUnspecified},
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

func TestParseReviewDecision(t *testing.T) {
	tests := []struct {
		input string
		want  vcs.ReviewState
	}{
		{"APPROVED", vcs.ReviewStateApproved},
		{"CHANGES_REQUESTED", vcs.ReviewStateChangesRequested},
		{"REVIEW_REQUIRED", vcs.ReviewStateRequired},
		{"", vcs.ReviewStateUnspecified},
		{"unknown", vcs.ReviewStateUnspecified},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseReviewDecision(tt.input); got != tt.want {
				t.Errorf("parseReviewDecision(%q) = %v, want %v", tt.input, got, tt.want)
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

func TestIsPRAlreadyExists(t *testing.T) {
	err := fmt.Errorf(`gh pr create: exit status 1: pull request create failed: GraphQL: a pull request for branch "owner:feature" into branch "main" already exists`)
	if !isPRAlreadyExists(err) {
		t.Fatal("expected duplicate PR error")
	}
}

func TestIsPRAlreadyExistsFalseForOtherErrors(t *testing.T) {
	err := fmt.Errorf("gh pr create: exit status 1: authentication failed")
	if isPRAlreadyExists(err) {
		t.Fatal("expected auth error not to be duplicate PR error")
	}
}

func TestCreateDraftPRReturnsErrPRAlreadyExistsForDuplicatePR(t *testing.T) {
	p := New(zerolog.Nop(),
		WithRunGH(func(_ context.Context, _ ...string) (string, error) {
			return "", errors.New(`gh pr create: exit status 1: pull request create failed: GraphQL: a pull request for branch "owner:feature" into branch "main" already exists`)
		}),
		WithSleepFunc(func(time.Duration) {}),
	)

	_, err := p.CreateDraftPR(context.Background(), testPROpts())
	if !errors.Is(err, vcs.ErrPRAlreadyExists) {
		t.Fatalf("error = %v, want ErrPRAlreadyExists", err)
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

func TestCreateDraftPR_EmptyRepoPath(t *testing.T) {
	p := New(zerolog.Nop(),
		WithRunGH(func(_ context.Context, args ...string) (string, error) {
			t.Fatal("gh should not be called with empty repo path")
			return "", nil
		}),
	)

	opts := testPROpts()
	opts.RepoPath = ""
	_, err := p.CreateDraftPR(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error for empty repo path")
	}
	if !strings.Contains(err.Error(), "repo path is empty") {
		t.Errorf("unexpected error: %v", err)
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
	// Verify the last error is preserved in the message.
	if !strings.Contains(err.Error(), "Head sha can't be blank") {
		t.Errorf("error should contain last error detail, got: %v", err)
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

func TestListOpenPRs_RetriesSecondaryRateLimit(t *testing.T) {
	var calls atomic.Int32
	var slept atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "", fmt.Errorf("gh pr list: HTTP 403: You have exceeded a secondary rate limit")
		}
		return `[]`, nil
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {
			slept.Add(1)
		}),
	)

	if _, err := p.ListOpenPRs(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d calls, want 2", got)
	}
	if got := slept.Load(); got != 1 {
		t.Errorf("got %d sleeps, want 1", got)
	}
}

func TestListPRsByState_PassesStateAndLimitAndMaps(t *testing.T) {
	const prJSON = `[{"number":7,"title":"Fix bug","headRefName":"feature","state":"OPEN","author":{"login":"alice"}}]`

	tests := []struct {
		name      string
		call      func(*Provider) ([]vcs.PRSummary, error)
		wantState string
		wantLimit string
	}{
		{
			name:      "open",
			call:      func(p *Provider) ([]vcs.PRSummary, error) { return p.ListOpenPRs(context.Background(), "owner/repo") },
			wantState: "open",
			wantLimit: "300",
		},
		{
			name:      "closed",
			call:      func(p *Provider) ([]vcs.PRSummary, error) { return p.ListClosedPRs(context.Background(), "owner/repo") },
			wantState: "closed",
			wantLimit: "50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			fakeGH := func(_ context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return prJSON, nil
			}

			p := New(zerolog.Nop(), WithRunGH(fakeGH))
			prs, err := tt.call(p)
			if err != nil {
				t.Fatalf("list PRs: %v", err)
			}

			joined := strings.Join(gotArgs, " ")
			if !strings.Contains(joined, "--state "+tt.wantState) {
				t.Errorf("args %q missing --state %s", joined, tt.wantState)
			}
			if !strings.Contains(joined, "--limit "+tt.wantLimit) {
				t.Errorf("args %q missing --limit %s", joined, tt.wantLimit)
			}

			if len(prs) != 1 {
				t.Fatalf("got %d PRs, want 1", len(prs))
			}
			got := prs[0]
			want := vcs.PRSummary{
				Number:     7,
				Title:      "Fix bug",
				HeadBranch: "feature",
				State:      parsePRState("OPEN"),
				Author:     "alice",
			}
			if got != want {
				t.Errorf("PRSummary = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSearchPRsByTitleTag_QueriesAllStatesAndAppliesExactBracketFilter(t *testing.T) {
	// The API returns matches for the bareword search; two carry the exact
	// "[BOS-289]" tag (open + merged), one is a "[BOS-2890]" near-miss that the
	// Go-side exact bracket filter must drop.
	const prJSON = `[
		{"number":1147,"title":"consolidate [BOS-289]","headRefName":"bos-289-a","state":"OPEN","author":{"login":"alice"}},
		{"number":1146,"title":"consolidate [BOS-289]","headRefName":"bos-289-b","state":"MERGED","author":{"login":"bob"}},
		{"number":42,"title":"unrelated [BOS-2890]","headRefName":"bos-2890","state":"MERGED","author":{"login":"carol"}}
	]`

	var gotArgs []string
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		gotArgs = append([]string(nil), args...)
		return prJSON, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	prs, err := p.SearchPRsByTitleTag(context.Background(), "owner/repo", "BOS-289")
	if err != nil {
		t.Fatalf("SearchPRsByTitleTag: %v", err)
	}

	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--state all") {
		t.Errorf("args %q missing --state all", joined)
	}
	if !strings.Contains(joined, "--search BOS-289 in:title") {
		t.Errorf("args %q missing --search BOS-289 in:title", joined)
	}
	if !strings.Contains(joined, "--limit 100") {
		t.Errorf("args %q missing --limit 100", joined)
	}

	if len(prs) != 2 {
		t.Fatalf("got %d PRs, want 2 (exact [BOS-289] matches only): %+v", len(prs), prs)
	}
	want := []vcs.PRSummary{
		{Number: 1147, Title: "consolidate [BOS-289]", HeadBranch: "bos-289-a", State: vcs.PRStateOpen, Author: "alice"},
		{Number: 1146, Title: "consolidate [BOS-289]", HeadBranch: "bos-289-b", State: vcs.PRStateMerged, Author: "bob"},
	}
	for i, w := range want {
		if prs[i] != w {
			t.Errorf("prs[%d] = %+v, want %+v", i, prs[i], w)
		}
	}
}

func TestGetPRStatus_ReviewRequiredDecisionDoesNotLookRejected(t *testing.T) {
	var viewArgs []string
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			viewArgs = append([]string(nil), args...)
			return `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","isDraft":false,"title":"Review gated","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","reviewDecision":"REVIEW_REQUIRED","reviews":[]}`, nil
		}
		if len(args) >= 2 && args[0] == "api" {
			return `{"rebaseable":false}`, nil
		}
		return "", fmt.Errorf("unexpected gh args: %v", args)
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if status.LatestReviewState != vcs.ReviewStateRequired {
		t.Fatalf("LatestReviewState = %v, want ReviewStateRequired", status.LatestReviewState)
	}
	if status.ReviewDecisionState != vcs.ReviewStateRequired {
		t.Fatalf("ReviewDecisionState = %v, want ReviewStateRequired", status.ReviewDecisionState)
	}
	if status.MergeStateStatus != vcs.MergeStateStatusBlocked {
		t.Fatalf("MergeStateStatus = %v, want MergeStateStatusBlocked", status.MergeStateStatus)
	}
	if status.Rebaseable == nil || *status.Rebaseable {
		t.Fatalf("Rebaseable = %v, want false", status.Rebaseable)
	}
	if !strings.Contains(strings.Join(viewArgs, " "), "reviewDecision") {
		t.Fatalf("gh pr view args = %v, want reviewDecision field", viewArgs)
	}
	if !strings.Contains(strings.Join(viewArgs, " "), "mergeStateStatus") {
		t.Fatalf("gh pr view args = %v, want mergeStateStatus field", viewArgs)
	}
}

// TestGetPRStatus_ReviewStateResolution pins how GetPRStatus reconciles the
// per-review states in the "reviews" array against the overall reviewDecision.
// The existing coverage only exercises an empty reviews array; this locks in the
// two behaviors that drive repair/merge gating: an individual review state wins
// when the decision is non-definitive, but a definitive APPROVED/CHANGES_REQUESTED
// decision always overrides a later individual review so a stale review cannot
// mask the authoritative decision.
func TestGetPRStatus_ReviewStateResolution(t *testing.T) {
	tests := []struct {
		name              string
		reviewDecision    string
		reviewStates      []string
		wantDecisionState vcs.ReviewState
		wantLatestState   vcs.ReviewState
	}{
		{
			name:              "non-definitive decision lets latest individual review win",
			reviewDecision:    "REVIEW_REQUIRED",
			reviewStates:      []string{"COMMENTED", "APPROVED"},
			wantDecisionState: vcs.ReviewStateRequired,
			wantLatestState:   vcs.ReviewStateApproved,
		},
		{
			name:              "pending review is ignored when picking latest",
			reviewDecision:    "REVIEW_REQUIRED",
			reviewStates:      []string{"APPROVED", "PENDING"},
			wantDecisionState: vcs.ReviewStateRequired,
			wantLatestState:   vcs.ReviewStateApproved,
		},
		{
			name:              "changes-requested decision overrides a later approval",
			reviewDecision:    "CHANGES_REQUESTED",
			reviewStates:      []string{"APPROVED"},
			wantDecisionState: vcs.ReviewStateChangesRequested,
			wantLatestState:   vcs.ReviewStateChangesRequested,
		},
		{
			name:              "approved decision overrides a later changes-requested review",
			reviewDecision:    "APPROVED",
			reviewStates:      []string{"CHANGES_REQUESTED"},
			wantDecisionState: vcs.ReviewStateApproved,
			wantLatestState:   vcs.ReviewStateApproved,
		},
		{
			name:              "no decision falls back to the latest individual review",
			reviewDecision:    "",
			reviewStates:      []string{"APPROVED", "COMMENTED"},
			wantDecisionState: vcs.ReviewStateUnspecified,
			wantLatestState:   vcs.ReviewStateCommented,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewsJSON := make([]string, 0, len(tt.reviewStates))
			for _, s := range tt.reviewStates {
				reviewsJSON = append(reviewsJSON, fmt.Sprintf(`{"state":%q}`, s))
			}
			fakeGH := func(_ context.Context, args ...string) (string, error) {
				if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
					return fmt.Sprintf(
						`{"state":"OPEN","mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","isDraft":false,`+
							`"title":"Review resolution","headRefName":"feature","baseRefName":"main",`+
							`"headRefOid":"abc123","reviewDecision":%q,"reviews":[%s]}`,
						tt.reviewDecision, strings.Join(reviewsJSON, ","),
					), nil
				}
				// A CONFLICTING PR is not mergeable, so GetPRStatus must not make
				// the secondary mergeability API call; surface it as a failure.
				return "", fmt.Errorf("unexpected gh args: %v", args)
			}

			p := New(zerolog.Nop(), WithRunGH(fakeGH))
			status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
			if err != nil {
				t.Fatalf("GetPRStatus: %v", err)
			}
			if status.ReviewDecisionState != tt.wantDecisionState {
				t.Errorf("ReviewDecisionState = %v, want %v", status.ReviewDecisionState, tt.wantDecisionState)
			}
			if status.LatestReviewState != tt.wantLatestState {
				t.Errorf("LatestReviewState = %v, want %v", status.LatestReviewState, tt.wantLatestState)
			}
		})
	}
}

func TestGetPRStatus_RestMergeabilityMapsRebaseConflict(t *testing.T) {
	apiCalled := false
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			return `{"state":"OPEN","mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN","isDraft":false,"title":"Rebase blocked","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","reviewDecision":"","reviews":[]}`, nil
		}
		if len(args) >= 2 && args[0] == "api" && args[1] == "repos/owner/repo/pulls/42" {
			apiCalled = true
			return `{"mergeable":true,"mergeable_state":"clean","rebaseable":false}`, nil
		}
		return "", fmt.Errorf("unexpected gh args: %v", args)
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if !apiCalled {
		t.Fatal("expected REST pull request payload to be fetched")
	}
	if status.Mergeable == nil || !*status.Mergeable {
		t.Fatalf("Mergeable = %v, want true", status.Mergeable)
	}
	if status.Rebaseable == nil || *status.Rebaseable {
		t.Fatalf("Rebaseable = %v, want false", status.Rebaseable)
	}
	if status.MergeStateStatus != vcs.MergeStateStatusClean {
		t.Fatalf("MergeStateStatus = %v, want MergeStateStatusClean", status.MergeStateStatus)
	}
}

func TestGetPRStatus_ReviewRequiredDoesNotSuppressCommentedReview(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			return `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","isDraft":false,"title":"Review gated","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","reviewDecision":"REVIEW_REQUIRED","reviews":[{"state":"COMMENTED"}]}`, nil
		}
		if len(args) >= 2 && args[0] == "api" {
			return `{"rebaseable":false}`, nil
		}
		return "", fmt.Errorf("unexpected gh args: %v", args)
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if status.ReviewDecisionState != vcs.ReviewStateRequired {
		t.Fatalf("ReviewDecisionState = %v, want ReviewStateRequired", status.ReviewDecisionState)
	}
	if status.LatestReviewState != vcs.ReviewStateCommented {
		t.Fatalf("LatestReviewState = %v, want ReviewStateCommented", status.LatestReviewState)
	}
}

func TestGetPRStatus_PendingReviewDoesNotSuppressChangesRequestedDecision(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			return `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","isDraft":false,"title":"Review gated","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","reviewDecision":"CHANGES_REQUESTED","reviews":[{"state":"CHANGES_REQUESTED"},{"state":"PENDING"}]}`, nil
		}
		if len(args) >= 2 && args[0] == "api" {
			return `{"rebaseable":false}`, nil
		}
		return "", fmt.Errorf("unexpected gh args: %v", args)
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if status.ReviewDecisionState != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewDecisionState = %v, want ReviewStateChangesRequested", status.ReviewDecisionState)
	}
	if status.LatestReviewState != vcs.ReviewStateChangesRequested {
		t.Fatalf("LatestReviewState = %v, want ReviewStateChangesRequested", status.LatestReviewState)
	}
}

func TestGetPRStatus_CommentedReviewDoesNotSuppressChangesRequestedDecision(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			return `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","isDraft":false,"title":"Review gated","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","reviewDecision":"CHANGES_REQUESTED","reviews":[{"state":"CHANGES_REQUESTED"},{"state":"COMMENTED"},{"state":"DISMISSED"}]}`, nil
		}
		if len(args) >= 2 && args[0] == "api" {
			return `{"rebaseable":false}`, nil
		}
		return "", fmt.Errorf("unexpected gh args: %v", args)
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if status.ReviewDecisionState != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewDecisionState = %v, want ReviewStateChangesRequested", status.ReviewDecisionState)
	}
	if status.LatestReviewState != vcs.ReviewStateChangesRequested {
		t.Fatalf("LatestReviewState = %v, want ReviewStateChangesRequested", status.LatestReviewState)
	}
}

func TestGetPRStatus_CommentedReviewDoesNotSuppressApprovedDecision(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			return `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","isDraft":false,"title":"Review gated","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","reviewDecision":"APPROVED","reviews":[{"state":"APPROVED"},{"state":"COMMENTED"},{"state":"DISMISSED"}]}`, nil
		}
		if len(args) >= 2 && args[0] == "api" {
			return `{"rebaseable":true}`, nil
		}
		return "", fmt.Errorf("unexpected gh args: %v", args)
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	status, err := p.GetPRStatus(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if status.ReviewDecisionState != vcs.ReviewStateApproved {
		t.Fatalf("ReviewDecisionState = %v, want ReviewStateApproved", status.ReviewDecisionState)
	}
	if status.LatestReviewState != vcs.ReviewStateApproved {
		t.Fatalf("LatestReviewState = %v, want ReviewStateApproved", status.LatestReviewState)
	}
}

func TestListOpenPRs_RetriesPrimaryRateLimit(t *testing.T) {
	var calls atomic.Int32
	var slept atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "", fmt.Errorf("gh pr list: HTTP 429: API rate limit exceeded for user ID")
		}
		return `[]`, nil
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {
			slept.Add(1)
		}),
	)

	if _, err := p.ListOpenPRs(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d calls, want 2", got)
	}
	if got := slept.Load(); got != 1 {
		t.Errorf("got %d sleeps, want 1", got)
	}
}

func TestMergePR_RetriesBadGateway(t *testing.T) {
	var calls atomic.Int32
	fakeGH := func(_ context.Context, args ...string) (string, error) {
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

	if err := p.MergePR(context.Background(), "owner/repo", 42, "rebase"); err != nil {
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

	err := p.MergePR(context.Background(), "owner/repo", 42, "rebase")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d calls, want 1", got)
	}
}

func TestMergePR_ClassifiesWorkflowScopeError(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		return "", fmt.Errorf("gh pr merge 4 --repo freshclaim/marketing --rebase --delete-branch: exit status 1: GraphQL: refusing to allow an OAuth App to create or update workflow `.github/workflows/_quality-gate.yml` without `workflow` scope (mergePullRequest)")
	}

	p := New(zerolog.Nop(),
		WithRunGH(fakeGH),
		WithSleepFunc(func(time.Duration) {}),
	)

	err := p.MergePR(context.Background(), "freshclaim/marketing", 4, "rebase")
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

// graphqlThreadsResponse builds a fake GraphQL response with the given threads.
// Each thread is (isResolved bool, authorLogin string).
func graphqlThreadsResponse(threads ...struct {
	resolved bool
	author   string
}) string {
	return graphqlThreadsPageResponse(false, "", threads...)
}

func graphqlThreadsPageResponse(hasNextPage bool, endCursor string, threads ...struct {
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
		`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}}}}}`,
		hasNextPage, endCursor, strings.Join(nodes, ","),
	)
}

func TestGetReviewComments_ChangesRequestedIncludesInlineComments(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && strings.Contains(args[1], "/reviews/123/comments") {
			return `[
				{"user":{"login":"reviewer"},"body":"fix this line","path":"main.go","line":42}
			]`, nil
		}
		return `[
			{"id":123,"user":{"login":"reviewer"},"body":"summary","state":"CHANGES_REQUESTED"},
			{"id":456,"user":{"login":"reviewer"},"body":"follow-up approval","state":"APPROVED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("got %d comments, want summaries plus inline comment", len(comments))
	}
	if comments[1].Body != "fix this line" {
		t.Fatalf("inline comment body = %q, want fix this line", comments[1].Body)
	}
	if comments[1].Path == nil || *comments[1].Path != "main.go" {
		t.Fatalf("inline comment path = %v, want main.go", comments[1].Path)
	}
	if comments[1].Line == nil || *comments[1].Line != 42 {
		t.Fatalf("inline comment line = %v, want 42", comments[1].Line)
	}
	if comments[1].State != vcs.ReviewStateChangesRequested {
		t.Fatalf("inline comment state = %v, want ChangesRequested", comments[1].State)
	}
	if comments[2].State != vcs.ReviewStateApproved {
		t.Fatalf("final review state = %v, want Approved", comments[2].State)
	}
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
				}{false, "chatgpt-codex-connector"},
				struct {
					resolved bool
					author   string
				}{true, "chatgpt-codex-connector"},
			), nil
		}
		if args[0] == "api" && strings.Contains(args[1], "/reviews/123/comments") {
			return `[
				{"user":{"login":"chatgpt-codex-connector[bot]"},"body":"fix this line","path":"main.go","line":42}
			]`, nil
		}
		return `[
			{"id":123,"user":{"login":"chatgpt-codex-connector[bot]"},"body":"found issues","state":"COMMENTED"},
			{"user":{"login":"human-reviewer"},"body":"looks good","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("got %d comments, want promoted summary, inline comment, and human comment", len(comments))
	}
	if comments[0].State != vcs.ReviewStateChangesRequested {
		t.Errorf("bot comment state = %v, want ChangesRequested", comments[0].State)
	}
	if comments[1].Body != "fix this line" {
		t.Fatalf("inline comment body = %q, want fix this line", comments[1].Body)
	}
	if comments[1].Path == nil || *comments[1].Path != "main.go" {
		t.Fatalf("inline comment path = %v, want main.go", comments[1].Path)
	}
	if comments[1].Line == nil || *comments[1].Line != 42 {
		t.Fatalf("inline comment line = %v, want 42", comments[1].Line)
	}
	if comments[1].State != vcs.ReviewStateChangesRequested {
		t.Errorf("inline comment state = %v, want ChangesRequested", comments[1].State)
	}
	if comments[2].State != vcs.ReviewStateCommented {
		t.Errorf("human comment state = %v, want Commented", comments[2].State)
	}
}

func TestGetReviewComments_CodexBotCommentedReviewWithInlineCommentsPromotesToChangesRequested(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			return graphqlThreadsResponse(
				struct {
					resolved bool
					author   string
				}{false, "chatgpt-codex-connector"},
			), nil
		}
		if args[0] == "api" && strings.Contains(args[1], "/reviews/987/comments") {
			return `[
				{"user":{"login":"chatgpt-codex-connector[bot]"},"body":"handle the nil case","path":"session.go","line":87}
			]`, nil
		}
		return `[
			{"id":987,"user":{"login":"chatgpt-codex-connector[bot]"},"body":"reviewed","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want promoted summary plus inline comment", len(comments))
	}
	for i, comment := range comments {
		if comment.State != vcs.ReviewStateChangesRequested {
			t.Fatalf("comment[%d].State = %v, want ChangesRequested", i, comment.State)
		}
	}
	if comments[1].Path == nil || *comments[1].Path != "session.go" {
		t.Fatalf("inline comment path = %v, want session.go", comments[1].Path)
	}
	if comments[1].Line == nil || *comments[1].Line != 87 {
		t.Fatalf("inline comment line = %v, want 87", comments[1].Line)
	}
}

func TestGetReviewComments_BotWithUnresolvedThreadOnSecondPage(t *testing.T) {
	graphQLCalls := 0
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			graphQLCalls++
			argsStr := strings.Join(args, " ")
			if !strings.Contains(argsStr, "after=cursor-1") {
				return graphqlThreadsPageResponse(true, "cursor-1",
					struct {
						resolved bool
						author   string
					}{true, "chatgpt-codex-connector"},
				), nil
			}
			return graphqlThreadsPageResponse(false, "",
				struct {
					resolved bool
					author   string
				}{false, "chatgpt-codex-connector"},
			), nil
		}
		if args[0] == "api" && strings.Contains(args[1], "/reviews/123/comments") {
			return `[
				{"user":{"login":"chatgpt-codex-connector[bot]"},"body":"fix second-page issue","path":"main.go","line":42}
			]`, nil
		}
		return `[
			{"id":123,"user":{"login":"chatgpt-codex-connector[bot]"},"body":"found issues","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if graphQLCalls != 2 {
		t.Fatalf("GraphQL calls = %d, want 2", graphQLCalls)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want promoted summary and inline comment", len(comments))
	}
	if comments[0].State != vcs.ReviewStateChangesRequested {
		t.Errorf("bot comment state = %v, want ChangesRequested", comments[0].State)
	}
	if comments[1].Body != "fix second-page issue" {
		t.Fatalf("inline comment body = %q, want fix second-page issue", comments[1].Body)
	}
}

// codexBoilerplateBody is the byte-identical summary body that
// chatgpt-codex-connector[bot] posts on every COMMENTED review. It carries no
// PR-specific prose (real feedback arrives as inline comments), so a review
// whose only content is this boilerplate must not be promoted to
// CHANGES_REQUESTED (BOS-254).
const codexBoilerplateBody = "\n### 💡 Codex Review\n\n" +
	"Here are some automated review suggestions for this pull request.\n\n" +
	"**Reviewed commit:** `57beeafbb5`\n    \n\n" +
	"<details> <summary>ℹ️ About Codex in GitHub</summary>\n<br/>\n\n" +
	"[Your team has set up Codex to review pull requests in this repo](https://chatgpt.com/codex/cloud/settings/general). Reviews are triggered when you\n" +
	"- Open a pull request for review\n- Mark a draft as ready\n- Comment \"@codex review\".\n\n" +
	"If Codex has suggestions, it will comment; otherwise it will react with 👍.\n\n\n\n\n" +
	"Codex can also answer questions or update the PR. Try commenting \"@codex address that feedback\".\n            \n</details>\n"

// TestGetReviewComments_EmptyCodexCommentedReviewWithStaleThreadIsDropped is
// the BOS-254 headline: an empty codex COMMENTED review (boilerplate-only body,
// zero inline comments of its own) must NOT be promoted to ChangesRequested
// even when the same bot has a separate unresolved/outdated thread on the PR.
// The old author-scoped heuristic promoted it off that stale thread, wedging the
// merge gate in a loop boss-repair could never clear. The empty review is
// dropped (not surfaced) rather than appended as COMMENTED, so it cannot
// overwrite an earlier promoted CHANGES_REQUESTED from the same bot in
// ComputeDisplayStatus's latest-by-author map.
func TestGetReviewComments_EmptyCodexCommentedReviewWithStaleThreadIsDropped(t *testing.T) {
	reviewsJSON := fmt.Sprintf(
		`[{"id":111,"user":{"login":"chatgpt-codex-connector[bot]"},"body":%q,"state":"COMMENTED"}]`,
		codexBoilerplateBody,
	)
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			// The bot has a separate, unresolved (outdated) thread on the PR —
			// but NOT one belonging to this empty review.
			return graphqlThreadsResponse(
				struct {
					resolved bool
					author   string
				}{false, "chatgpt-codex-connector"},
			), nil
		}
		if args[0] == "api" && strings.Contains(args[1], "/reviews/111/comments") {
			// This review carries no inline comments of its own.
			return `[]`, nil
		}
		return reviewsJSON, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("got %d comments, want the empty boilerplate codex review dropped (not promoted, not surfaced)", len(comments))
	}
}

// TestGetReviewComments_EmptyCodexFollowupDoesNotClobberEarlierPromotion pins
// BOS-254 finding #2: when a bot posts a real review with unresolved inline
// suggestions and then a later empty boilerplate COMMENTED review, the empty
// follow-up must not overwrite the earlier promoted CHANGES_REQUESTED in the
// surfaced set. The earlier review's ChangesRequested entries must survive so
// ComputeDisplayStatus keeps the bot's latest-by-author state blocking.
func TestGetReviewComments_EmptyCodexFollowupDoesNotClobberEarlierPromotion(t *testing.T) {
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "api" && args[1] == "graphql" {
			return graphqlThreadsResponse(
				struct {
					resolved bool
					author   string
				}{false, "chatgpt-codex-connector"},
			), nil
		}
		// Review 111 (earlier) carries a real inline suggestion; review 222
		// (later, empty follow-up) has none.
		if args[0] == "api" && strings.Contains(args[1], "/reviews/111/comments") {
			return `[
				{"user":{"login":"chatgpt-codex-connector[bot]"},"body":"handle the nil case","path":"session.go","line":87}
			]`, nil
		}
		if args[0] == "api" && strings.Contains(args[1], "/reviews/222/comments") {
			return `[]`, nil
		}
		return fmt.Sprintf(
			`[{"id":111,"user":{"login":"chatgpt-codex-connector[bot]"},"body":"reviewed","state":"COMMENTED"},`+
				`{"id":222,"user":{"login":"chatgpt-codex-connector[bot]"},"body":%q,"state":"COMMENTED"}]`,
			codexBoilerplateBody,
		), nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect [promoted summary(CR), inline(CR)]; the empty follow-up is dropped.
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want the promoted review + its inline comment (empty follow-up dropped)", len(comments))
	}
	for i, c := range comments {
		if c.State != vcs.ReviewStateChangesRequested {
			t.Fatalf("comment[%d].State = %v, want ChangesRequested (earlier promotion must survive)", i, c.State)
		}
	}
	// Mirror the display-layer consequence: latest-by-author stays blocking.
	latestByAuthor := map[string]vcs.ReviewState{}
	for _, c := range comments {
		latestByAuthor[c.Author] = c.State
	}
	if latestByAuthor["chatgpt-codex-connector[bot]"] != vcs.ReviewStateChangesRequested {
		t.Fatalf("latest-by-author codex state = %v, want ChangesRequested (empty follow-up must not clobber)", latestByAuthor["chatgpt-codex-connector[bot]"])
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
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want only the human review", len(comments))
	}
	if comments[0].State != vcs.ReviewStateApproved {
		t.Errorf("human comment state = %v, want Approved", comments[0].State)
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
	if len(comments) != 0 {
		t.Fatalf("got %d comments, want none for resolved/no-thread bot review", len(comments))
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
	// GraphQL failed — fail-closed means no promotion.
	if comments[0].State != vcs.ReviewStateCommented {
		t.Errorf("bot comment state = %v, want Commented (fail-closed)", comments[0].State)
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
			// cursor has an unresolved thread, cubic-dev-ai all resolved.
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
		// cursor's review carries its own inline suggestion; cubic-dev-ai is
		// dropped (all resolved) before its inline comments are ever fetched.
		if args[0] == "api" && strings.Contains(args[1], "/reviews/501/comments") {
			return `[
				{"user":{"login":"cursor[bot]"},"body":"fix the leak","path":"main.go","line":10}
			]`, nil
		}
		return `[
			{"id":501,"user":{"login":"cursor[bot]"},"body":"issue found","state":"COMMENTED"},
			{"id":502,"user":{"login":"cubic-dev-ai[bot]"},"body":"issue found","state":"COMMENTED"}
		]`, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))
	comments, err := p.GetReviewComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// cursor[bot] carries its own inline suggestion under an unresolved thread →
	// promoted (summary + inline). cubic-dev-ai[bot]'s threads are all resolved →
	// dropped.
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want cursor's promoted summary + inline (cubic dropped)", len(comments))
	}
	if comments[0].State != vcs.ReviewStateChangesRequested {
		t.Errorf("cursor[bot] summary state = %v, want ChangesRequested", comments[0].State)
	}
	if comments[1].Body != "fix the leak" {
		t.Errorf("inline comment body = %q, want fix the leak", comments[1].Body)
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

// flagValue returns the argument immediately following the first occurrence of
// flag, or "" when the flag is absent or has no value. Used to assert the
// --state gh requests without coupling to the full argv ordering.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A populated gh payload exercising the field mapping both list calls perform:
// headRefName -> HeadBranch, author.login -> Author, and parsePRState on state.
const listPRsPayload = `[
	{"number":42,"title":"Add widget","headRefName":"feature/widget","state":"OPEN","author":{"login":"alice"}},
	{"number":7,"title":"Fix typo","headRefName":"bugfix/typo","state":"CLOSED","author":{"login":"bob"}}
]`

func TestListOpenPRs_ParsesAndMapsPayload(t *testing.T) {
	var gotArgs []string
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		gotArgs = append([]string(nil), args...)
		return listPRsPayload, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	prs, err := p.ListOpenPRs(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}

	if got := flagValue(gotArgs, "--state"); got != "open" {
		t.Errorf("ListOpenPRs requested --state %q, want %q", got, "open")
	}

	want := []vcs.PRSummary{
		{Number: 42, Title: "Add widget", HeadBranch: "feature/widget", State: vcs.PRStateOpen, Author: "alice"},
		{Number: 7, Title: "Fix typo", HeadBranch: "bugfix/typo", State: vcs.PRStateClosed, Author: "bob"},
	}
	assertPRSummaries(t, prs, want)
}

func TestListClosedPRs_ParsesAndMapsPayload(t *testing.T) {
	var gotArgs []string
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		gotArgs = append([]string(nil), args...)
		return listPRsPayload, nil
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	prs, err := p.ListClosedPRs(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("ListClosedPRs: %v", err)
	}

	if got := flagValue(gotArgs, "--state"); got != "closed" {
		t.Errorf("ListClosedPRs requested --state %q, want %q", got, "closed")
	}

	want := []vcs.PRSummary{
		{Number: 42, Title: "Add widget", HeadBranch: "feature/widget", State: vcs.PRStateOpen, Author: "alice"},
		{Number: 7, Title: "Fix typo", HeadBranch: "bugfix/typo", State: vcs.PRStateClosed, Author: "bob"},
	}
	assertPRSummaries(t, prs, want)
}

func TestListOpenPRs_EmptyPayloadYieldsNoPRs(t *testing.T) {
	fakeGH := func(_ context.Context, _ ...string) (string, error) { return `[]`, nil }

	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	prs, err := p.ListOpenPRs(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 0 {
		t.Errorf("got %d PRs, want 0", len(prs))
	}
}

func TestGetCheckResults_NoChecksReportedIsEmptyNotError(t *testing.T) {
	// `gh pr checks` exits non-zero when the head commit has no check runs.
	// That is a normal empty state — GetCheckResults must surface it as an
	// empty slice with no error, so the display poller still recomputes and
	// clears any stale status (e.g. a "draft" left over from before the PR
	// became ready). Regression test for the stuck-draft bug (WON-1118).
	fakeGH := func(_ context.Context, _ ...string) (string, error) {
		return "", fmt.Errorf("gh pr checks 299 --repo IKHOR/wondercanvas-mono --json name,state,workflow: exit status 1: no checks reported on the 'dave/won-1118-fable-02' branch")
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	results, err := p.GetCheckResults(context.Background(), "IKHOR/wondercanvas-mono", 299)
	if err != nil {
		t.Fatalf("GetCheckResults: unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d check results, want 0", len(results))
	}
}

func TestGetCheckResults_RealErrorPropagates(t *testing.T) {
	// A genuine gh failure (e.g. rate limit) must still surface as an error so
	// the poller preserves the previous status instead of collapsing it.
	fakeGH := func(_ context.Context, _ ...string) (string, error) {
		return "", fmt.Errorf("gh pr checks 299 --repo IKHOR/wondercanvas-mono --json name,state,workflow: exit status 1: GraphQL: API rate limit already exceeded for user ID 96322")
	}

	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	if _, err := p.GetCheckResults(context.Background(), "IKHOR/wondercanvas-mono", 299); err == nil {
		t.Fatal("expected error for a real gh failure, got nil")
	}
}

func assertPRSummaries(t *testing.T, got, want []vcs.PRSummary) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d PRs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PR[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
