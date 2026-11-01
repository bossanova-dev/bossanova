package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
)

// emptySnapshot is a one-page open-PR snapshot of a repository with no open PRs.
const emptySnapshot = `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`

// withoutSnapshot makes every open-PR snapshot query fail, so a test of the
// per-PR read path keeps exercising it under vcs.WithCachedReads.
func withoutSnapshot(f ghFunc) ghFunc {
	return func(ctx context.Context, args ...string) (string, error) {
		if isGraphQL(args) {
			return "", errors.New("snapshot unavailable in this test")
		}
		return f(ctx, args...)
	}
}

func isGraphQL(args []string) bool { return len(args) >= 2 && args[0] == "api" && args[1] == "graphql" }

func isPerPRRead(args []string) bool {
	if len(args) >= 2 && args[0] == "pr" && (args[1] == "view" || args[1] == "checks" || args[1] == "list") {
		return true
	}
	return len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/pulls/")
}

// routeKey names one gh invocation: "graphql" (plus " cursor=<c>" on a later
// page), "pr view 7", "pr checks 7", "pr list", or "api <path>".
func routeKey(args []string) string {
	switch {
	case isGraphQL(args):
		for _, a := range args {
			if strings.HasPrefix(a, "cursor=") {
				return "graphql " + a
			}
		}
		return "graphql"
	case len(args) >= 3 && args[0] == "pr" && (args[1] == "view" || args[1] == "checks"):
		return "pr " + args[1] + " " + args[2]
	case len(args) >= 2 && args[0] == "pr":
		return "pr " + args[1]
	case len(args) >= 2 && args[0] == "api":
		return "api " + args[1]
	}
	return strings.Join(args, " ")
}

// routedGH answers each gh invocation from routes by routeKey. A route value
// starting with "error:" fails the call with the rest as its message.
func routedGH(t *testing.T, routes map[string]string) *scriptedGH {
	t.Helper()
	var mu sync.Mutex
	return &scriptedGH{reply: func(args []string) (string, error) {
		mu.Lock()
		out, ok := routes[routeKey(args)]
		mu.Unlock()
		if !ok {
			t.Errorf("unexpected gh call %q", routeKey(args))
			return "", fmt.Errorf("unexpected gh call %v", args)
		}
		if msg, failed := strings.CutPrefix(out, "error:"); failed {
			return "", errors.New(msg)
		}
		return out, nil
	}}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func callKeys(g *scriptedGH) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := make([]string, len(g.calls))
	for i, c := range g.calls {
		keys[i] = routeKey(c)
	}
	return keys
}

// perPRRoutes derives, from a snapshot fixture, the replies the per-PR path
// would get for the same PR state: `gh pr view` reports bot reviewers under a
// bare login with no type, the REST pull carries mergeability and
// rebaseability, and the REST review list spells bots login[bot].
func perPRRoutes(t *testing.T, fixture string) map[string]string {
	t.Helper()
	snap := &prSnapshot{p: New(zerolog.Nop()), prs: map[int]*snapshotPR{}}
	if _, err := snap.addPage(fixture); err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{}
	for n, pr := range snap.prs {
		fields := pr.prStatusFields
		rest := []map[string]any{}
		fields.Reviews = nil
		for i, r := range pr.Reviews {
			login := r.Author.Login
			if r.Author.Typename == "Bot" {
				login += "[bot]"
			}
			rest = append(rest, map[string]any{"id": i + 1, "node_id": r.ID, "user": map[string]any{"login": login},
				"author_association": r.AuthorAssociation, "state": r.State, "body": ""})
			r.Author.Typename = ""
			fields.Reviews = append(fields.Reviews, r)
		}
		var mergeable any
		if pr.Mergeable != "UNKNOWN" {
			mergeable = pr.Mergeable == "MERGEABLE"
		}
		routes[fmt.Sprintf("pr view %d", n)] = mustJSON(t, fields)
		routes[fmt.Sprintf("api repos/o/r/pulls/%d", n)] = mustJSON(t, map[string]any{
			"mergeable": mergeable, "mergeable_state": strings.ToLower(pr.MergeStateStatus), "rebaseable": pr.CanBeRebased,
		})
		routes[fmt.Sprintf("api repos/o/r/pulls/%d/reviews?per_page=100", n)] = mustJSON(t, rest)
	}
	return routes
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestOpenPRSnapshotBatchesBackgroundReads is the saving: every background
// status, checks and open-PR read for a repo's open PRs costs one GraphQL
// query, with no per-PR `gh pr view`, `gh pr checks` or REST pulls/N call.
func TestOpenPRSnapshotBatchesBackgroundReads(t *testing.T) {
	gh := routedGH(t, map[string]string{"graphql": readFixture(t, "open_pr_snapshot_three.json")})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
	bg := vcs.WithCachedReads(context.Background())

	for _, n := range []int{1, 2, 3} {
		if _, err := p.GetPRStatus(bg, "o/r", n); err != nil {
			t.Fatalf("status %d: %v", n, err)
		}
		if _, err := p.GetCheckSet(bg, "o/r", n); err != nil {
			t.Fatalf("checks %d: %v", n, err)
		}
	}
	prs, err := p.ListOpenPRs(bg, "https://github.com/o/r.git")
	if err != nil || len(prs) != 3 {
		t.Fatalf("ListOpenPRs = %v, %v; want 3 PRs", prs, err)
	}
	if got := callKeys(gh); !reflect.DeepEqual(got, []string{"graphql"}) {
		t.Fatalf("gh calls = %q, want one graphql query", got)
	}
	t.Logf("batched gh invocations: %q", callKeys(gh))
}

// TestOpenPRSnapshotPagesWithCursor pins paging: a second page costs exactly
// one more query, requested with the cursor the first page returned.
func TestOpenPRSnapshotPagesWithCursor(t *testing.T) {
	gh := routedGH(t, map[string]string{
		"graphql":                     readFixture(t, "open_pr_snapshot_page1.json"),
		"graphql cursor=Y3Vyc29yOjE=": readFixture(t, "open_pr_snapshot_page2.json"),
	})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
	bg := vcs.WithCachedReads(context.Background())

	prs, err := p.ListOpenPRs(bg, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].Number != 11 || prs[1].Number != 12 {
		t.Fatalf("open PRs = %+v, want 11 then 12", prs)
	}
	if s, err := p.GetPRStatus(bg, "o/r", 12); err != nil || !s.Draft {
		t.Fatalf("PR 12 status = %+v, %v; want the second page's draft", s, err)
	}
	want := []string{"graphql", "graphql cursor=Y3Vyc29yOjE="}
	if got := callKeys(gh); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls = %q, want %q", got, want)
	}
}

// TestOpenPRSnapshotStatusParity pins one decision path: for every PR shape
// the snapshot represents, its PRStatus equals what the per-PR read returns for
// the same PR state.
func TestOpenPRSnapshotStatusParity(t *testing.T) {
	fixture := readFixture(t, "open_pr_snapshot_parity.json")
	snapGH := routedGH(t, map[string]string{"graphql": fixture})
	snapP := New(zerolog.Nop(), WithRunGH(snapGH.run), WithNowFunc(newFakeClock().Now))
	perGH := routedGH(t, perPRRoutes(t, fixture))
	perP := New(zerolog.Nop(), WithRunGH(perGH.run))
	bg := vcs.WithCachedReads(context.Background())

	yes, no := true, false
	cases := []struct {
		pr         int
		name       string
		mergeable  *bool
		rebaseable *bool
		merge      vcs.MergeStateStatus
		latest     vcs.ReviewState
	}{
		{1, "clean; bot review moves state, untrusted human does not", &yes, &yes, vcs.MergeStateStatusClean, vcs.ReviewStateCommented},
		{2, "conflicting", &no, nil, vcs.MergeStateStatusDirty, vcs.ReviewStateUnspecified},
		{3, "review required, not rebaseable", &yes, &no, vcs.MergeStateStatusBlocked, vcs.ReviewStateRequired},
		{4, "behind; APPROVED decision overrides a later comment", &yes, &yes, vcs.MergeStateStatusBehind, vcs.ReviewStateApproved},
		{5, "draft", &yes, nil, vcs.MergeStateStatusDraft, vcs.ReviewStateUnspecified},
		{6, "cross-repository; CHANGES_REQUESTED decision overrides", &yes, &yes, vcs.MergeStateStatusBlocked, vcs.ReviewStateChangesRequested},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := snapP.GetPRStatus(bg, "o/r", tc.pr)
			if err != nil {
				t.Fatal(err)
			}
			want, err := perP.fetchPRStatus(context.Background(), "o/r", tc.pr)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot status = %+v\nper-PR status   = %+v", got, want)
			}
			if !reflect.DeepEqual(got.Mergeable, tc.mergeable) || !reflect.DeepEqual(got.Rebaseable, tc.rebaseable) ||
				got.MergeStateStatus != tc.merge || got.LatestReviewState != tc.latest {
				t.Fatalf("status = %+v; want mergeable %v rebaseable %v merge %v latest %v",
					got, tc.mergeable, tc.rebaseable, tc.merge, tc.latest)
			}
		})
	}
	if s, _ := snapP.GetPRStatus(bg, "o/r", 6); !s.IsCrossRepository {
		t.Fatal("PR 6 lost IsCrossRepository")
	}
	if got := snapGH.count(func(a []string) bool { return !isGraphQL(a) }); got != 0 {
		t.Fatalf("snapshot reads made %d per-PR calls, want 0", got)
	}
}

// TestOpenPRSnapshotCheckSetParity pins the rollup-to-row conversion: the
// snapshot's CheckSet equals `gh pr checks` fed the rows gh prints for the same
// rollup (newest first, a re-run job collapsed to its latest run).
func TestOpenPRSnapshotCheckSetParity(t *testing.T) {
	gh := routedGH(t, map[string]string{"graphql": readFixture(t, "open_pr_snapshot_parity.json")})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
	bg := vcs.WithCachedReads(context.Background())

	got, err := p.GetCheckSet(bg, "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	rows := `[{"name":"lint","state":"SUCCESS","workflow":"ci","description":""},
{"name":"test","state":"IN_PROGRESS","workflow":"ci","description":""},
{"name":"deploy","state":"FAILURE","workflow":"release","description":""},
{"name":"build","state":"SUCCESS","workflow":"ci","description":""},
{"name":"boss/build","state":"SUCCESS","workflow":"","description":"built by boss-build"},
{"name":"boss/verify","state":"PENDING","workflow":"","description":"verify: claimed by delta"}]`
	perGH := routedGH(t, map[string]string{"pr checks 1": rows})
	want, err := New(zerolog.Nop(), WithRunGH(perGH.run)).fetchCheckSet(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot checks = %+v\nper-PR checks   = %+v", got, want)
	}
	if len(got.Checks) != 5 || !got.HasBuildReceipt || got.Checks[0].Name != "lint" || *got.Checks[0].Conclusion != vcs.CheckConclusionSuccess {
		t.Fatalf("checks = %+v; want 5 gates led by the re-run lint success, with a build receipt", got)
	}

	empty, err := p.GetCheckSet(bg, "o/r", 2)
	if err != nil || empty.Checks == nil || len(empty.Checks) != 0 {
		t.Fatalf("empty rollup = %+v, %v; want an empty, non-nil Checks", empty, err)
	}
}

// TestOpenPRSnapshotListMatchesGHPRList pins the open-PR list projection,
// including gh's app/<login> spelling of a bot author.
func TestOpenPRSnapshotListMatchesGHPRList(t *testing.T) {
	gh := routedGH(t, map[string]string{"graphql": readFixture(t, "open_pr_snapshot_parity.json")})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))

	prs, err := p.ListOpenPRs(vcs.WithCachedReads(context.Background()), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	want0 := vcs.PRSummary{Number: 1, Title: "clean", HeadBranch: "feature-1", State: vcs.PRStateOpen, Author: "app/dependabot"}
	if len(prs) != 9 || prs[0] != want0 || prs[1].Author != "alice" {
		t.Fatalf("open PRs = %+v; want 9 led by %+v", prs, want0)
	}
}

// TestOpenPRSnapshotDeclinesFallThroughToPerPRReads pins exactness: a PR the
// snapshot cannot reproduce is read per PR as today, and that read's answer
// is what the caller gets.
func TestOpenPRSnapshotDeclinesFallThroughToPerPRReads(t *testing.T) {
	perPRView := `{"state":"OPEN","isDraft":true,"title":"per-pr","reviews":[]}`
	cases := []struct {
		name  string
		pr    int
		check bool
	}{
		{"absent from the snapshot", 99, false},
		{"mergeability unknown", 7, false},
		{"truncated reviews with no state-moving review", 8, false},
		{"truncated rollup", 9, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := routedGH(t, map[string]string{
				"graphql":                          readFixture(t, "open_pr_snapshot_parity.json"),
				fmt.Sprintf("pr view %d", tc.pr):   perPRView,
				fmt.Sprintf("pr checks %d", tc.pr): `[{"name":"per-pr","state":"SUCCESS","workflow":"ci","description":""}]`,
			})
			p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
			bg := vcs.WithCachedReads(context.Background())
			if tc.check {
				set, err := p.GetCheckSet(bg, "o/r", tc.pr)
				if err != nil || len(set.Checks) != 1 || set.Checks[0].Name != "per-pr" {
					t.Fatalf("checks = %+v, %v; want the per-PR read", set, err)
				}
			} else {
				s, err := p.GetPRStatus(bg, "o/r", tc.pr)
				if err != nil || s.Title != "per-pr" {
					t.Fatalf("status = %+v, %v; want the per-PR read", s, err)
				}
			}
			if got := gh.count(isPerPRRead); got != 1 {
				t.Fatalf("per-PR calls = %d (%q), want 1", got, callKeys(gh))
			}
		})
	}
}

// TestOpenPRSnapshotRateLimitedSpendsNothingPerPR pins the quota guard: a
// snapshot refused for an exhausted GraphQL quota returns ErrRateLimited and
// does not fall back to per-PR calls against the same exhausted quota.
func TestOpenPRSnapshotRateLimitedSpendsNothingPerPR(t *testing.T) {
	clock := newFakeClock()
	gh := routedGH(t, map[string]string{
		"graphql":        "error:" + graphQLExhausted,
		"api rate_limit": rateLimitBody(clock, 0, 30*time.Minute),
	})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	_, err := p.GetPRStatus(vcs.WithCachedReads(context.Background()), "o/r", 7)
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if got := gh.count(isPerPRRead); got != 0 {
		t.Fatalf("per-PR calls after an exhausted quota = %d, want 0", got)
	}
}

// TestOpenPRSnapshotMalformedFallsThrough pins that a snapshot GitHub answered
// with garbage can never blank a loop: the read degrades to per-PR.
func TestOpenPRSnapshotMalformedFallsThrough(t *testing.T) {
	gh := routedGH(t, map[string]string{
		"graphql":     `{"data":{"repository":null}}`,
		"pr checks 7": `[{"name":"build","state":"SUCCESS","workflow":"ci","description":""}]`,
	})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))

	set, err := p.GetCheckSet(vcs.WithCachedReads(context.Background()), "o/r", 7)
	if err != nil || len(set.Checks) != 1 {
		t.Fatalf("checks = %+v, %v; want the per-PR read", set, err)
	}
}

// TestOpenPRSnapshotPartialFetchStoresNothing pins all-or-nothing paging: a
// failed second page stores no snapshot, so the first read after the failure
// backoff starts again from the first page.
func TestOpenPRSnapshotPartialFetchStoresNothing(t *testing.T) {
	routes := map[string]string{
		"graphql":                     readFixture(t, "open_pr_snapshot_page1.json"),
		"graphql cursor=Y3Vyc29yOjE=": "error:HTTP 502: Bad Gateway",
		"pr view 11":                  `{"state":"OPEN","isDraft":true,"title":"per-pr","reviews":[]}`,
	}
	gh := routedGH(t, routes)
	clock := newFakeClock()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	if s, err := p.GetPRStatus(bg, "o/r", 11); err != nil || s.Title != "per-pr" {
		t.Fatalf("status = %+v, %v; want the per-PR fallback", s, err)
	}
	clock.Advance(defaultReadCacheTTL)
	gh.mu.Lock()
	routes["graphql cursor=Y3Vyc29yOjE="] = readFixture(t, "open_pr_snapshot_page2.json")
	gh.mu.Unlock()
	if _, err := p.GetCheckSet(bg, "o/r", 11); err != nil {
		t.Fatal(err)
	}
	want := []string{"graphql", "graphql cursor=Y3Vyc29yOjE=", "pr view 11", "graphql", "graphql cursor=Y3Vyc29yOjE="}
	if got := callKeys(gh); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls = %q, want %q", got, want)
	}
}

// TestOpenPRSnapshotFailureBacksOff pins the spend bound for a snapshot that
// keeps failing: inside one TTL of a failure, background reads go straight to
// the per-PR read; a webhook or the TTL ending lets the snapshot be tried again.
func TestOpenPRSnapshotFailureBacksOff(t *testing.T) {
	gh := routedGH(t, map[string]string{
		"graphql":      "error:HTTP 502: Bad Gateway",
		"pr view 11":   `{"state":"OPEN","isDraft":true,"title":"per-pr","reviews":[]}`,
		"pr checks 11": `[]`,
	})
	clock := newFakeClock()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	_, _ = p.GetPRStatus(bg, "o/r", 11)
	clock.Advance(defaultReadCacheTTL - time.Second)
	if _, err := p.GetCheckSet(bg, "o/r", 11); err != nil {
		t.Fatal(err)
	}
	want := []string{"graphql", "pr view 11", "pr checks 11"}
	if got := callKeys(gh); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls inside the backoff = %q, want %q", got, want)
	}
	p.InvalidatePR("o/r", 11)
	_, _ = p.GetPRStatus(bg, "o/r", 11)
	clock.Advance(defaultReadCacheTTL)
	_, _ = p.GetCheckSet(bg, "o/r", 11)
	want = append(want, "graphql", "pr view 11", "graphql", "pr checks 11")
	if got := callKeys(gh); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls after a webhook and after the TTL = %q, want %q", got, want)
	}
}

// TestOpenPRSnapshotFailureAfterCallerGaveUpBacksOff pins that the detached
// flight records its own failure: a snapshot that fails after its caller's
// poll timeout still keeps the next background read off the snapshot.
func TestOpenPRSnapshotFailureAfterCallerGaveUpBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		gh := &scriptedGH{reply: func(args []string) (string, error) {
			switch {
			case isGraphQL(args):
				once.Do(func() { close(started) })
				<-release
				return "", errors.New("HTTP 502: Bad Gateway")
			case routeKey(args) == "pr view 11":
				return `{"state":"OPEN","isDraft":true,"title":"per-pr","reviews":[]}`, nil
			}
			return "", fmt.Errorf("unexpected gh call %v", args)
		}}
		p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
		ctx, cancel := context.WithCancel(vcs.WithCachedReads(context.Background()))
		done := make(chan error, 1)
		go func() {
			_, err := p.GetPRStatus(ctx, "o/r", 11)
			done <- err
		}()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("caller err = %v, want context.Canceled", err)
		}
		close(release)
		synctest.Wait()

		if s, err := p.GetPRStatus(vcs.WithCachedReads(context.Background()), "o/r", 11); err != nil || s.Title != "per-pr" {
			t.Fatalf("status = %+v, %v; want the per-PR read", s, err)
		}
		if got := gh.count(isGraphQL); got != 1 {
			t.Fatalf("snapshot queries = %d, want 1 (the failed flight backs off)", got)
		}
	})
}

// TestOpenPRSnapshotFreshness pins today's freshness: the snapshot serves
// background reads for one TTL and is refetched after it.
func TestOpenPRSnapshotFreshness(t *testing.T) {
	clock := newFakeClock()
	gh := routedGH(t, map[string]string{"graphql": readFixture(t, "open_pr_snapshot_three.json")})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	_, _ = p.GetPRStatus(bg, "o/r", 1)
	clock.Advance(defaultReadCacheTTL - time.Second)
	_, _ = p.GetCheckSet(bg, "o/r", 2)
	if got := gh.count(isGraphQL); got != 1 {
		t.Fatalf("snapshot queries inside the TTL = %d, want 1", got)
	}
	clock.Advance(time.Second)
	_, _ = p.GetCheckSet(bg, "o/r", 2)
	if got := gh.count(isGraphQL); got != 2 {
		t.Fatalf("snapshot queries after the TTL = %d, want 2", got)
	}
}

// TestOpenPRSnapshotInvalidatedByAnyPR pins the webhook hook: InvalidatePR for
// one PR marks the whole repo snapshot stale, so a read of another PR in the
// repo refetches it.
func TestOpenPRSnapshotInvalidatedByAnyPR(t *testing.T) {
	gh := routedGH(t, map[string]string{"graphql": readFixture(t, "open_pr_snapshot_three.json")})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
	bg := vcs.WithCachedReads(context.Background())

	_, _ = p.GetPRStatus(bg, "o/r", 1)
	p.InvalidatePR("https://github.com/O/R.git", 1)
	_, _ = p.GetPRStatus(bg, "o/r", 2)
	if got := gh.count(isGraphQL); got != 2 {
		t.Fatalf("snapshot queries = %d, want a refetch after InvalidatePR", got)
	}
}

// TestOpenPRSnapshotInvalidationBeatsInFlightFetch pins the generation guard
// for snapshots: a fetch that began before InvalidatePR never stores.
func TestOpenPRSnapshotInvalidationBeatsInFlightFetch(t *testing.T) {
	snapshot := readFixture(t, "open_pr_snapshot_three.json")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if !isGraphQL(args) {
			return "", fmt.Errorf("unexpected gh call %v", args)
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(started)
			<-release
		}
		return snapshot, nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
	bg := vcs.WithCachedReads(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.GetPRStatus(bg, "o/r", 1)
	}()
	<-started
	p.InvalidatePR("o/r", 3)
	close(release)
	<-done

	_, _ = p.GetPRStatus(bg, "o/r", 2)
	if got := gh.count(isGraphQL); got != 2 {
		t.Fatalf("snapshot queries = %d: the pre-invalidation fetch was stored", got)
	}
}

// TestOpenPRSnapshotFresherPerPRReadWins pins "freshest wins": a fresh
// unmarked read newer than the snapshot is what a later background read of
// that PR returns, while other reads stay on the snapshot.
func TestOpenPRSnapshotFresherPerPRReadWins(t *testing.T) {
	clock := newFakeClock()
	gh := routedGH(t, map[string]string{
		"graphql":     readFixture(t, "open_pr_snapshot_three.json"),
		"pr view 3":   `{"state":"OPEN","isDraft":true,"title":"fresh","reviews":[]}`,
		"pr checks 3": `[]`,
	})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	if s, _ := p.GetPRStatus(bg, "o/r", 3); s == nil || s.Title != "three" {
		t.Fatalf("background status = %+v, want the snapshot's", s)
	}
	clock.Advance(time.Second)
	if _, err := p.GetPRStatus(context.Background(), "o/r", 3); err != nil {
		t.Fatal(err)
	}
	if s, _ := p.GetPRStatus(bg, "o/r", 3); s == nil || s.Title != "fresh" {
		t.Fatalf("background status after a fresher read = %+v, want fresh", s)
	}
	if _, err := p.GetCheckSet(bg, "o/r", 3); err != nil {
		t.Fatal(err)
	}
	want := []string{"graphql", "pr view 3"}
	if got := callKeys(gh); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls = %q, want %q", got, want)
	}
}

// TestOpenPRSnapshotInFlightLosesToNewerPerPRRead pins data age over
// completion order: an unmarked read that stores while a snapshot fetch is in
// flight began after that fetch, so its answer wins for the flight's own caller
// and for later background reads of that PR.
func TestOpenPRSnapshotInFlightLosesToNewerPerPRRead(t *testing.T) {
	snapshot := readFixture(t, "open_pr_snapshot_three.json")
	started, release := make(chan struct{}), make(chan struct{})
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isGraphQL(args):
			close(started)
			<-release
			return snapshot, nil
		case routeKey(args) == "pr view 3":
			return `{"state":"OPEN","isDraft":true,"title":"fresh","reviews":[]}`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	clock := newFakeClock()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	var flight *vcs.PRStatus
	done := make(chan struct{})
	go func() {
		defer close(done)
		flight, _ = p.GetPRStatus(bg, "o/r", 3)
	}()
	<-started
	clock.Advance(time.Second)
	if s, err := p.GetPRStatus(context.Background(), "o/r", 3); err != nil || s.Title != "fresh" {
		t.Fatalf("unmarked status = %+v, %v; want fresh", s, err)
	}
	clock.Advance(time.Second)
	close(release)
	<-done

	if flight == nil || flight.Title != "fresh" {
		t.Fatalf("flight caller's status = %+v, want the newer per-PR read", flight)
	}
	if s, _ := p.GetPRStatus(bg, "o/r", 3); s == nil || s.Title != "fresh" {
		t.Fatalf("background status = %+v, want the newer per-PR read", s)
	}
	if s, _ := p.GetPRStatus(bg, "o/r", 1); s == nil || s.Title == "fresh" {
		t.Fatalf("PR 1 status = %+v, want the snapshot's", s)
	}
	if got := gh.count(isGraphQL); got != 1 {
		t.Fatalf("snapshot queries = %d, want 1", got)
	}
}

// TestOpenPRSnapshotNotConsultedByUnmarkedReads pins the safety side: merge
// gates and webhook-triggered reads always read GitHub per PR, even with a
// fresh snapshot.
func TestOpenPRSnapshotNotConsultedByUnmarkedReads(t *testing.T) {
	gh := routedGH(t, map[string]string{
		"graphql":     readFixture(t, "open_pr_snapshot_three.json"),
		"pr view 3":   `{"state":"OPEN","isDraft":true,"title":"live","reviews":[]}`,
		"pr checks 3": `[]`,
		"pr list":     `[]`,
	})
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(newFakeClock().Now))
	ctx := context.Background()

	_, _ = p.ListOpenPRs(vcs.WithCachedReads(ctx), "o/r")
	if s, err := p.GetPRStatus(ctx, "o/r", 3); err != nil || s.Title != "live" {
		t.Fatalf("unmarked status = %+v, %v; want the live per-PR read", s, err)
	}
	_, _ = p.GetCheckSet(ctx, "o/r", 3)
	_, _ = p.ListOpenPRs(ctx, "o/r")
	want := []string{"graphql", "pr view 3", "pr checks 3", "pr list"}
	if got := callKeys(gh); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls = %q, want %q", got, want)
	}
}
