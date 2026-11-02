package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
)

// reviewActivityThreads answers fetchReviewComments' thread query for PR 7: the
// bot owns one blocking thread, so its COMMENTED review is promoted.
const reviewActivityThreads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},` +
	`"nodes":[{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"author":{"login":"chatgpt-codex-connector"}}]}}]}}}}}`

// reviewActivityInline is one trusted inline comment, the reply to every
// reviews/<id>/comments read.
const reviewActivityInline = `[{"user":{"login":"bob"},"author_association":"MEMBER","body":"nit","path":"a.go","line":3}]`

// reviewFetchCalls is every gh call one full fetchReviewComments of PR 7 makes.
var reviewFetchCalls = []string{
	"api repos/o/r/pulls/7/reviews?per_page=100",
	"threads",
	"api repos/o/r/pulls/7/reviews/101/comments",
	"api repos/o/r/pulls/7/reviews/102/comments",
}

// activityRoute names one gh call: "snapshot" for the open-PR snapshot query,
// "threads" for fetchReviewComments' review-thread query, otherwise routeKey.
func activityRoute(args []string) string {
	if !isGraphQL(args) {
		return routeKey(args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "query=") && strings.Contains(a, "pullRequests(states: OPEN") {
			return "snapshot"
		}
	}
	return "threads"
}

func isReviewRead(args []string) bool {
	return !isGraphQL(args) && strings.Contains(routeKey(args), "/reviews")
}

// reviewActivityGH answers the snapshot query with its current snapshot (a
// value starting "error:" fails it) and per-PR reads from routes. When hold is
// set, the first call routed to it blocks until release closes.
type reviewActivityGH struct {
	*scriptedGH
	mu       sync.Mutex
	snapshot string
	routes   map[string]string
	holds    map[string]activityHold
}

// activityHold blocks one routed call: started closes once it is blocked, and
// it returns once release closes.
type activityHold struct {
	started, release chan struct{}
}

func newReviewActivityGH(t *testing.T, snapshot string) *reviewActivityGH {
	t.Helper()
	g := &reviewActivityGH{snapshot: snapshot, routes: map[string]string{
		"threads": reviewActivityThreads,
		"api repos/o/r/pulls/7/reviews?per_page=100":  readFixture(t, "reviews_review_activity.json"),
		"api repos/o/r/pulls/7/reviews/101/comments":  reviewActivityInline,
		"api repos/o/r/pulls/7/reviews/102/comments":  reviewActivityInline,
		"api repos/o/r/pulls/8/reviews?per_page=100":  `[]`,
		"api repos/o/r/pulls/9/reviews?per_page=100":  `[]`,
		"api repos/o/r/pulls/99/reviews?per_page=100": `[]`,
	}}
	g.scriptedGH = &scriptedGH{reply: func(args []string) (string, error) {
		key := activityRoute(args)
		g.mu.Lock()
		out, ok := g.routes[key]
		if key == "snapshot" {
			out, ok = g.snapshot, true
		}
		h, wait := g.holds[key]
		delete(g.holds, key)
		g.mu.Unlock()
		if wait {
			close(h.started)
			<-h.release
		}
		if !ok {
			t.Errorf("unexpected gh call %q", key)
			return "", fmt.Errorf("unexpected gh call %v", args)
		}
		if msg, failed := strings.CutPrefix(out, "error:"); failed {
			return "", errors.New(msg)
		}
		return out, nil
	}}
	return g
}

func (g *reviewActivityGH) setSnapshot(s string) {
	g.mu.Lock()
	g.snapshot = s
	g.mu.Unlock()
}

// holdFirst makes the next call routed to key block until the returned release
// function runs; started closes once that call is blocked.
func (g *reviewActivityGH) holdFirst(key string) (started <-chan struct{}, release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holds == nil {
		g.holds = map[string]activityHold{}
	}
	h := activityHold{started: make(chan struct{}), release: make(chan struct{})}
	g.holds[key] = h
	return h.started, func() { close(h.release) }
}

func (g *reviewActivityGH) calls() []string {
	g.scriptedGH.mu.Lock()
	defer g.scriptedGH.mu.Unlock()
	keys := make([]string, len(g.scriptedGH.calls))
	for i, c := range g.scriptedGH.calls {
		keys[i] = activityRoute(c)
	}
	return keys
}

// callsSince returns the calls made after the first n.
func (g *reviewActivityGH) callsSince(n int) []string { return g.calls()[n:] }

func newReviewActivityProvider(t *testing.T, snapshot string) (*Provider, *reviewActivityGH, *clocktest.Clock) {
	t.Helper()
	clock := newFakeClock()
	gh := newReviewActivityGH(t, snapshot)
	return New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now)), gh, clock
}

// mutateActivity returns the base fixture with PR 7's node changed by edit.
func mutateActivity(t *testing.T, edit func(pr map[string]any)) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFixture(t, "open_pr_snapshot_review_activity.json")), &doc); err != nil {
		t.Fatal(err)
	}
	conn := doc["data"].(map[string]any)["repository"].(map[string]any)["pullRequests"].(map[string]any)
	edit(conn["nodes"].([]any)[0].(map[string]any))
	return mustJSON(t, doc)
}

func connNodes(pr map[string]any, conn string) []any {
	return pr[conn].(map[string]any)["nodes"].([]any)
}

func connNode(pr map[string]any, conn string, i int) map[string]any {
	return connNodes(pr, conn)[i].(map[string]any)
}

// appendNode adds node to one of PR 7's connections and bumps its totalCount.
func appendNode(pr map[string]any, conn string, node map[string]any) {
	c := pr[conn].(map[string]any)
	c["nodes"] = append(connNodes(pr, conn), node)
	c["totalCount"] = c["totalCount"].(float64) + 1
}

func activityOf(t *testing.T, fixture string, prID int) (string, bool) {
	t.Helper()
	snap := &prSnapshot{p: New(zerolog.Nop()), prs: map[int]*snapshotPR{}}
	if _, err := snap.addPage(fixture); err != nil {
		t.Fatal(err)
	}
	return snap.reviewActivity(prID)
}

func readReviews(t *testing.T, ctx context.Context, p *Provider, prID int) []vcs.ReviewComment {
	t.Helper()
	comments, err := p.GetReviewComments(ctx, "o/r", prID)
	if err != nil {
		t.Fatalf("GetReviewComments(%d): %v", prID, err)
	}
	return comments
}

// readReviewsAsync reads prID's review comments on another goroutine and
// reports the error on the returned channel, so a failure reaches the test
// goroutine instead of calling t.Fatal off it.
func readReviewsAsync(ctx context.Context, p *Provider, prID int) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := p.GetReviewComments(ctx, "o/r", prID)
		done <- err
	}()
	return done
}

func assertCalls(t *testing.T, got, want []string, what string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: gh calls = %q, want %q", what, got, want)
	}
}

// TestReviewActivityUnchangedSkipsReread is the saving: past the read cache's
// TTL, a background review read whose snapshot fingerprint is unchanged costs
// only the shared snapshot query, and returns an equal, independent copy.
func TestReviewActivityUnchangedSkipsReread(t *testing.T) {
	p, gh, clock := newReviewActivityProvider(t, readFixture(t, "open_pr_snapshot_review_activity.json"))
	bg := vcs.WithCachedReads(context.Background())

	first := readReviews(t, bg, p, 7)
	assertCalls(t, gh.calls(), append([]string{"snapshot"}, reviewFetchCalls...), "first read")
	t.Logf("first read gh invocations: %q", gh.calls())
	if len(first) != 4 {
		t.Fatalf("first read = %+v, want the CHANGES_REQUESTED review, its inline comment and the promoted bot review with its own", first)
	}

	clock.Advance(90 * time.Second)
	n := len(gh.calls())
	second := readReviews(t, bg, p, 7)
	assertCalls(t, gh.callsSince(n), []string{"snapshot"}, "read past the TTL with unchanged activity")
	t.Logf("second read gh invocations: %q", gh.callsSince(n))
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("second read = %+v, want %+v", second, first)
	}
	second[0].Body = "mutated"
	if third := readReviews(t, bg, p, 7); third[0].Body != first[0].Body {
		t.Fatalf("third read body = %q: callers share one cached value", third[0].Body)
	}
}

// activityEdit is one change to an input fetchReviewComments reads that the
// snapshot observes, applied to PR 7 of the base fixture.
type activityEdit struct {
	name string
	edit func(pr map[string]any)
}

func activityEdits() []activityEdit {
	return []activityEdit{
		{"new review", func(pr map[string]any) {
			appendNode(pr, "reviews", map[string]any{"id": "PRR_3", "state": "APPROVED", "authorAssociation": "MEMBER",
				"updatedAt": "2026-11-01T11:00:00Z", "comments": map[string]any{"totalCount": 0},
				"author": map[string]any{"login": "carol", "__typename": "User"}})
		}},
		{"review dismissed", func(pr map[string]any) { connNode(pr, "reviews", 0)["state"] = "DISMISSED" }},
		{"review edited", func(pr map[string]any) { connNode(pr, "reviews", 0)["updatedAt"] = "2026-11-01T12:00:00Z" }},
		{"association changed", func(pr map[string]any) { connNode(pr, "reviews", 0)["authorAssociation"] = "CONTRIBUTOR" }},
		{"inline comment added", func(pr map[string]any) {
			connNode(pr, "reviews", 1)["comments"] = map[string]any{"totalCount": 3}
		}},
		{"thread resolved", func(pr map[string]any) { connNode(pr, "reviewThreads", 0)["isResolved"] = true }},
		{"thread outdated", func(pr map[string]any) { connNode(pr, "reviewThreads", 0)["isOutdated"] = true }},
		{"thread added", func(pr map[string]any) {
			appendNode(pr, "reviewThreads", map[string]any{"id": "PRRT_3", "isResolved": false, "isOutdated": false})
		}},
		{"new head commit", func(pr map[string]any) { pr["headRefOid"] = "sha-b" }},
	}
}

// TestReviewActivityFingerprint pins the projection: the digest is
// deterministic, every observed input moves it, and a PR whose activity the
// snapshot cannot fully see is declined.
func TestReviewActivityFingerprint(t *testing.T) {
	base := readFixture(t, "open_pr_snapshot_review_activity.json")
	baseFP, ok := activityOf(t, base, 7)
	if again, _ := activityOf(t, base, 7); !ok || again != baseFP {
		t.Fatalf("fingerprint = %q then %q (ok=%v), want one deterministic digest", baseFP, again, ok)
	}
	for _, tc := range activityEdits() {
		t.Run(tc.name, func(t *testing.T) {
			if fp, ok := activityOf(t, mutateActivity(t, tc.edit), 7); !ok || fp == baseFP {
				t.Fatalf("fingerprint = %q (ok=%v), want one different from the base %q", fp, ok, baseFP)
			}
		})
	}
	for _, pr := range []int{8, 9, 99} { // truncated reviews, truncated threads, absent
		if fp, ok := activityOf(t, base, pr); ok {
			t.Fatalf("reviewActivity(%d) = %q, want a decline", pr, fp)
		}
	}
}

// TestReviewActivityInputsForceReread pins that a moved fingerprint makes the
// next background read past the TTL re-read GitHub.
func TestReviewActivityInputsForceReread(t *testing.T) {
	base := readFixture(t, "open_pr_snapshot_review_activity.json")
	for _, tc := range activityEdits() {
		t.Run(tc.name, func(t *testing.T) {
			p, gh, clock := newReviewActivityProvider(t, base)
			bg := vcs.WithCachedReads(context.Background())
			readReviews(t, bg, p, 7)
			clock.Advance(90 * time.Second)
			gh.setSnapshot(mutateActivity(t, tc.edit))
			n := len(gh.calls())
			readReviews(t, bg, p, 7)
			assertCalls(t, gh.callsSince(n), append([]string{"snapshot"}, reviewFetchCalls...), "read after the activity")
		})
	}
}

// TestReviewActivityDeclinesKeepTodaysTTL pins that a PR whose fingerprint the
// snapshot cannot establish is served as before the fingerprint: from the
// cache inside the TTL, re-read from GitHub after it.
func TestReviewActivityDeclinesKeepTodaysTTL(t *testing.T) {
	base := readFixture(t, "open_pr_snapshot_review_activity.json")
	cases := []struct {
		name     string
		pr       int
		snapshot string
	}{
		{"truncated review window", 8, base},
		{"truncated thread page", 9, base},
		{"absent from the snapshot", 99, base},
		{"snapshot failed", 8, "error:HTTP 502: Bad Gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, gh, clock := newReviewActivityProvider(t, tc.snapshot)
			bg := vcs.WithCachedReads(context.Background())
			reviews := func() int { return gh.count(isReviewRead) }

			readReviews(t, bg, p, tc.pr)
			clock.Advance(defaultReadCacheTTL - time.Second)
			readReviews(t, bg, p, tc.pr)
			if got := reviews(); got != 1 {
				t.Fatalf("review reads inside the TTL = %d, want 1", got)
			}
			clock.Advance(time.Second)
			readReviews(t, bg, p, tc.pr)
			if got := reviews(); got != 2 {
				t.Fatalf("review reads after the TTL = %d, want 2", got)
			}
		})
	}
}

// TestReviewActivityRateLimitedSpendsNothingPerPR pins the quota guard: a
// snapshot refused for an exhausted quota fails the read without a per-PR
// review call against the same quota.
func TestReviewActivityRateLimitedSpendsNothingPerPR(t *testing.T) {
	p, gh, clock := newReviewActivityProvider(t, "error:"+graphQLExhausted)
	gh.routes["api rate_limit"] = rateLimitBody(clock, 0, 30*time.Minute)

	_, err := p.GetReviewComments(vcs.WithCachedReads(context.Background()), "o/r", 7)
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if got := gh.count(isReviewRead); got != 0 {
		t.Fatalf("review reads after an exhausted quota = %d, want 0", got)
	}
}

// TestReviewActivityMaxAgeBoundsAMatchedEntry pins the safety net for what the
// fingerprint cannot see: a matched entry is served up to reviewCommentsMaxAge
// and re-read from GitHub at it.
func TestReviewActivityMaxAgeBoundsAMatchedEntry(t *testing.T) {
	p, gh, clock := newReviewActivityProvider(t, readFixture(t, "open_pr_snapshot_review_activity.json"))
	bg := vcs.WithCachedReads(context.Background())

	readReviews(t, bg, p, 7)
	clock.Advance(reviewCommentsMaxAge - time.Second)
	readReviews(t, bg, p, 7)
	if got := gh.count(isRESTReviewList); got != 1 {
		t.Fatalf("review list reads inside the max age = %d, want 1", got)
	}
	clock.Advance(time.Second)
	readReviews(t, bg, p, 7)
	if got := gh.count(isRESTReviewList); got != 2 {
		t.Fatalf("review list reads at the max age = %d, want 2", got)
	}
}

func isRESTReviewList(args []string) bool {
	return strings.HasSuffix(routeKey(args), "/reviews?per_page=100")
}

// TestReviewActivityStoredFingerprintPredatesFetch pins the ordering
// invariant: an entry only ever carries the fingerprint of a snapshot taken
// before its fetch began, so activity between that snapshot and a later one
// forces a re-read, and a snapshot taken mid-fetch is never stored with it.
func TestReviewActivityStoredFingerprintPredatesFetch(t *testing.T) {
	base := readFixture(t, "open_pr_snapshot_review_activity.json")
	resolved := mutateActivity(t, func(pr map[string]any) { connNode(pr, "reviewThreads", 0)["isResolved"] = true })
	bg := vcs.WithCachedReads(context.Background())

	t.Run("background fetch after older snapshot", func(t *testing.T) {
		p, gh, clock := newReviewActivityProvider(t, base)
		if _, err := p.ListOpenPRs(bg, "o/r"); err != nil { // S1 at T0
			t.Fatal(err)
		}
		gh.setSnapshot(resolved) // activity at T1
		clock.Advance(10 * time.Second)
		readReviews(t, bg, p, 7) // fetched at T2 with S1's fingerprint
		clock.Advance(defaultReadCacheTTL)
		readReviews(t, bg, p, 7) // S2 at T3
		if got := gh.count(isRESTReviewList); got != 2 {
			t.Fatalf("review list reads = %d, want a re-read after S2", got)
		}
	})
	t.Run("snapshot taken during an unmarked fetch", func(t *testing.T) {
		p, gh, clock := newReviewActivityProvider(t, base)
		started, release := gh.holdFirst("api repos/o/r/pulls/7/reviews?per_page=100")
		done := readReviewsAsync(context.Background(), p, 7)
		<-started
		clock.Advance(time.Second)
		if _, err := p.ListOpenPRs(bg, "o/r"); err != nil { // snapshot after the fetch began
			t.Fatal(err)
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		clock.Advance(defaultReadCacheTTL)
		readReviews(t, bg, p, 7)
		if got := gh.count(isRESTReviewList); got != 2 {
			t.Fatalf("review list reads = %d: the entry carried a snapshot taken after its fetch began", got)
		}
	})
	t.Run("snapshot that began before an unmarked fetch but completed after it", func(t *testing.T) {
		p, gh, clock := newReviewActivityProvider(t, base)
		snapStarted, snapRelease := gh.holdFirst("snapshot")
		snapDone := make(chan error, 1)
		go func() { // S begins at T0 and reads PR 7 only once released
			_, err := p.ListOpenPRs(bg, "o/r")
			snapDone <- err
		}()
		<-snapStarted
		clock.Advance(time.Second)
		fetchStarted, fetchRelease := gh.holdFirst("api repos/o/r/pulls/7/reviews?per_page=100")
		done := readReviewsAsync(context.Background(), p, 7) // unmarked fetch begins at T1
		<-fetchStarted
		clock.Advance(time.Second)
		snapRelease() // S completes at T2, stored as fetched at T0
		if err := <-snapDone; err != nil {
			t.Fatal(err)
		}
		fetchRelease()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		clock.Advance(defaultReadCacheTTL)
		readReviews(t, bg, p, 7)
		if got := gh.count(isRESTReviewList); got != 2 {
			t.Fatalf("review list reads = %d: the entry carried a snapshot that completed after its fetch began", got)
		}
	})
}

// TestReviewActivityUnmarkedReadsAlwaysFetch pins the safety side: a read
// without vcs.WithCachedReads always reads GitHub and never fetches the
// snapshot, and its entry carries the cached snapshot's fingerprint when that
// snapshot predates it, so background reads can reuse it.
func TestReviewActivityUnmarkedReadsAlwaysFetch(t *testing.T) {
	p, gh, clock := newReviewActivityProvider(t, readFixture(t, "open_pr_snapshot_review_activity.json"))
	bg := vcs.WithCachedReads(context.Background())

	readReviews(t, context.Background(), p, 7)
	assertCalls(t, gh.calls(), reviewFetchCalls, "unmarked read with no snapshot")

	if _, err := p.ListOpenPRs(bg, "o/r"); err != nil {
		t.Fatal(err)
	}
	for _, what := range []string{"unmarked read under a fresh snapshot", "unmarked read with a matching entry"} {
		n := len(gh.calls())
		readReviews(t, context.Background(), p, 7)
		assertCalls(t, gh.callsSince(n), reviewFetchCalls, what)
	}

	clock.Advance(90 * time.Second)
	n := len(gh.calls())
	readReviews(t, bg, p, 7)
	assertCalls(t, gh.callsSince(n), []string{"snapshot"}, "background read after an unmarked read under the snapshot")
}

// TestReviewActivityInvalidationForcesReread pins the webhook hook: after
// InvalidatePR a background read re-reads GitHub even with an unchanged
// fingerprint, and a fetch begun before the invalidation stores nothing.
func TestReviewActivityInvalidationForcesReread(t *testing.T) {
	base := readFixture(t, "open_pr_snapshot_review_activity.json")
	bg := vcs.WithCachedReads(context.Background())

	t.Run("after the read", func(t *testing.T) {
		p, gh, _ := newReviewActivityProvider(t, base)
		readReviews(t, bg, p, 7)
		p.InvalidatePR("o/r", 7)
		n := len(gh.calls())
		readReviews(t, bg, p, 7)
		assertCalls(t, gh.callsSince(n), append([]string{"snapshot"}, reviewFetchCalls...), "read after InvalidatePR")
	})
	t.Run("during the read", func(t *testing.T) {
		p, gh, _ := newReviewActivityProvider(t, base)
		started, release := gh.holdFirst("api repos/o/r/pulls/7/reviews?per_page=100")
		done := readReviewsAsync(bg, p, 7)
		<-started
		p.InvalidatePR("o/r", 7)
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		readReviews(t, bg, p, 7)
		if got := gh.count(isRESTReviewList); got != 2 {
			t.Fatalf("review list reads = %d: the pre-invalidation fetch was stored", got)
		}
	})
}

// TestReadCacheSweepKeepsFingerprintedEntries pins the kind-aware sweep: a
// fingerprinted entry outlives the plain TTL until reviewCommentsMaxAge, while
// the sweep still evicts expired plain entries.
func TestReadCacheSweepKeepsFingerprintedEntries(t *testing.T) {
	clock := newFakeClock()
	c := newReadCache(defaultReadCacheTTL)
	c.now = clock.Now
	c.store("reviews|o/r#7", 0, clock.Now(), "kept", "fp")
	for i := 0; i < readCacheSweepAt; i++ {
		c.store(fmt.Sprintf("status|o/r#%d", i), 0, clock.Now(), "expired", "")
	}
	clock.Advance(2 * defaultReadCacheTTL)
	c.store("status|o/r#999", 0, clock.Now(), "trigger", "")
	if v, ok := c.matching("reviews|o/r#7", "fp"); !ok || v != "kept" {
		t.Fatalf("fingerprinted entry after a sweep = %v, %v; want it kept", v, ok)
	}
	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	if size != 2 {
		t.Fatalf("entries after a sweep = %d, want the fingerprinted entry and the trigger", size)
	}
	clock.Advance(reviewCommentsMaxAge)
	for i := 0; i < readCacheSweepAt; i++ {
		c.store(fmt.Sprintf("checks|o/r#%d", i), 0, clock.Now(), "fresh", "")
	}
	if _, ok := c.matching("reviews|o/r#7", "fp"); ok {
		t.Fatal("fingerprinted entry survived past reviewCommentsMaxAge")
	}
	c.mu.Lock()
	_, present := c.entries["reviews|o/r#7"]
	c.mu.Unlock()
	if present {
		t.Fatal("sweep kept a fingerprinted entry past reviewCommentsMaxAge")
	}
}
