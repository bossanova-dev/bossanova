package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/prstack"
)

type prIdentityListing struct{ prs []vcs.PRSummary }

func (l *prIdentityListing) ListOpenPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return slices.Clone(l.prs), nil
}

type prIdentityGit struct{}

func (prIdentityGit) FetchBaseTip(context.Context, string, string) (string, error) {
	return strings.Repeat("0", 40), nil
}

func (prIdentityGit) PatchIDs(context.Context, string, string, []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

type failingPRIdentityWriter struct{ calls int }

func (w *failingPRIdentityWriter) SetPRAuthorAndTarget(context.Context, string, int, string, string) error {
	w.calls++
	return errors.New("disk full")
}

// The PR-stack change callback stores the open PR's author and base before it
// republishes, so the published session already carries them. A retarget
// replaces the stored target and leaves base_branch alone, and the values
// outlive the PR leaving the listing and a fresh detector (a restart).
func TestPRStackChangePersistsAuthorAndTargetBeforePublishing_Sessions_AC277501(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	repos, sessions := db.NewRepoStore(database), db.NewSessionStore(database)
	repo, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: "widgets", LocalPath: "/tmp/widgets", OriginURL: "https://github.com/acme/widgets.git", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt"})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	pr := 12
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "t", WorktreePath: "/tmp/wt/s", BranchName: "feat", BaseBranch: "main", PRNumber: &pr})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	listing := &prIdentityListing{prs: []vcs.PRSummary{{Number: pr, HeadBranch: "feat", BaseBranch: "main", HeadSHA: "h", Author: "octocat"}}}
	detector := prstack.NewDetector(listing, prIdentityGit{}, zerolog.Nop())
	var published [][2]string
	detector.SetOnChange(onPRStackChange(detector, sessions, func(ctx context.Context, id string) {
		row, err := sessions.Get(ctx, id)
		if err != nil {
			t.Fatalf("publish read: %v", err)
		}
		if row.BaseBranch != "main" {
			t.Fatalf("base_branch = %q; want the creation-time main", row.BaseBranch)
		}
		published = append(published, [2]string{row.PRAuthorLogin, row.PRTargetBranch})
	}, zerolog.Nop()))
	refresh := func(step string, want ...[2]string) {
		t.Helper()
		published = nil
		row, err := sessions.Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		detector.Refresh(ctx, repo, []*models.Session{row})
		if !slices.Equal(published, want) {
			t.Fatalf("%s: published (author, target) %v; want %v", step, published, want)
		}
	}

	refresh("open PR", [2]string{"octocat", "main"})
	listing.prs[0].BaseBranch = "dev"
	refresh("retarget", [2]string{"octocat", "dev"})
	listing.prs = nil
	refresh("PR left the listing", [2]string{"octocat", "dev"})

	restarted := prstack.NewDetector(listing, prIdentityGit{}, zerolog.Nop())
	restarted.SetOnChange(onPRStackChange(restarted, sessions, func(context.Context, string) {}, zerolog.Nop()))
	restarted.Refresh(ctx, repo, []*models.Session{sess})
	row, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.PRAuthorLogin != "octocat" || row.PRTargetBranch != "dev" {
		t.Fatalf("after restart stored (author, target) = (%q, %q); want (octocat, dev)", row.PRAuthorLogin, row.PRTargetBranch)
	}
}

// A failed store write still republishes the session.
func TestPRStackChangePublishesWhenTheWriteFails(t *testing.T) {
	listing := &prIdentityListing{prs: []vcs.PRSummary{{Number: 3, BaseBranch: "main", HeadSHA: "h", Author: "octocat"}}}
	detector := prstack.NewDetector(listing, prIdentityGit{}, zerolog.Nop())
	writer := &failingPRIdentityWriter{}
	var publishes int
	detector.SetOnChange(onPRStackChange(detector, writer, func(context.Context, string) { publishes++ }, zerolog.Nop()))
	pr := 3
	detector.Refresh(t.Context(), &models.Repo{ID: "r", LocalPath: "/repo", OriginURL: "o/r", DefaultBaseBranch: "main"}, []*models.Session{{ID: "s", PRNumber: &pr}})
	if writer.calls != 1 || publishes != 1 {
		t.Fatalf("writes %d, publishes %d; want 1 and 1", writer.calls, publishes)
	}
}
