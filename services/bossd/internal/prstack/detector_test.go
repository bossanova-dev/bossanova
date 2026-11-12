package prstack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	gitpkg "github.com/recurser/bossd/internal/adapter/git"
)

type scriptedListing struct {
	prs   []vcs.PRSummary
	err   error
	calls int
}

func (l *scriptedListing) ListOpenPRs(ctx context.Context, _ string) ([]vcs.PRSummary, error) {
	l.calls++
	if !vcs.CachedReadsAllowed(ctx) {
		return nil, errors.New("listing read without cached reads")
	}
	return slices.Clone(l.prs), l.err
}

type countingGit struct {
	tip       string
	tipErr    error
	ids       map[string][]string
	tipCalls  int
	readHeads []string
	// unreadable is a head PatchIDs fails to read.
	unreadable string
}

func (g *countingGit) FetchBaseTip(context.Context, string, string) (string, error) {
	g.tipCalls++
	return g.tip, g.tipErr
}

func (g *countingGit) PatchIDs(_ context.Context, _, _ string, heads []string) (map[string][]string, error) {
	g.readHeads = append(g.readHeads, heads...)
	out := make(map[string][]string)
	var err error
	for _, h := range heads {
		if h == g.unreadable {
			err = errors.New("unreadable head " + h)
		} else if v, ok := g.ids[h]; ok {
			out[h] = v
		}
	}
	return out, err
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, repo, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "add", name)
	run(t, repo, "commit", "-q", "-m", name)
	return run(t, repo, "rev-parse", "HEAD")
}

func session(id string, pr int) *models.Session {
	return &models.Session{ID: id, PRNumber: &pr}
}

// A child carrying a rebased copy of an open parent's commits is stacked on
// it, counting only the parent's commits. A PR into the parent's head branch
// is stacked on it first. Once the parent merges and leaves the open list,
// and the PR into its branch is retargeted to the default branch, neither is
// stacked. A PR into production is never stacked and serves its base beside
// the default branch.
func TestDetectorRealGit(t *testing.T) {
	origin, repo := t.TempDir(), t.TempDir()
	run(t, origin, "init", "-q", "--bare", "-b", "main")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "test@example.test"}, {"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "init"}, {"remote", "add", "origin", origin},
		{"push", "-q", "origin", "main"},
	} {
		run(t, repo, args...)
	}
	run(t, repo, "checkout", "-q", "-b", "parent")
	commit(t, repo, "a.txt")
	parent := commit(t, repo, "b.txt")
	run(t, repo, "checkout", "-q", "main")
	commit(t, repo, "main.txt")
	run(t, repo, "push", "-q", "origin", "main")
	run(t, repo, "checkout", "-q", "-b", "child")
	run(t, repo, "cherry-pick", "main~1..parent")
	if run(t, repo, "rev-parse", "HEAD") == parent {
		t.Fatal("the rebased copy kept the parent's SHA")
	}
	child := commit(t, repo, "c.txt")
	run(t, repo, "checkout", "-q", "-b", "onparent", "parent")
	onParent := commit(t, repo, "e.txt")
	run(t, repo, "checkout", "-q", "-b", "hotfix", "main")
	hotfix := commit(t, repo, "d.txt")
	run(t, repo, "push", "-q", "origin", "parent", "child", "hotfix", "onparent")

	repoRow := &models.Repo{ID: "r", LocalPath: repo, OriginURL: "https://github.com/acme/widgets.git", DefaultBaseBranch: "main"}
	listing := &scriptedListing{prs: []vcs.PRSummary{
		{Number: 10, Title: "Parent", HeadBranch: "parent", BaseBranch: "main", HeadSHA: parent},
		{Number: 12, Title: "Child", HeadBranch: "child", BaseBranch: "main", HeadSHA: child},
		{Number: 13, Title: "Hotfix", HeadBranch: "hotfix", BaseBranch: "production", HeadSHA: hotfix},
		{Number: 14, Title: "On parent", HeadBranch: "onparent", BaseBranch: "parent", HeadSHA: onParent},
	}}
	d := NewDetector(listing, gitpkg.NewManager(zerolog.Nop()), zerolog.Nop())
	sessions := []*models.Session{session("s10", 10), session("s12", 12), session("s13", 13), session("s14", 14), session("s99", 99), {ID: "no-pr"}}
	d.Refresh(t.Context(), repoRow, sessions)

	onPR10 := func(isBase bool) []Parent {
		return []Parent{{Number: 10, Title: "Parent", URL: "https://github.com/acme/widgets/pull/10", HeadBranch: "parent", SharedCommits: 2, Commits: 2, IsBaseBranch: isBase}}
	}
	want := map[string]Result{
		"s10": {BaseBranch: "main", DefaultBranch: "main"},
		"s12": {BaseBranch: "main", DefaultBranch: "main", StackedOn: onPR10(false)},
		"s13": {BaseBranch: "production", DefaultBranch: "main"},
		"s14": {BaseBranch: "parent", DefaultBranch: "main", StackedOn: onPR10(true)},
	}
	assertResults(t, d, want, "s99", "no-pr")

	run(t, repo, "checkout", "-q", "main")
	run(t, repo, "merge", "-q", "--no-ff", "-m", "merge parent", "parent")
	run(t, repo, "push", "-q", "origin", "main")
	listing.prs = listing.prs[1:]
	listing.prs[2].BaseBranch = "main"
	d.Refresh(t.Context(), repoRow, sessions)
	want["s12"] = Result{BaseBranch: "main", DefaultBranch: "main"}
	want["s14"] = Result{BaseBranch: "main", DefaultBranch: "main"}
	delete(want, "s10")
	assertResults(t, d, want, "s10", "s99", "no-pr")
}

// A parent that merged into the default branch under new SHAs leaves two
// open children still carrying copies of its commits. The copies are no
// longer either child's own, so the older child is not mistaken for the
// newer one's parent.
func TestDetectorIgnoresCommitsMergedUnderNewSHAs(t *testing.T) {
	origin, repo := t.TempDir(), t.TempDir()
	run(t, origin, "init", "-q", "--bare", "-b", "main")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "test@example.test"}, {"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "init"}, {"remote", "add", "origin", origin},
	} {
		run(t, repo, args...)
	}
	run(t, repo, "checkout", "-q", "-b", "parent")
	commit(t, repo, "a.txt")
	commit(t, repo, "b.txt")
	run(t, repo, "checkout", "-q", "-b", "older", "parent")
	older := commit(t, repo, "c.txt")
	run(t, repo, "checkout", "-q", "-b", "newer", "parent")
	newer := commit(t, repo, "d.txt")
	run(t, repo, "checkout", "-q", "main")
	commit(t, repo, "main.txt")
	run(t, repo, "cherry-pick", "main~1..parent")
	run(t, repo, "push", "-q", "origin", "main", "older", "newer")

	repoRow := &models.Repo{ID: "r", LocalPath: repo, OriginURL: "https://github.com/acme/widgets.git", DefaultBaseBranch: "main"}
	listing := &scriptedListing{prs: []vcs.PRSummary{
		{Number: 20, Title: "Older", HeadBranch: "older", BaseBranch: "main", HeadSHA: older},
		{Number: 21, Title: "Newer", HeadBranch: "newer", BaseBranch: "main", HeadSHA: newer},
	}}
	d := NewDetector(listing, gitpkg.NewManager(zerolog.Nop()), zerolog.Nop())
	d.Refresh(t.Context(), repoRow, []*models.Session{session("s20", 20), session("s21", 21)})
	assertResults(t, d, map[string]Result{
		"s20": {BaseBranch: "main", DefaultBranch: "main"},
		"s21": {BaseBranch: "main", DefaultBranch: "main"},
	})
}

func assertResults(t *testing.T, d *Detector, want map[string]Result, absent ...string) {
	t.Helper()
	for id, w := range want {
		if got, ok := d.Stack(id); !ok || !reflect.DeepEqual(got, w) {
			t.Fatalf("Stack(%s) = %+v, %v; want %+v", id, got, ok, w)
		}
	}
	for _, id := range absent {
		if got, ok := d.Stack(id); ok {
			t.Fatalf("Stack(%s) = %+v; want no result", id, got)
		}
	}
}

// The detector reads git only when the open PRs changed, the result is ten
// minutes old, or a failed pass's retry is due, reuses patch ids measured
// from the same default tip, so a partial pass's retry rereads only the head
// it missed, and reports a session only when its result changed.
func TestDetectorRecomputeAndOnChange(t *testing.T) {
	tip := strings.Repeat("0", 40)
	listing := &scriptedListing{prs: []vcs.PRSummary{
		{Number: 1, HeadBranch: "one", BaseBranch: "main", HeadSHA: "h1"},
		{Number: 2, HeadBranch: "two", BaseBranch: "main", HeadSHA: "h2"},
	}}
	g := &countingGit{tip: tip, ids: map[string][]string{"h1": {"p1"}, "h2": {"p1", "p2"}}}
	d := NewDetector(listing, g, zerolog.Nop())
	clock := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return clock }
	var changed []string
	d.SetOnChange(func(id string) { changed = append(changed, id) })
	repoRow := &models.Repo{ID: "r", LocalPath: "/repo", OriginURL: "o/r", DefaultBaseBranch: "main"}
	sessions := []*models.Session{session("s1", 1), session("s2", 2)}
	refresh := func(step string, wantTipCalls int, wantReads []string, wantChanged ...string) {
		t.Helper()
		changed = nil
		d.Refresh(t.Context(), repoRow, sessions)
		if g.tipCalls != wantTipCalls || !slices.Equal(g.readHeads, wantReads) || !slices.Equal(changed, wantChanged) {
			t.Fatalf("%s: tip reads %d, patch-id reads %v, changed %v; want %d, %v, %v", step, g.tipCalls, g.readHeads, changed, wantTipCalls, wantReads, wantChanged)
		}
	}

	refresh("first pass", 1, []string{"h1", "h2"}, "s1", "s2")
	if r, _ := d.Stack("s2"); len(r.StackedOn) != 1 || r.StackedOn[0].Number != 1 {
		t.Fatalf("s2 = %+v; want stacked on #1", r)
	}
	refresh("unchanged listing", 1, []string{"h1", "h2"})
	clock = clock.Add(recomputeAfter)
	refresh("ten minutes later, same tip", 2, []string{"h1", "h2"})

	listing.prs[1].HeadSHA, g.ids["h2b"] = "h2b", []string{"p2"}
	refresh("new head unstacks #2", 3, []string{"h1", "h2", "h2b"}, "s2")

	g.tipErr = errors.New("offline")
	listing.prs[0].BaseBranch = "production"
	refresh("git failure keeps results", 4, []string{"h1", "h2", "h2b"})
	if r, _ := d.Stack("s1"); r.BaseBranch != "main" {
		t.Fatalf("s1 = %+v; want the previous base kept through a git failure", r)
	}
	refresh("retry not due", 4, []string{"h1", "h2", "h2b"})
	g.tipErr = nil
	clock = clock.Add(retryAfter)
	refresh("retry due", 5, []string{"h1", "h2", "h2b"}, "s1")

	listing.prs[0].BaseBranch = "staging"
	listing.prs[1].HeadSHA, g.ids["h2c"], g.unreadable = "h2c", []string{"p1", "p3"}, "h2c"
	refresh("a partial pass serves what it read", 6, []string{"h1", "h2", "h2b", "h2c"}, "s1")
	refresh("partial retry not due", 6, []string{"h1", "h2", "h2b", "h2c"})
	g.unreadable = ""
	clock = clock.Add(retryAfter)
	refresh("partial retry rereads only the missed head", 7, []string{"h1", "h2", "h2b", "h2c", "h2c"}, "s2")
	if r, _ := d.Stack("s2"); len(r.StackedOn) != 1 || r.StackedOn[0].Number != 1 {
		t.Fatalf("s2 = %+v; want stacked on #1 once its head was read", r)
	}
	reads := slices.Clone(g.readHeads)

	listing.err = errors.New("rate limited")
	refresh("listing failure keeps results", 7, reads)
	if _, ok := d.Stack("s1"); !ok {
		t.Fatal("s1 lost its result to a listing failure")
	}
	listing.err = nil

	sessions = sessions[:1]
	refresh("archived session dropped", 7, reads, "s2")
	listed := listing.calls
	sessions[0].PRNumber = nil
	refresh("no session PR serves nothing", 7, reads, "s1")
	if listing.calls != listed {
		t.Fatalf("listed open PRs %d more times for a repo with no session PR", listing.calls-listed)
	}
	sessions = []*models.Session{session("s1", 1)}
	refresh("session PR back", 7, reads, "s1")
	repoRow.LocalPath = ""
	refresh("repo without a clone serves nothing", 7, reads, "s1")
}
