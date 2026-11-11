package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
)

const (
	testStandardFloor = 0.35
	testReserveFloor  = 0.10
)

// floorGH answers rate_limit with a settable body, `pr view` with a draft PR
// and `pr edit` with success. Any other call (the open-PR snapshot) fails, so
// a background read falls through to its per-PR `pr view`.
type floorGH struct {
	*scriptedGH
	mu   sync.Mutex
	body string
}

func newFloorGH(body string) *floorGH {
	f := &floorGH{body: body}
	f.scriptedGH = &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.body, nil
		case isPRView(args):
			return draftPRView, nil
		case isPREdit(args):
			return "", nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	return f
}

func (f *floorGH) setBody(body string) {
	f.mu.Lock()
	f.body = body
	f.mu.Unlock()
}

func isPREdit(args []string) bool { return len(args) >= 2 && args[0] == "pr" && args[1] == "edit" }

// graphQLAt is a rate_limit body whose GraphQL quota has remaining of 5000
// left, resetting in resetIn.
func graphQLAt(clock *clocktest.Clock, remaining int, resetIn time.Duration) string {
	return rateLimitBodyWithLimit(clock, 5000, remaining, resetIn)
}

func newFloorProvider(gh *floorGH, clock *clocktest.Clock, opts ...ProviderOption) *Provider {
	opts = append([]ProviderOption{
		WithRunGH(gh.run), WithNowFunc(clock.Now),
		WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor),
	}, opts...)
	return New(zerolog.Nop(), opts...)
}

// TestBackgroundReadBelowStandardFloorIsRefused_Sessions_AC222201 is the
// core contract: a background read below the standard floor fails as rate
// limited without reaching GitHub, and callers find the window end through
// vcs.RateLimitResetAt.
func TestBackgroundReadBelowStandardFloorIsRefused_Sessions_AC222201(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(graphQLAt(clock, 1500, 20*time.Minute))
	p := newFloorProvider(gh, clock)

	_, err := primaryPRRead(p, vcs.WithCachedReads(context.Background()), "o/r", 7)
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("background read at 30%% err = %v, want vcs.ErrRateLimited", err)
	}
	if got := gh.count(isPRView); got != 0 {
		t.Fatalf("gh pr view ran %d times below the floor, want 0", got)
	}
	want := time.Unix(clock.Now().Add(20*time.Minute).Unix(), 0)
	if got, ok := vcs.RateLimitResetAt(err); !ok || !got.Equal(want) {
		t.Fatalf("RateLimitResetAt = %v, %v; want %v", got, ok, want)
	}
}

// TestForegroundReadsAndBackgroundWritesPassTheFloor_Sessions_AC222201 pins
// what the floor never holds back: an unmarked read, a background write, and
// an unmarked read straight after a refusal (the refusal pauses nothing).
func TestForegroundReadsAndBackgroundWritesPassTheFloor_Sessions_AC222201(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(graphQLAt(clock, 1500, 20*time.Minute))
	p := newFloorProvider(gh, clock)
	bg := vcs.WithCachedReads(context.Background())

	if _, err := primaryPRRead(p, bg, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("background read err = %v, want vcs.ErrRateLimited", err)
	}
	if _, err := primaryPRRead(p, context.Background(), "o/r", 7); err != nil {
		t.Fatalf("foreground read after a floor refusal: %v", err)
	}
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times, want 1 (the foreground read)", got)
	}
	if err := p.UpdatePRTitle(bg, "o/r", 7, "t"); err != nil {
		t.Fatalf("background title write below the floor: %v", err)
	}
	if got := gh.count(isPREdit); got != 1 {
		t.Fatalf("gh pr edit ran %d times, want 1", got)
	}
}

// TestReserveTierReadsDownToTheReserveFloor_Callbacks_AC222201 pins the two
// tiers: a reserve-tier read proceeds between the floors and is refused below
// the reserve floor.
func TestReserveTierReadsDownToTheReserveFloor_Callbacks_AC222201(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining int
		refused   bool
	}{
		{name: "30% remaining", remaining: 1500, refused: false},
		{name: "8% remaining", remaining: 400, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			gh := newFloorGH(graphQLAt(clock, tc.remaining, 20*time.Minute))
			p := newFloorProvider(gh, clock)

			_, err := primaryPRRead(p, vcs.WithReserveQuotaReads(context.Background()), "o/r", 7)
			if got := errors.Is(err, vcs.ErrRateLimited); got != tc.refused {
				t.Fatalf("reserve read err = %v, refused = %v; want refused = %v", err, got, tc.refused)
			}
			if wantViews := map[bool]int{true: 0, false: 1}[tc.refused]; gh.count(isPRView) != wantViews {
				t.Fatalf("gh pr view ran %d times, want %d", gh.count(isPRView), wantViews)
			}
		})
	}
}

// TestFloorFailsOpen pins the cases where the floor cannot judge, and so lets
// a background read through.
func TestFloorFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(*clocktest.Clock) string
		opts []ProviderOption
	}{
		{name: "unreadable rate_limit", body: func(*clocktest.Clock) string { return "not json" }},
		{name: "zero limit", body: func(c *clocktest.Clock) string { return rateLimitBodyWithLimit(c, 0, 100, time.Hour) }},
		{name: "zero floors", body: func(c *clocktest.Clock) string { return graphQLAt(c, 100, time.Hour) },
			opts: []ProviderOption{WithBackgroundQuotaFloors(0, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			gh := newFloorGH(tc.body(clock))
			p := newFloorProvider(gh, clock, tc.opts...)

			if _, err := primaryPRRead(p, vcs.WithCachedReads(context.Background()), "o/r", 7); err != nil {
				t.Fatalf("background read: %v", err)
			}
			if got := gh.count(isPRView); got != 1 {
				t.Fatalf("gh pr view ran %d times, want 1", got)
			}
		})
	}
}

// TestProviderWithoutFloorsNeverProbes pins that New without the option
// behaves as before: no rate_limit read on the background path.
func TestProviderWithoutFloorsNeverProbes(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(graphQLAt(clock, 100, time.Hour))
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	if _, err := primaryPRRead(p, vcs.WithCachedReads(context.Background()), "o/r", 7); err != nil {
		t.Fatalf("background read: %v", err)
	}
	if got := gh.count(isRateLimitProbe); got != 0 {
		t.Fatalf("rate_limit probed %d times without floors, want 0", got)
	}
}

// TestFloorRereadsAReadingWhoseResetPassed pins that a cached reading from the
// previous quota window is re-read before deciding: the new window's figures,
// not the stale ones, decide the read.
func TestFloorRereadsAReadingWhoseResetPassed(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(graphQLAt(clock, 4000, 30*time.Second))
	p := newFloorProvider(gh, clock)
	bg := vcs.WithCachedReads(context.Background())

	if _, err := primaryPRRead(p, bg, "o/r", 7); err != nil {
		t.Fatalf("read at 80%%: %v", err)
	}
	clock.Advance(40 * time.Second) // past the reset, inside quotaReadingTTL
	gh.setBody(graphQLAt(clock, 1500, time.Hour))
	if _, err := primaryPRRead(p, bg, "o/r", 8); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("read after the reset err = %v, want the new window's refusal", err)
	}
	if got := gh.count(isRateLimitProbe); got != 2 {
		t.Fatalf("rate_limit probed %d times, want 2 (the stale reading re-read once)", got)
	}
}

// TestConcurrentBackgroundReadsShareOneFloorProbe pins that background reads
// meeting a cold quota cache together share one rate_limit probe.
func TestConcurrentBackgroundReadsShareOneFloorProbe(t *testing.T) {
	const callers = 10
	clock := newFakeClock()
	var p *Provider
	var holdFirstProbe sync.Once
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			// Hold the first probe until every caller has reached the probe
			// path, so an uncoalesced caller runs its own probe meanwhile.
			holdFirstProbe.Do(func() {
				deadline := time.Now().Add(10 * time.Second)
				for p.quotas.waitingCount() < callers && time.Now().Before(deadline) {
					runtime.Gosched()
				}
			})
			return graphQLAt(clock, 1500, time.Hour), nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p = New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor))
	bg := vcs.WithCachedReads(context.Background())

	var done sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		// Distinct repositories, so the read cache cannot coalesce them first.
		done.Go(func() { _, errs[i] = primaryPRRead(p, bg, fmt.Sprintf("o/r%d", i), 7) })
	}
	done.Wait()
	for i, err := range errs {
		if !errors.Is(err, vcs.ErrRateLimited) {
			t.Fatalf("caller %d err = %v, want vcs.ErrRateLimited", i, err)
		}
	}
	if got := gh.count(isRateLimitProbe); got != 1 {
		t.Fatalf("rate_limit probed %d times for %d concurrent reads, want 1", got, callers)
	}
}

// TestFloorLogsTransitionsNotRefusals pins the floor's own log: one Info line
// when background reads of a quota start being refused and one when they
// resume, however many reads it refuses in between.
func TestFloorLogsTransitionsNotRefusals(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(graphQLAt(clock, 1500, 20*time.Minute))
	var logs bytes.Buffer
	p := New(zerolog.New(&logs), WithRunGH(gh.run), WithNowFunc(clock.Now),
		WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor))
	bg := vcs.WithCachedReads(context.Background())

	for i := range 3 {
		if _, err := primaryPRRead(p, bg, "o/r", 7+i); !errors.Is(err, vcs.ErrRateLimited) {
			t.Fatalf("read %d err = %v, want vcs.ErrRateLimited", i, err)
		}
	}
	clock.Advance(quotaReadingTTL)
	gh.setBody(graphQLAt(clock, 5000, time.Hour))
	if _, err := primaryPRRead(p, bg, "o/r", 7); err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	if got := countInfosWith(logs.String(), "below the background floor"); got != 1 {
		t.Fatalf("logged %d refusal transitions, want 1:\n%s", got, logs.String())
	}
	if got := countInfosWith(logs.String(), "back above the background floor"); got != 1 {
		t.Fatalf("logged %d resume transitions, want 1:\n%s", got, logs.String())
	}
}

func countInfosWith(logs, fragment string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"level":"info"`) && strings.Contains(line, fragment) {
			n++
		}
	}
	return n
}

func TestIsReadInvocation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"pr", "view", "7", "--repo", "o/r"}, true},
		{[]string{"pr", "checks", "7"}, true},
		{[]string{"pr", "list", "--repo", "o/r"}, true},
		{[]string{"pr", "diff", "7"}, true},
		{[]string{"run", "list", "--commit", "abc"}, true},
		{[]string{"api", "repos/o/r/pulls/7"}, true},
		{[]string{"api", "graphql", "-f", "query=query($o:String!) { viewer { login } }"}, true},
		{[]string{"api", "graphql", "-f", "query=  mutation { x }"}, false},
		{[]string{"api", "graphql", "-f", "owner=o"}, false},
		{[]string{"pr", "edit", "7", "--title", "t"}, false},
		{[]string{"pr", "ready", "7"}, false},
		{[]string{"pr", "merge", "7"}, false},
		{[]string{"pr", "create", "--title", "t"}, false},
		{[]string{"api", "-X", "POST", "repos/o/r/statuses/abc"}, false},
		{[]string{"api", "repos/o/r/issues/7/comments", "-f", "body=x"}, false},
		{[]string{"api", "repos/o/r", "--method=PATCH"}, false},
		{[]string{"api", "repos/o/r", "--input", "-"}, false},
		{[]string{"repo", "view"}, false},
		{[]string{"pr"}, false},
	} {
		if got := isReadInvocation(tc.args); got != tc.want {
			t.Errorf("isReadInvocation(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
