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
func (s *stubProvider) GetAllowedMergeStrategies(context.Context, string) ([]string, error) {
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
}
