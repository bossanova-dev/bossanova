package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossalib/kit/clocktest"

	"github.com/recurser/bossd/internal/ghusage"
)

func newFakeClock() *clocktest.Clock { return clocktest.New(time.Unix(1_800_000_000, 0)) }

// scriptedGH records every gh invocation and answers from handlers keyed by
// the first two args ("pr view", "api rate_limit", "api repos/..." → "api").
type scriptedGH struct {
	mu    sync.Mutex
	calls [][]string
	reply func(args []string) (string, error)
}

func (g *scriptedGH) run(_ context.Context, args ...string) (string, error) {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string(nil), args...))
	g.mu.Unlock()
	return g.reply(args)
}

func (g *scriptedGH) count(match func(args []string) bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if match(c) {
			n++
		}
	}
	return n
}

func isPRView(args []string) bool { return len(args) >= 2 && args[0] == "pr" && args[1] == "view" }
func isRateLimitProbe(args []string) bool {
	return len(args) >= 2 && args[0] == "api" && args[1] == "rate_limit"
}
func isRESTCall(args []string) bool {
	return len(args) >= 1 && args[0] == "api" && !isRateLimitProbe(args)
}

const graphQLExhausted = "gh pr view 7 --repo o/r: exit status 1: GraphQL: API rate limit already exceeded for user ID 96322."

const draftPRView = `{"state":"OPEN","isDraft":true,"title":"t","headRefName":"h","baseRefName":"main","headRefOid":"abc","mergeable":"MERGEABLE","mergeStateStatus":"DRAFT","reviewDecision":"","reviews":[]}`

func rateLimitBody(clock *clocktest.Clock, graphQLRemaining int, resetIn time.Duration) string {
	return rateLimitBodyWithLimit(clock, 0, graphQLRemaining, resetIn)
}

// rateLimitBodyWithLimit is rateLimitBody with a graphql limit; 0 omits it, as
// some responses do.
func rateLimitBodyWithLimit(clock *clocktest.Clock, graphQLLimit, graphQLRemaining int, resetIn time.Duration) string {
	limit := ""
	if graphQLLimit > 0 {
		limit = fmt.Sprintf(`"limit":%d,`, graphQLLimit)
	}
	return fmt.Sprintf(`{"resources":{"core":{"remaining":4999,"reset":%d},"graphql":{%s"remaining":%d,"reset":%d}}}`,
		clock.Now().Add(time.Hour).Unix(), limit, graphQLRemaining, clock.Now().Add(resetIn).Unix())
}

// TestBreakerPausesExhaustedQuotaForEveryCaller is the core contract: once
// GitHub reports the GraphQL quota spent, every later GraphQL call fails fast
// with vcs.ErrRateLimited without contacting GitHub, until the reset GitHub
// reported — and then calls resume on their own.
func TestBreakerPausesExhaustedQuotaForEveryCaller(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	exhausted := true
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 0, 20*time.Minute), nil
		case isPRView(args) && exhausted:
			return "", errors.New(graphQLExhausted)
		case isPRView(args):
			return draftPRView, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("first call err = %v, want vcs.ErrRateLimited", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := p.GetPRStatus(context.Background(), "o/r", 7+i); !errors.Is(err, vcs.ErrRateLimited) {
			t.Fatalf("paused call err = %v, want vcs.ErrRateLimited", err)
		}
	}
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times while the quota was paused, want 1", got)
	}
	if got := gh.count(isRateLimitProbe); got != 1 {
		t.Fatalf("rate_limit probed %d times, want 1", got)
	}

	clock.Advance(19 * time.Minute)
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("call before reset err = %v, want vcs.ErrRateLimited", err)
	}

	exhausted = false
	clock.Advance(2 * time.Minute)
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); err != nil {
		t.Fatalf("call after reset: %v", err)
	}
	if got := gh.count(isPRView); got != 2 {
		t.Fatalf("gh pr view ran %d times, want 2 (one refused, one after reset)", got)
	}
}

// TestBreakerQuotasAreIndependent pins that a spent GraphQL quota does not
// stop REST calls, which spend the separate core quota.
func TestBreakerQuotasAreIndependent(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 0, 30*time.Minute), nil
		case isPRView(args):
			return "", errors.New(graphQLExhausted)
		case isRESTCall(args):
			return `[]`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("err = %v, want vcs.ErrRateLimited", err)
	}
	if _, err := p.GetReviewObservation(context.Background(), "o/r", 7); err != nil {
		t.Fatalf("REST call while GraphQL is paused: %v", err)
	}
}

// TestBreakerFallsBackWhenResetIsUnreadable pins that a failed probe still
// pauses — briefly — rather than leaving every caller to re-hit the quota.
func TestBreakerFallsBackWhenResetIsUnreadable(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			return "", errors.New("probe failed")
		}
		return "", errors.New(graphQLExhausted)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	_, _ = p.GetPRStatus(context.Background(), "o/r", 7)
	_, _ = p.GetPRStatus(context.Background(), "o/r", 7)
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times inside the fallback pause, want 1", got)
	}
	clock.Advance(rateLimitFallbackPause + time.Second)
	_, _ = p.GetPRStatus(context.Background(), "o/r", 7)
	if got := gh.count(isPRView); got != 2 {
		t.Fatalf("gh pr view ran %d times after the fallback pause, want 2", got)
	}
}

// TestBreakerDoesNotPauseWhenQuotaAlreadyReset covers the reset landing
// between the refused call and the probe: nothing should be paused.
func TestBreakerDoesNotPauseWhenQuotaAlreadyReset(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	calls := 0
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			return rateLimitBodyWithLimit(clock, 5000, 5000, time.Hour), nil
		}
		calls++
		if calls == 1 {
			return "", errors.New(graphQLExhausted)
		}
		return draftPRView, nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("err = %v, want the refused call to report vcs.ErrRateLimited", err)
	}
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); err != nil {
		t.Fatalf("second call: %v (the quota had headroom, nothing should pause)", err)
	}
}

// TestBreakerIgnoresAuthFailures pins the look-alike that must not open the
// breaker: the anonymous limit is an auth failure, not a refusal of the
// account. A secondary limit does open it; see
// TestSecondaryRefusalBurstPausesWithoutProbing.
func TestBreakerIgnoresAuthFailures(t *testing.T) {
	const msg = "gh api: HTTP 403: API rate limit exceeded for 203.0.113.7. (But here's the good news: Authenticated requests get a higher rate limit.)"
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			t.Fatal("probed rate_limit for an auth failure")
		}
		return "", errors.New(msg)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run))
	_, err := p.GetPRStatus(context.Background(), "o/r", 7)
	if errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("err = %v classified as a rate-limit refusal", err)
	}
}

func TestRateLimitResetAtWrapped(t *testing.T) {
	reset := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
	err := fmt.Errorf("mark PR ready: %w", &rateLimitedError{quota: quotaGraphQL, resetAt: reset})
	got, ok := vcs.RateLimitResetAt(err)
	if !ok || !got.Equal(reset) {
		t.Fatalf("reset = %v, %v; want %v", got, ok, reset)
	}
	for _, other := range []error{nil, errors.New("unrelated"), vcs.ErrRateLimited} {
		if _, ok := vcs.RateLimitResetAt(other); ok {
			t.Errorf("unexpected reset for %v", other)
		}
	}
}

// failOpenGH answers like GitHub in the fail-open shape: every GraphQL read is
// refused as exhausted while rate_limit reports headroom on both quotas.
func failOpenGH(clock *clocktest.Clock) *scriptedGH {
	return &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 4000, 40*time.Minute), nil
		case isPRView(args):
			return "", errors.New(graphQLExhausted)
		case isRESTCall(args):
			return `[]`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
}

// TestQuotaReadingIsReusedWithinTTL pins the probe budget: refusals inside one
// burst pause share one rate_limit reading, and the first refusal after it
// ends pays for exactly one more.
func TestQuotaReadingIsReusedWithinTTL(t *testing.T) {
	clock := newFakeClock()
	gh := failOpenGH(clock)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))

	for i := 0; i < 50; i++ {
		if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
			t.Fatalf("call %d err = %v, want vcs.ErrRateLimited", i, err)
		}
	}
	if got := gh.count(isRateLimitProbe); got > 1 {
		t.Fatalf("rate_limit probed %d times for 50 refusals in one window, want at most 1", got)
	}

	clock.Advance(quotaReadingTTL + time.Second)
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("call after TTL err = %v, want vcs.ErrRateLimited", err)
	}
	if got := gh.count(isRateLimitProbe); got != 2 {
		t.Fatalf("rate_limit probed %d times after the reading expired, want 2", got)
	}
}

// TestConcurrentRefusalsShareOneProbe pins that callers refused together, with
// no cached reading, wait for one probe instead of each running their own.
func TestConcurrentRefusalsShareOneProbe(t *testing.T) {
	const callers = 20
	clock := newFakeClock()
	var p *Provider
	var refused sync.WaitGroup
	var holdFirstProbe sync.Once
	refused.Add(callers)
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			// Hold the first probe until every caller has reached the probe
			// path, so an uncoalesced caller runs its own probe meanwhile.
			holdFirstProbe.Do(func() {
				deadline := time.Now().Add(10 * time.Second)
				for p.quotas.waitingCount() < callers && time.Now().Before(deadline) {
					runtime.Gosched()
				}
			})
			return rateLimitBody(clock, 0, 20*time.Minute), nil
		case isPRView(args):
			refused.Done()
			refused.Wait()
			return "", errors.New(graphQLExhausted)
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p = New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	var done sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		done.Go(func() { _, errs[i] = p.GetPRStatus(context.Background(), "o/r", 7) })
	}
	done.Wait()
	for i, err := range errs {
		if !errors.Is(err, vcs.ErrRateLimited) {
			t.Fatalf("caller %d err = %v, want vcs.ErrRateLimited", i, err)
		}
	}
	if got := gh.count(isRateLimitProbe); got != 1 {
		t.Fatalf("rate_limit probed %d times for %d concurrent refusals, want 1", got, callers)
	}
}

// TestRefusalNewerThanCachedReadingReprobes pins that a refusal is judged by
// a reading taken after the refused call: a cached headroom reading from
// before it is re-probed rather than trusted or zeroed, and the burst pause
// leaves the other quota working.
func TestRefusalNewerThanCachedReadingReprobes(t *testing.T) {
	clock := newFakeClock()
	gh := failOpenGH(clock)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))
	ctx := context.Background()

	if _, err := p.QuotaStatus(ctx); err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	clock.Advance(10 * time.Second)
	if _, err := p.GetPRStatus(ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	if got := gh.count(isRateLimitProbe); got != 2 {
		t.Fatalf("rate_limit probed %d times, want 2 (the cached reading predates the refusal)", got)
	}
	if _, err := p.GetReviewObservation(ctx, "o/r", 7); err != nil {
		t.Fatalf("REST call while GraphQL is paused: %v", err)
	}
}

// TestRefusalLeavesCachedReadingAtGitHubsFigures pins that a refusal never
// writes an inferred figure into the shared reading: both quotas keep what
// GitHub reported.
func TestRefusalLeavesCachedReadingAtGitHubsFigures(t *testing.T) {
	clock := newFakeClock()
	gh := failOpenGH(clock)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))
	ctx := context.Background()

	before, ok := p.readQuotas(ctx)
	if !ok {
		t.Fatal("no reading")
	}
	clock.Advance(10 * time.Second)
	_, _ = p.GetPRStatus(ctx, "o/r", 7)

	after, ok := p.readQuotas(ctx)
	if !ok {
		t.Fatal("no cached reading after the refusal")
	}
	for _, quota := range []string{quotaGraphQL, quotaCore} {
		got, _ := after.status(quota)
		want, _ := before.status(quota)
		// The re-probe's reset moves with the fake clock; the figures must not.
		if got.Remaining != want.Remaining || got.Limit != want.Limit {
			t.Fatalf("%s = %+v after a graphql refusal, want GitHub's %+v", quota, got, want)
		}
	}
}

// TestFailedProbeIsNotCached pins that a failed probe leaves no reading: once
// the fallback pause ends, the next refusal probes again.
func TestFailedProbeIsNotCached(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			return "", errors.New("probe failed")
		}
		return "", errors.New(graphQLExhausted)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	ctx := context.Background()

	_, _ = p.GetPRStatus(ctx, "o/r", 7)
	if _, ok := p.readQuotas(ctx); ok {
		t.Fatal("a failed probe left a cached reading")
	}
	probes := gh.count(isRateLimitProbe)
	clock.Advance(rateLimitFallbackPause + time.Second)
	_, _ = p.GetPRStatus(ctx, "o/r", 7)
	if got := gh.count(isRateLimitProbe); got != probes+1 {
		t.Fatalf("rate_limit probed %d times after the fallback pause, want %d", got, probes+1)
	}
}

// TestQuotaStatusReadsBothQuotasOncePerTTL pins the free read the cron quota
// floor relies on: limit and remaining for core and graphql, one probe per
// cache TTL however many callers ask, and a fresh probe once it lapses.
func TestQuotaStatusReadsBothQuotasOncePerTTL(t *testing.T) {
	clock := newFakeClock()
	reset := clock.Now().Add(time.Hour).Unix()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if !isRateLimitProbe(args) {
			return "", fmt.Errorf("unexpected gh call %v", args)
		}
		return fmt.Sprintf(`{"resources":{"core":{"limit":5000,"remaining":1200,"reset":%d},"graphql":{"limit":5000,"remaining":300,"reset":%d}}}`, reset, reset), nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	want := []vcs.Quota{
		{Name: "core", Limit: 5000, Remaining: 1200, ResetAt: time.Unix(reset, 0)},
		{Name: "graphql", Limit: 5000, Remaining: 300, ResetAt: time.Unix(reset, 0)},
	}
	for i := 0; i < 3; i++ {
		got, err := p.QuotaStatus(context.Background())
		if err != nil {
			t.Fatalf("QuotaStatus: %v", err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("QuotaStatus = %v, want %v", got, want)
		}
	}
	if n := gh.count(isRateLimitProbe); n != 1 {
		t.Fatalf("rate_limit probed %d times inside one TTL, want 1", n)
	}
	clock.Advance(quotaReadingTTL)
	if _, err := p.QuotaStatus(context.Background()); err != nil {
		t.Fatalf("QuotaStatus after TTL: %v", err)
	}
	if n := gh.count(isRateLimitProbe); n != 2 {
		t.Fatalf("rate_limit probed %d times after the TTL lapsed, want 2", n)
	}
}

// TestQuotaStatusFailuresAreNotCached pins that an unreadable answer reaches
// the caller as an error (so the floor fails open) and the next call retries,
// and that a quota absent from the response is omitted rather than invented.
func TestQuotaStatusFailuresAreNotCached(t *testing.T) {
	clock := newFakeClock()
	fail := true
	gh := &scriptedGH{reply: func([]string) (string, error) {
		if fail {
			return "", errors.New("gh: network unreachable")
		}
		return fmt.Sprintf(`{"resources":{"core":{"limit":5000,"remaining":10,"reset":%d}}}`, clock.Now().Add(time.Hour).Unix()), nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	if _, err := p.QuotaStatus(context.Background()); err == nil {
		t.Fatal("QuotaStatus succeeded on a failed probe")
	}
	fail = false
	got, err := p.QuotaStatus(context.Background())
	if err != nil {
		t.Fatalf("QuotaStatus after a failed probe: %v", err)
	}
	if len(got) != 1 || got[0].Name != "core" || got[0].Remaining != 10 {
		t.Fatalf("QuotaStatus = %v, want only the core quota", got)
	}
}

// TestGHMetersEverySentCall pins the usage ledger contract: every call gh
// sends to GitHub appends one record carrying the context's label and the
// call's quota, a call the breaker refuses appends a breaker skip instead, and
// the free rate_limit probe appends nothing.
func TestGHMetersEverySentCall(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 0, 20*time.Minute), nil
		case isPRView(args):
			return "", errors.New(graphQLExhausted)
		case isRESTCall(args):
			return `[]`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	ledger := ghusage.New(t.TempDir(), zerolog.Nop(), ghusage.WithNow(clock.Now))
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithUsageRecorder(ledger))

	finalizeCtx := ghusage.WithLabel(context.Background(), ghusage.LabelFinalize)
	if _, err := p.GetReviewObservation(finalizeCtx, "o/r", 7); err != nil {
		t.Fatalf("REST call: %v", err)
	}
	// The first GraphQL call is sent and spends the quota; the rest are
	// refused by the breaker without contacting GitHub.
	for i := 0; i < 3; i++ {
		if _, err := p.GetPRStatus(context.Background(), "o/r", 7+i); !errors.Is(err, vcs.ErrRateLimited) {
			t.Fatalf("GraphQL call %d err = %v, want vcs.ErrRateLimited", i, err)
		}
	}

	restCalls := gh.count(isRESTCall)
	if restCalls == 0 || gh.count(isPRView) != 1 || gh.count(isRateLimitProbe) != 1 {
		t.Fatalf("gh calls: rest=%d pr view=%d probe=%d, want rest>0, 1 pr view and 1 probe",
			restCalls, gh.count(isPRView), gh.count(isRateLimitProbe))
	}
	usage, err := ledger.Read(clock.Now(), time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []ghusage.Count{
		{Label: ghusage.LabelFinalize, Quota: ghusage.QuotaCore, Calls: int64(restCalls)},
		{Label: ghusage.LabelUnattributed, Quota: ghusage.QuotaGraphQL, Calls: 1, BreakerSkips: 2},
	}
	if restCalls == 1 {
		// Equal calls sort by skips: the refused GraphQL row comes first.
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(usage.Counts, want) {
		t.Fatalf("ledger = %+v, want %+v", usage.Counts, want)
	}
}
