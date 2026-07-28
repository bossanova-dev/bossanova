package mergepolicy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossalib/vcs"
)

// stubProvider implements the subset of vcs.Provider these tests exercise.
// The rest panics so accidental extra calls surface loudly.
type stubProvider struct {
	allowed       []string
	allowedErr    error
	mergeCommit   string
	mergeCommitFn func(prID int) (string, error)

	// gotRepoPath records the last repoPath GetAllowedMergeStrategies was
	// called with. SquashFallback takes adjacent (originURL, strategy)
	// strings, so a transposition compiles and then silently queries the
	// wrong repo — which fails open and makes the whole fallback dead code
	// with no error and no log. Tests pin the forwarding mechanically.
	gotRepoPath string
}

func (s *stubProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	panic("unused")
}
func (s *stubProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	panic("unused")
}
func (s *stubProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	panic("unused")
}
func (s *stubProvider) GetFailedCheckLogs(context.Context, string, string) (string, error) {
	panic("unused")
}
func (s *stubProvider) MarkReadyForReview(context.Context, string, int) error { panic("unused") }
func (s *stubProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	panic("unused")
}
func (s *stubProvider) ListOpenPRs(context.Context, string) ([]vcs.PRSummary, error) {
	panic("unused")
}
func (s *stubProvider) ListClosedPRs(context.Context, string) ([]vcs.PRSummary, error) {
	panic("unused")
}

func (s *stubProvider) SearchPRsByTitleTag(context.Context, string, string) ([]vcs.PRSummary, error) {
	panic("unused")
}
func (s *stubProvider) MergePR(context.Context, string, int, string) error { panic("unused") }
func (s *stubProvider) UpdatePRTitle(context.Context, string, int, string) error {
	panic("unused")
}
func (s *stubProvider) GetPRMergeCommit(_ context.Context, _ string, prID int) (string, error) {
	if s.mergeCommitFn != nil {
		return s.mergeCommitFn(prID)
	}
	return s.mergeCommit, nil
}
func (s *stubProvider) GetAllowedMergeStrategies(_ context.Context, repoPath string) ([]string, error) {
	s.gotRepoPath = repoPath
	return s.allowed, s.allowedErr
}

type stubVerifier struct {
	ancestorFn func(ref, target string) (bool, error)
	fetchErr   error
}

func (v *stubVerifier) IsAncestor(_ context.Context, _, ref, target string) (bool, error) {
	return v.ancestorFn(ref, target)
}
func (v *stubVerifier) FetchBase(_ context.Context, _, _ string) error { return v.fetchErr }

// stubCounter implements MergeCommitCounter for ResolveStrategyForBranch
// tests. failOnCall panics the test if CountMergeCommits is invoked, so
// tests can assert the fail-open-before-counting paths never reach it.
type stubCounter struct {
	count      int
	err        error
	failOnCall *testing.T

	// Recorded arguments from the last call. CountMergeCommits takes three
	// same-typed positional strings, so a transposition (base/head swapped,
	// or localPath threaded in the wrong slot) compiles and silently counts
	// the wrong range. These fields let a test pin the ordering mechanically.
	gotLocalPath string
	gotBase      string
	gotHead      string
}

func (c *stubCounter) CountMergeCommits(_ context.Context, localPath, base, head string) (int, error) {
	if c.failOnCall != nil {
		c.failOnCall.Fatal("CountMergeCommits must not be called")
	}
	c.gotLocalPath, c.gotBase, c.gotHead = localPath, base, head
	return c.count, c.err
}

func TestResolveStrategy(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		allowed    []string
		allowedErr error
		want       string
		wantErr    error
	}{
		{"configured is allowed", "squash", []string{"merge", "squash", "rebase"}, nil, "squash", nil},
		{"configured disabled falls back to merge", "rebase", []string{"merge", "squash"}, nil, "merge", nil},
		{"empty configured prefers merge", "", []string{"squash", "rebase", "merge"}, nil, "merge", nil},
		{"only squash allowed", "rebase", []string{"squash"}, nil, "squash", nil},
		{"configured rebase allowed with only rebase remains", "rebase", []string{"rebase"}, nil, "rebase", nil},
		{"none allowed errors out", "merge", []string{}, nil, "", ErrMergeStrategyDisallowed},
		{"query failure keeps configured", "rebase", nil, errors.New("gh boom"), "rebase", nil},
		{"query failure with empty configured defaults to merge", "", nil, errors.New("gh boom"), "merge", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubProvider{allowed: tc.allowed, allowedErr: tc.allowedErr}
			got, err := ResolveStrategy(context.Background(), p, "owner/repo", tc.configured)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerifyOnBase(t *testing.T) {
	t.Run("happy path: merge commit on base", func(t *testing.T) {
		p := &stubProvider{mergeCommit: "deadbeef"}
		v := &stubVerifier{ancestorFn: func(ref, target string) (bool, error) {
			if ref != "deadbeef" || target != "refs/remotes/origin/main" {
				t.Errorf("unexpected IsAncestor args: %q %q", ref, target)
			}
			return true, nil
		}}
		if err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 42); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("regression: merge commit NOT on base surfaces ErrMergeNotOnBase", func(t *testing.T) {
		// The madverts-core incident: gh reports MERGED with a specific
		// commit, but origin/main's history no longer contains it.
		p := &stubProvider{mergeCommit: "76b35392"}
		v := &stubVerifier{ancestorFn: func(_, _ string) (bool, error) { return false, nil }}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 2222)
		if !errors.Is(err, ErrMergeNotOnBase) {
			t.Fatalf("want ErrMergeNotOnBase, got %v", err)
		}
		// Message must name the SHA and PR so the user can act.
		if !strings.Contains(err.Error(), "76b35392") || !strings.Contains(err.Error(), "2222") {
			t.Errorf("err = %v; missing SHA or PR number", err)
		}
	})

	t.Run("fetch failure wraps", func(t *testing.T) {
		p := &stubProvider{mergeCommit: "abc"}
		v := &stubVerifier{
			fetchErr: errors.New("network down"),
			ancestorFn: func(_, _ string) (bool, error) {
				t.Fatal("IsAncestor must not run after fetch fails")
				return false, nil
			},
		}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 1)
		if err == nil || !strings.Contains(err.Error(), "network down") {
			t.Fatalf("want wrapped network error, got %v", err)
		}
	})

	t.Run("GetPRMergeCommit failure short-circuits", func(t *testing.T) {
		p := &stubProvider{mergeCommitFn: func(int) (string, error) { return "", vcs.ErrPRNotMerged }}
		v := &stubVerifier{
			ancestorFn: func(_, _ string) (bool, error) { t.Fatal("should not reach"); return false, nil },
		}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 1)
		if !errors.Is(err, vcs.ErrPRNotMerged) {
			t.Fatalf("want ErrPRNotMerged, got %v", err)
		}
	})

	t.Run("GetPRMergeCommit failure is an infra failure", func(t *testing.T) {
		p := &stubProvider{mergeCommitFn: func(int) (string, error) { return "", vcs.ErrPRNotMerged }}
		v := &stubVerifier{
			ancestorFn: func(_, _ string) (bool, error) { t.Fatal("should not reach"); return false, nil },
		}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 1)
		if !errors.Is(err, ErrMergeVerifyInfra) {
			t.Fatalf("want ErrMergeVerifyInfra, got %v", err)
		}
		if !errors.Is(err, vcs.ErrPRNotMerged) {
			t.Fatalf("want wrapped vcs.ErrPRNotMerged, got %v", err)
		}
	})

	t.Run("fetch failure is an infra failure", func(t *testing.T) {
		wantErr := errors.New("network down")
		p := &stubProvider{mergeCommit: "abc"}
		v := &stubVerifier{
			fetchErr: wantErr,
			ancestorFn: func(_, _ string) (bool, error) {
				t.Fatal("IsAncestor must not run after fetch fails")
				return false, nil
			},
		}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 1)
		if !errors.Is(err, ErrMergeVerifyInfra) {
			t.Fatalf("want ErrMergeVerifyInfra, got %v", err)
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("want wrapped underlying fetch error, got %v", err)
		}
	})

	t.Run("IsAncestor invocation failure is an infra failure", func(t *testing.T) {
		wantErr := errors.New("git exited 128")
		p := &stubProvider{mergeCommit: "abc"}
		v := &stubVerifier{ancestorFn: func(_, _ string) (bool, error) { return false, wantErr }}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 1)
		if !errors.Is(err, ErrMergeVerifyInfra) {
			t.Fatalf("want ErrMergeVerifyInfra, got %v", err)
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("want wrapped underlying ancestor-check error, got %v", err)
		}
	})

	t.Run("ErrMergeNotOnBase does NOT satisfy ErrMergeVerifyInfra", func(t *testing.T) {
		// A completed check with a negative answer is semantic, not
		// infrastructural -- callers must not re-check via the provider API
		// and treat it as success.
		p := &stubProvider{mergeCommit: "76b35392"}
		v := &stubVerifier{ancestorFn: func(_, _ string) (bool, error) { return false, nil }}
		err := VerifyOnBase(context.Background(), p, v, "/repo", "owner/repo", "main", 2222)
		if !errors.Is(err, ErrMergeNotOnBase) {
			t.Fatalf("want ErrMergeNotOnBase, got %v", err)
		}
		if errors.Is(err, ErrMergeVerifyInfra) {
			t.Fatalf("ErrMergeNotOnBase must NOT satisfy errors.Is(err, ErrMergeVerifyInfra), got %v", err)
		}
	})
}

func TestResolveStrategyForBranch(t *testing.T) {
	t.Run("configured rebase, allowed includes rebase, no merge commits", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase"}}
		c := &stubCounter{count: 0}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "rebase" {
			t.Errorf("strategy = %q, want %q", strategy, "rebase")
		}
		if substituted != "" {
			t.Errorf("substituted = %q, want empty", substituted)
		}
	})

	t.Run("configured rebase, allowed includes squash, 2 merge commits substitutes squash", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase", "squash"}}
		c := &stubCounter{count: 2}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "squash" {
			t.Errorf("strategy = %q, want %q", strategy, "squash")
		}
		if substituted == "" || !strings.Contains(substituted, "2") {
			t.Errorf("substituted = %q, want non-empty reason containing the merge-commit count", substituted)
		}
	})

	t.Run("configured rebase, only rebase allowed, merge commits present errors", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase"}}
		c := &stubCounter{count: 1}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "/repo", "main", "feat",
		)
		if !errors.Is(err, ErrMergeStrategyIncompatible) {
			t.Fatalf("want ErrMergeStrategyIncompatible, got %v", err)
		}
		wantMsg := "MERGE_STRATEGY_INCOMPATIBLE: branch has 1 merge commit(s), repo strategy is rebase"
		if err.Error() != wantMsg {
			t.Errorf("err = %q, want %q", err.Error(), wantMsg)
		}
		if strategy != "" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want both empty on error", strategy, substituted)
		}
	})

	t.Run("non-rebase resolved strategy: merge is returned unchanged, counter never called", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"merge", "squash", "rebase"}}
		c := &stubCounter{failOnCall: t}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "merge", c, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "merge" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want %q, \"\"", strategy, substituted, "merge")
		}
	})

	t.Run("non-rebase resolved strategy: squash is returned unchanged, counter never called", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"merge", "squash", "rebase"}}
		c := &stubCounter{failOnCall: t}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "squash", c, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "squash" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want %q, \"\"", strategy, substituted, "squash")
		}
	})

	t.Run("counter error fails open to rebase", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase", "squash"}}
		c := &stubCounter{err: errors.New("git invocation failed")}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "rebase" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want %q, \"\"", strategy, substituted, "rebase")
		}
	})

	t.Run("allowed-strategies error after a positive count fails open to rebase", func(t *testing.T) {
		// The fail-open branch reached ONLY once counting succeeded with
		// merge commits present: ResolveStrategy already failed open to the
		// configured "rebase", the count says substitution is warranted, and
		// then the second GetAllowedMergeStrategies read fails. The merge must
		// still be attempted rather than blocked by an auxiliary check.
		p := &stubProvider{allowedErr: errors.New("github api unreachable")}
		c := &stubCounter{count: 3}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "rebase" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want %q, \"\"", strategy, substituted, "rebase")
		}
		if c.gotHead == "" {
			t.Fatal("counter was never called; this case must exercise the post-count fail-open branch")
		}
	})

	t.Run("counter receives localPath, base, head in that order", func(t *testing.T) {
		// CountMergeCommits' three positional string arguments are mutually
		// transposable without a compile error, and a swap would silently
		// count the wrong revision range. Pin the wiring mechanically.
		p := &stubProvider{allowed: []string{"rebase", "squash"}}
		c := &stubCounter{count: 1}
		if _, _, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c,
			"/local/path", "base-branch", "head-branch",
		); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if c.gotLocalPath != "/local/path" {
			t.Errorf("localPath = %q, want %q", c.gotLocalPath, "/local/path")
		}
		if c.gotBase != "base-branch" {
			t.Errorf("base = %q, want %q", c.gotBase, "base-branch")
		}
		if c.gotHead != "head-branch" {
			t.Errorf("head = %q, want %q", c.gotHead, "head-branch")
		}
		// Same hazard one level up: (originURL, configured) are adjacent
		// same-typed strings threaded into GetAllowedMergeStrategies.
		if p.gotRepoPath != "owner/repo" {
			t.Errorf("GetAllowedMergeStrategies repoPath = %q, want the originURL %q", p.gotRepoPath, "owner/repo")
		}
	})

	t.Run("nil counter fails open to rebase", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase"}}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", nil, "/repo", "main", "feat",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "rebase" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want %q, \"\"", strategy, substituted, "rebase")
		}
	})

	t.Run("empty head and localPath fail open to rebase, counter never called", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase"}}
		c := &stubCounter{failOnCall: t}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "", "main", "",
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strategy != "rebase" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want %q, \"\"", strategy, substituted, "rebase")
		}
	})

	t.Run("ResolveStrategy error propagates ErrMergeStrategyDisallowed", func(t *testing.T) {
		p := &stubProvider{allowed: []string{}}
		c := &stubCounter{failOnCall: t}
		strategy, substituted, err := ResolveStrategyForBranch(
			context.Background(), p, "owner/repo", "rebase", c, "/repo", "main", "feat",
		)
		if !errors.Is(err, ErrMergeStrategyDisallowed) {
			t.Fatalf("want ErrMergeStrategyDisallowed, got %v", err)
		}
		if strategy != "" || substituted != "" {
			t.Errorf("strategy = %q, substituted = %q, want both empty on error", strategy, substituted)
		}
	})
}

func TestIsRebaseRefusal(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		err      error
		want     bool
	}{
		{"nil error", "rebase", nil, false},
		{"non-rebase strategy with matching text", "squash", errors.New("GraphQL: This branch can't be rebased (mergePullRequest)"), false},
		{"rebase with GitHub's exact refusal text", "rebase", errors.New("GraphQL: This branch can't be rebased (mergePullRequest)"), true},
		{"rebase with different-case refusal text", "rebase", errors.New("Cannot be rebased due to conflicts"), true},
		{"rebase with unrelated error", "rebase", errors.New("network timeout"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsRebaseRefusal(tc.strategy, tc.err)
			if got != tc.want {
				t.Errorf("IsRebaseRefusal(%q, %v) = %v, want %v", tc.strategy, tc.err, got, tc.want)
			}
		})
	}
}

func TestSquashFallback(t *testing.T) {
	refusal := errors.New("GraphQL: This branch can't be rebased (mergePullRequest)")

	t.Run("rebase refusal with squash enabled returns squash and a reason", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase", "squash"}}
		fallback, reason, err := SquashFallback(context.Background(), p, "owner/repo", "rebase", refusal)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if fallback != "squash" {
			t.Errorf("fallback = %q, want %q", fallback, "squash")
		}
		if !strings.Contains(reason, "squash") {
			t.Errorf("reason = %q, want a reason mentioning the squash retry", reason)
		}
		// originURL and strategy are adjacent same-typed parameters; a swap
		// would query the repo named "rebase", fail open, and disable the
		// fallback entirely without any error or log.
		if p.gotRepoPath != "owner/repo" {
			t.Errorf("GetAllowedMergeStrategies repoPath = %q, want the originURL %q", p.gotRepoPath, "owner/repo")
		}
	})

	t.Run("rebase refusal with squash disabled is terminally incompatible", func(t *testing.T) {
		p := &stubProvider{allowed: []string{"rebase"}}
		fallback, reason, err := SquashFallback(context.Background(), p, "owner/repo", "rebase", refusal)
		if !errors.Is(err, ErrMergeStrategyIncompatible) {
			t.Fatalf("want ErrMergeStrategyIncompatible, got %v", err)
		}
		// The provider's own refusal must survive in the chain so the caller
		// can report WHY the merge is impossible, not just that it is.
		if !errors.Is(err, refusal) {
			t.Errorf("err = %v, want the original merge error wrapped too", err)
		}
		if fallback != "" || reason != "" {
			t.Errorf("fallback = %q, reason = %q, want both empty on error", fallback, reason)
		}
	})

	t.Run("allowed-strategies error fails open with a diagnostic reason", func(t *testing.T) {
		p := &stubProvider{allowedErr: errors.New("github api unreachable")}
		fallback, reason, err := SquashFallback(context.Background(), p, "owner/repo", "rebase", refusal)
		if err != nil {
			t.Fatalf("fail-open branch must not return an error, got %v", err)
		}
		if fallback != "" {
			t.Errorf("fallback = %q, want empty (no retry when squash cannot be confirmed)", fallback)
		}
		// The degradation must never be silent: the caller logs this reason.
		if !strings.Contains(reason, "github api unreachable") {
			t.Errorf("reason = %q, want it to carry the underlying read failure", reason)
		}
	})

	t.Run("non-refusal merge error is passed through untouched", func(t *testing.T) {
		// The provider must not be consulted at all: this is not a rebase
		// refusal, so there is no fallback decision to make. The empty stub
		// allows nothing, so a consulted provider would surface
		// ErrMergeStrategyIncompatible — a nil error proves the early return.
		fallback, reason, err := SquashFallback(
			context.Background(), &stubProvider{}, "owner/repo", "rebase", errors.New("network timeout"),
		)
		if err != nil || fallback != "" || reason != "" {
			t.Errorf("got (%q, %q, %v), want (\"\", \"\", nil)", fallback, reason, err)
		}
	})

	t.Run("non-rebase strategy is passed through untouched", func(t *testing.T) {
		fallback, reason, err := SquashFallback(
			context.Background(), &stubProvider{}, "owner/repo", "squash", refusal,
		)
		if err != nil || fallback != "" || reason != "" {
			t.Errorf("got (%q, %q, %v), want (\"\", \"\", nil)", fallback, reason, err)
		}
	})
}
