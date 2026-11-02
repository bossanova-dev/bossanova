package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
)

// webhookHealthSwitch is a WebhookDeliveryHealth whose answer a test flips.
type webhookHealthSwitch struct {
	mu      sync.Mutex
	healthy bool
}

func (h *webhookHealthSwitch) WebhookDeliveryHealthy(string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

func (h *webhookHealthSwitch) set(healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.healthy = healthy
}

// freshnessGH answers the snapshot query and the per-PR, branch-head and
// review reads these tests make, recording each call's route and clock time.
type freshnessGH struct {
	*scriptedGH
	atMu sync.Mutex
	at   []string
}

func newFreshnessGH(t *testing.T, now func() time.Time, t0 time.Time) *freshnessGH {
	t.Helper()
	snapshot := readFixture(t, "open_pr_snapshot_three.json")
	g := &freshnessGH{}
	g.scriptedGH = &scriptedGH{reply: func(args []string) (string, error) {
		key := routeKey(args)
		g.atMu.Lock()
		g.at = append(g.at, fmt.Sprintf("t0+%s %s", now().Sub(t0), key))
		g.atMu.Unlock()
		switch {
		case isGraphQL(args):
			return snapshot, nil
		case key == "pr view 1" || key == "pr view 9":
			return draftPRView, nil
		case key == "api repos/o/r/git/ref/heads/main":
			return `{"object":{"sha":"tip"}}`, nil
		case strings.HasSuffix(key, "/reviews?per_page=100"):
			return `[]`, nil
		}
		return "", fmt.Errorf("unexpected gh call %q", key)
	}}
	return g
}

func (g *freshnessGH) calls(match func([]string) bool) int { return g.count(match) }

// log is every recorded call as "t0+<elapsed> <route>".
func (g *freshnessGH) log() []string {
	g.atMu.Lock()
	defer g.atMu.Unlock()
	return append([]string(nil), g.at...)
}

func isRoute(want string) func([]string) bool {
	return func(args []string) bool { return routeKey(args) == want }
}

// readAll makes every background read the snapshot serves.
func readAll(t *testing.T, p *Provider) {
	t.Helper()
	bg := vcs.WithCachedReads(context.Background())
	if _, err := p.GetPRStatus(bg, "o/r", 1); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := p.GetCheckSet(bg, "o/r", 2); err != nil {
		t.Fatalf("checks: %v", err)
	}
	if _, err := p.ListOpenPRs(bg, "o/r"); err != nil {
		t.Fatalf("list: %v", err)
	}
}

func freshnessProvider(t *testing.T, health WebhookDeliveryHealth) (*Provider, *freshnessGH, func(time.Duration)) {
	t.Helper()
	clock := newFakeClock()
	t0 := clock.Now()
	gh := newFreshnessGH(t, clock.Now, t0)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	if health != nil {
		p.SetWebhookHealth(health)
	}
	return p, gh, clock.Advance
}

// TestSnapshotServesHealthyRepoForSafetyNetAge: with healthy webhooks the
// snapshot fetched at t0 serves every background read until t0+10m, and the
// first read at t0+10m refetches it.
func TestSnapshotServesHealthyRepoForSafetyNetAge(t *testing.T) {
	p, gh, advance := freshnessProvider(t, &webhookHealthSwitch{healthy: true})

	readAll(t, p)
	advance(snapshotSafetyNetMaxAge - time.Second)
	readAll(t, p)
	if got := gh.calls(isGraphQL); got != 1 {
		t.Fatalf("snapshot queries before t0+10m = %d, want 1 (calls %q)", got, gh.log())
	}
	advance(time.Second)
	readAll(t, p)
	if got := gh.calls(isGraphQL); got != 2 {
		t.Fatalf("snapshot queries at t0+10m = %d, want a refetch (calls %q)", got, gh.log())
	}
	if got := len(gh.log()); got != 2 {
		t.Fatalf("gh calls = %q, want only the two snapshot queries", gh.log())
	}
	t.Logf("recorded gh invocations: %q", gh.log())
}

// TestSnapshotFallsBackToFiveMinutesWithoutHealthyWebhooks covers a repo whose
// webhooks are unhealthy and a daemon with no health signal at all.
func TestSnapshotFallsBackToFiveMinutesWithoutHealthyWebhooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health WebhookDeliveryHealth
	}{
		{"unhealthy", &webhookHealthSwitch{}},
		{"unwired", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, gh, advance := freshnessProvider(t, tc.health)

			readAll(t, p)
			advance(snapshotFallbackMaxAge - time.Second)
			readAll(t, p)
			if got := gh.calls(isGraphQL); got != 1 {
				t.Fatalf("snapshot queries before t0+5m = %d, want 1", got)
			}
			advance(time.Second)
			readAll(t, p)
			if got := gh.calls(isGraphQL); got != 2 {
				t.Fatalf("snapshot queries at t0+5m = %d, want a refetch", got)
			}
		})
	}
}

// TestSnapshotHealthLapseTakesEffectOnNextRead: a snapshot stored while
// healthy is held to the fallback age once health lapses.
func TestSnapshotHealthLapseTakesEffectOnNextRead(t *testing.T) {
	health := &webhookHealthSwitch{healthy: true}
	p, gh, advance := freshnessProvider(t, health)

	readAll(t, p)
	advance(6 * time.Minute)
	health.set(false)
	readAll(t, p)
	if got := gh.calls(isGraphQL); got != 2 {
		t.Fatalf("snapshot queries after the lapse = %d, want a refetch", got)
	}
}

// TestSnapshotSurvivesTheSizeSweep: a store that sweeps a full cache of
// expired entries keeps a snapshot younger than the safety-net age.
func TestSnapshotSurvivesTheSizeSweep(t *testing.T) {
	p, gh, advance := freshnessProvider(t, &webhookHealthSwitch{healthy: true})

	readAll(t, p)
	advance(2 * time.Minute)
	stale := p.cache.now().Add(-2 * defaultReadCacheTTL)
	for i := 0; i <= readCacheSweepAt; i++ {
		key := prReadKey(readKindStatus, "o/other", i)
		gen, _ := p.cache.begin(key)
		p.cache.store(key, gen, stale, &vcs.PRStatus{}, "")
	}
	if n := len(p.cache.entries); n > readCacheSweepAt {
		t.Fatalf("cache holds %d entries after the sweep, want the expired ones evicted", n)
	}
	readAll(t, p)
	if got := gh.calls(isGraphQL); got != 1 {
		t.Fatalf("snapshot queries = %d: the sweep evicted a 2-minute-old snapshot", got)
	}
}

// TestSnapshotNeverStepsBackPastANewerPerPRRead: the snapshot outlives the
// per-PR TTL, so once a newer per-PR entry expires the older snapshot must not
// answer again; the background read repeats the per-PR read instead.
func TestSnapshotNeverStepsBackPastANewerPerPRRead(t *testing.T) {
	p, gh, advance := freshnessProvider(t, nil)
	bg := vcs.WithCachedReads(context.Background())

	if st, err := p.GetPRStatus(bg, "o/r", 1); err != nil || st.HeadSHA != "sha1" {
		t.Fatalf("snapshot read = %+v, %v; want head sha1", st, err)
	}
	advance(30 * time.Second)
	// An unmarked read (a post-action refresh) sees the PR's new head.
	if st, err := p.GetPRStatus(context.Background(), "o/r", 1); err != nil || st.HeadSHA != "abc" {
		t.Fatalf("unmarked read = %+v, %v; want head abc", st, err)
	}

	advance(30 * time.Second)
	if st, err := p.GetPRStatus(bg, "o/r", 1); err != nil || st.HeadSHA != "abc" {
		t.Fatalf("read within the TTL = %+v, %v; want the newer head abc", st, err)
	}

	advance(30 * time.Second) // the per-PR entry is past the TTL; the snapshot is 90s old
	st, err := p.GetPRStatus(bg, "o/r", 1)
	if err != nil {
		t.Fatalf("read past the TTL: %v", err)
	}
	if st.HeadSHA != "abc" {
		t.Fatalf("read past the TTL served head %q from the older snapshot, want abc (calls %q)", st.HeadSHA, gh.log())
	}
	if got := gh.calls(isGraphQL); got != 1 {
		t.Fatalf("snapshot queries = %d, want 1: the snapshot is still young (calls %q)", got, gh.log())
	}
	if got := gh.calls(isRoute("pr view 1")); got != 2 {
		t.Fatalf("per-PR reads = %d, want the expired entry re-read (calls %q)", got, gh.log())
	}
}

// TestInvalidateRepoMarksRepoReadsStale pins the repo-level webhook hook: the
// snapshot, list, per-PR status and branch head all refetch, in one snapshot
// query for the snapshot-served reads, while review comments stay cached.
func TestInvalidateRepoMarksRepoReadsStale(t *testing.T) {
	p, gh, _ := freshnessProvider(t, &webhookHealthSwitch{healthy: true})
	bg := vcs.WithCachedReads(context.Background())

	readAll(t, p)
	// PR 9 is absent from the snapshot, so its status is a per-PR entry.
	if _, err := p.GetPRStatus(bg, "o/r", 9); err != nil {
		t.Fatalf("status 9: %v", err)
	}
	if _, err := p.GetBranchHeadSHA(bg, "o/r", "main"); err != nil {
		t.Fatalf("branch head: %v", err)
	}
	if _, err := p.GetReviewComments(bg, "o/r", 1); err != nil {
		t.Fatalf("reviews: %v", err)
	}
	reviews := isRoute("api repos/o/r/pulls/1/reviews?per_page=100")
	before := map[string]int{
		"graphql": gh.calls(isGraphQL), "pr view 9": gh.calls(isRoute("pr view 9")),
		"head": gh.calls(isRoute("api repos/o/r/git/ref/heads/main")), "reviews": gh.calls(reviews),
	}

	p.InvalidateRepo("https://github.com/O/R")

	readAll(t, p)
	_, _ = p.GetPRStatus(bg, "o/r", 9)
	_, _ = p.GetBranchHeadSHA(bg, "o/r", "main")
	_, _ = p.GetReviewComments(bg, "o/r", 1)
	after := map[string]int{
		"graphql": gh.calls(isGraphQL), "pr view 9": gh.calls(isRoute("pr view 9")),
		"head": gh.calls(isRoute("api repos/o/r/git/ref/heads/main")), "reviews": gh.calls(reviews),
	}
	want := map[string]int{"graphql": 1, "pr view 9": 1, "head": 1, "reviews": 0}
	for k, n := range want {
		if got := after[k] - before[k]; got != n {
			t.Errorf("%s calls after InvalidateRepo = %d, want %d (calls %q)", k, got, n, gh.log())
		}
	}
}

// TestInvalidateRepoBeatsInFlightSnapshot: a snapshot fetch blocked when
// InvalidateRepo runs never stores, so the following read refetches.
func TestInvalidateRepoBeatsInFlightSnapshot(t *testing.T) {
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
	p.SetWebhookHealth(&webhookHealthSwitch{healthy: true})
	bg := vcs.WithCachedReads(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.GetPRStatus(bg, "o/r", 1)
	}()
	<-started
	p.InvalidateRepo("o/r")
	close(release)
	<-done

	_, _ = p.GetPRStatus(bg, "o/r", 2)
	if got := gh.count(isGraphQL); got != 2 {
		t.Fatalf("snapshot queries = %d: the pre-invalidation fetch was stored", got)
	}
}

// TestInvalidateRepoLeavesOtherReposAlone: only the named repo's reads go stale.
func TestInvalidateRepoLeavesOtherReposAlone(t *testing.T) {
	p, gh, _ := freshnessProvider(t, &webhookHealthSwitch{healthy: true})

	readAll(t, p)
	p.InvalidateRepo("o/r-other")
	readAll(t, p)
	if got := gh.calls(isGraphQL); got != 1 {
		t.Fatalf("snapshot queries = %d: another repo's invalidation dropped o/r", got)
	}
}

// TestUnmarkedReadsIgnoreAHealthySnapshot: reads without vcs.WithCachedReads
// still go to GitHub on a healthy repo with a fresh snapshot.
func TestUnmarkedReadsIgnoreAHealthySnapshot(t *testing.T) {
	p, gh, _ := freshnessProvider(t, &webhookHealthSwitch{healthy: true})

	readAll(t, p)
	s, err := p.GetPRStatus(context.Background(), "o/r", 1)
	if err != nil || s.Title != "t" {
		t.Fatalf("unmarked status = %+v, %v; want the live per-PR read", s, err)
	}
	if got := gh.calls(isRoute("pr view 1")); got != 1 {
		t.Fatalf("unmarked per-PR reads = %d, want 1", got)
	}
}

// dependabotPoll makes the reads one dependabot poll of repo o/r makes through
// the host: the open-PR list, then checks and status for every open PR, all as
// background reads. It returns the gh invocations the poll added.
func dependabotPoll(t *testing.T, p *Provider, gh *freshnessGH) []string {
	t.Helper()
	before := len(gh.log())
	bg := vcs.WithCachedReads(context.Background())
	prs, err := p.ListOpenPRs(bg, "o/r")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(prs) == 0 {
		t.Fatal("list returned no open PRs; the poll would read nothing per PR")
	}
	for _, pr := range prs {
		if _, err := p.GetCheckResults(bg, "o/r", pr.Number); err != nil {
			t.Fatalf("checks %d: %v", pr.Number, err)
		}
		if _, err := p.GetPRStatus(bg, "o/r", pr.Number); err != nil {
			t.Fatalf("status %d: %v", pr.Number, err)
		}
	}
	return gh.log()[before:]
}

// TestDependabotRepeatPollMakesNoPerPRCalls: the first dependabot poll of a
// repo costs one snapshot query, and a repeat poll two minutes later with no
// invalidation costs no gh call at all — no pr checks, pr view or REST pull.
func TestDependabotRepeatPollMakesNoPerPRCalls(t *testing.T) {
	p, gh, advance := freshnessProvider(t, nil)

	first := dependabotPoll(t, p, gh)
	t.Logf("poll 1 gh invocations: %q", first)
	if len(first) != 1 || !strings.HasSuffix(first[0], " graphql") {
		t.Fatalf("poll 1 gh invocations = %q, want exactly one graphql query", first)
	}

	advance(2 * time.Minute)
	second := dependabotPoll(t, p, gh)
	t.Logf("poll 2 gh invocations: %q", second)
	if len(second) != 0 {
		t.Fatalf("poll 2 gh invocations = %q, want none", second)
	}
}

// TestDependabotHourOfPollsStaysWithinSnapshotBudget: 30 polls at the 2-minute
// poll interval over one hour, with no webhook health signal, cost no more than
// 12 snapshot queries for a single-page repo and nothing else.
func TestDependabotHourOfPollsStaysWithinSnapshotBudget(t *testing.T) {
	p, gh, advance := freshnessProvider(t, nil)

	for i := range 30 {
		if i > 0 {
			advance(2 * time.Minute)
		}
		dependabotPoll(t, p, gh)
	}
	calls := gh.log()
	t.Logf("gh invocations over the hour: %q", calls)
	if snapshots := gh.calls(isGraphQL); snapshots > 12 || snapshots != len(calls) {
		t.Fatalf("gh invocations over the hour = %q, want at most 12, all snapshot queries", calls)
	}
}
