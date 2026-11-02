package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
)

// pacerTestWait bounds every wait on a goroutine the test started, so a
// pacer that never releases a read fails the test instead of hanging it.
const pacerTestWait = 10 * time.Second

// pacerTestRate is 60 points a minute: one slot a second and a bucket of 10.
const (
	pacerTestRate  = 60
	pacerTestBurst = 10
)

// pacedGH answers every call and records the fake time of each one it sends,
// the free rate_limit probe excluded. While refusing is set it refuses sent
// calls with GitHub's per-minute wording; rate_limit reports headroom, so a
// refusal opens a burst pause.
type pacedGH struct {
	clock    *clocktest.Clock
	sent     chan time.Time
	refusing atomic.Bool
}

func newPacedGH(clock *clocktest.Clock) *pacedGH {
	return &pacedGH{clock: clock, sent: make(chan time.Time, 256)}
}

func (g *pacedGH) run(_ context.Context, args ...string) (string, error) {
	if isRateLimitProbe(args) {
		return rateLimitBodyWithLimit(g.clock, 5000, 4699, 40*time.Minute), nil
	}
	g.sent <- g.clock.Now()
	if g.refusing.Load() {
		return "", errors.New(graphQLExhausted)
	}
	return "{}", nil
}

func (g *pacedGH) recvSend(t *testing.T) time.Time {
	t.Helper()
	select {
	case at := <-g.sent:
		return at
	case <-time.After(pacerTestWait):
		t.Fatal("no gh call was sent")
		return time.Time{}
	}
}

func (g *pacedGH) assertNoSend(t *testing.T) {
	t.Helper()
	select {
	case at := <-g.sent:
		t.Fatalf("a gh call was sent at %v, want none", at)
	default:
	}
}

func newPacedProvider(gh *pacedGH, logger zerolog.Logger, opts ...ProviderOption) *Provider {
	return New(logger, append([]ProviderOption{
		WithRunGH(gh.run), WithNowFunc(gh.clock.Now), WithAfterFunc(gh.clock.After), WithJitterFunc(noJitter),
		WithCallPacing(CallPacing{CorePointsPerMinute: pacerTestRate, GraphQLPointsPerMinute: pacerTestRate}),
	}, opts...)...)
}

// prViewArgs is a GraphQL read; graphQLMutationArgs is a GraphQL write.
func prViewArgs(n int) []string { return []string{"pr", "view", strconv.Itoa(n), "--repo", "o/r"} }

var graphQLMutationArgs = []string{"api", "graphql", "-f", "query=mutation { addComment }"}

func backgroundCtx() context.Context { return vcs.WithCachedReads(context.Background()) }

// goGH runs one gh call on its own goroutine and delivers its error.
func goGH(ctx context.Context, p *Provider, args []string, errs chan<- error) {
	go func() {
		_, err := p.gh(ctx, args...)
		errs <- err
	}()
}

// ghBounded runs one gh call that must not park, failing the test instead of
// hanging when it does.
func ghBounded(t *testing.T, ctx context.Context, p *Provider, args []string) error {
	t.Helper()
	errs := make(chan error, 1)
	goGH(ctx, p, args, errs)
	return recvErr(t, errs)
}

func recvErr(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-time.After(pacerTestWait):
		t.Fatal("a parked gh call never returned")
		return nil
	}
}

func blockUntilTimers(t *testing.T, clock *clocktest.Clock, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pacerTestWait)
	defer cancel()
	if err := clock.BlockUntil(ctx, n); err != nil {
		t.Fatalf("waiting for %d parked reads: %v (pending %v)", n, err, clock.PendingDurations())
	}
}

// drainBucket spends the whole bucket with background reads, all sent at once.
func drainBucket(t *testing.T, p *Provider, gh *pacedGH) {
	t.Helper()
	for i := range pacerTestBurst {
		if _, err := p.gh(backgroundCtx(), prViewArgs(i)...); err != nil {
			t.Fatalf("background read %d inside the bucket: %v", i, err)
		}
		gh.recvSend(t)
	}
}

func assertWaits(t *testing.T, clock *clocktest.Clock, want ...time.Duration) {
	t.Helper()
	got := clock.Waits()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pacer timers = %v, want %v", got, want)
	}
}

func TestPacerSpreadsBackgroundBurstAcrossTheMinute(t *testing.T) {
	clock := newFakeClock()
	gh := newPacedGH(clock)
	p := newPacedProvider(gh, zerolog.Nop())
	t0 := clock.Now()
	const reads = 70

	errs := make(chan error, reads)
	for i := range reads {
		goGH(backgroundCtx(), p, prViewArgs(i), errs)
	}
	blockUntilTimers(t, clock, reads-pacerTestBurst)
	var sends []time.Time
	for range pacerTestBurst {
		sends = append(sends, gh.recvSend(t))
	}
	gh.assertNoSend(t)
	for range reads - pacerTestBurst {
		clock.Advance(time.Second)
		sends = append(sends, gh.recvSend(t))
	}
	for range reads {
		if err := recvErr(t, errs); err != nil {
			t.Fatalf("paced background read: %v", err)
		}
	}

	for n, at := range sends {
		slot := t0.Add(time.Duration(max(0, n+1-pacerTestBurst)) * time.Second)
		if at.Before(slot) {
			t.Fatalf("send %d at %v, before its slot %v", n+1, at.Sub(t0), slot.Sub(t0))
		}
		if n < pacerTestBurst && !at.Equal(t0) {
			t.Fatalf("burst send %d at %v, want t0", n+1, at.Sub(t0))
		}
	}
	if last := sends[len(sends)-1]; last.After(t0.Add(time.Minute)) {
		t.Fatalf("last send at %v, want all %d sent within the minute", last.Sub(t0), reads)
	}
}

func TestPacerNeverDelaysForegroundOrWrites(t *testing.T) {
	clock := newFakeClock()
	gh := newPacedGH(clock)
	p := newPacedProvider(gh, zerolog.Nop())
	t0 := clock.Now()
	drainBucket(t, p, gh)

	if _, err := p.gh(context.Background(), prViewArgs(100)...); err != nil {
		t.Fatalf("foreground read: %v", err)
	}
	if at := gh.recvSend(t); !at.Equal(t0) {
		t.Fatalf("foreground read sent at %v, want t0", at.Sub(t0))
	}
	if _, err := p.gh(backgroundCtx(), graphQLMutationArgs...); err != nil {
		t.Fatalf("background-context write: %v", err)
	}
	if at := gh.recvSend(t); !at.Equal(t0) {
		t.Fatalf("write sent at %v, want t0", at.Sub(t0))
	}
	assertWaits(t, clock)

	// Alone, the next background read would wait one slot. The foreground
	// read (1 point) and the write (writePointCost) took theirs first.
	errs := make(chan error, 1)
	goGH(backgroundCtx(), p, prViewArgs(101), errs)
	blockUntilTimers(t, clock, 1)
	want := time.Duration(1+1+writePointCost) * time.Second
	assertWaits(t, clock, want)
	clock.Advance(want)
	if at := gh.recvSend(t); !at.Equal(t0.Add(want)) {
		t.Fatalf("queued background read sent at %v, want %v", at.Sub(t0), want)
	}
	if err := recvErr(t, errs); err != nil {
		t.Fatalf("queued background read: %v", err)
	}
}

// pacerUsage counts metered calls and skips.
type pacerUsage struct {
	mu    sync.Mutex
	calls int
	skips int
}

func (u *pacerUsage) Record(string, string, string) { u.mu.Lock(); u.calls++; u.mu.Unlock() }

func (u *pacerUsage) RecordSkip(string, string, string, string) {
	u.mu.Lock()
	u.skips++
	u.mu.Unlock()
}

func (u *pacerUsage) skipCount() int { u.mu.Lock(); defer u.mu.Unlock(); return u.skips }

func (u *pacerUsage) count() int { u.mu.Lock(); defer u.mu.Unlock(); return u.calls }

func TestPacerRefusesBackgroundReadPastMaxWaitWithoutPausing(t *testing.T) {
	clock := newFakeClock()
	gh := newPacedGH(clock)
	usage := &pacerUsage{}
	var logs bytes.Buffer
	p := newPacedProvider(gh, zerolog.New(&logs), WithUsageRecorder(usage))
	t0 := clock.Now()
	const writes = 15 // 75 points against a bucket of 10: 65 in debt.
	for range writes {
		if _, err := p.gh(context.Background(), graphQLMutationArgs...); err != nil {
			t.Fatalf("foreground write: %v", err)
		}
		gh.recvSend(t)
	}
	metered := usage.count()

	for range 2 {
		err := ghBounded(t, backgroundCtx(), p, prViewArgs(1))
		var limited *rateLimitedError
		if !errors.Is(err, vcs.ErrRateLimited) || !errors.As(err, &limited) || !limited.paced {
			t.Fatalf("background read past the max wait err = %v, want a paced vcs.ErrRateLimited", err)
		}
		assertResetAt(t, err, t0.Add(time.Duration(writes*writePointCost-pacerTestBurst+1)*time.Second))
		if msg := err.Error(); !strings.Contains(msg, "paced below the per-minute limit") || strings.Contains(msg, "quota exhausted") {
			t.Fatalf("paced refusal text = %q, want the pacing named and no exhausted quota", msg)
		}
		if isGitHubTransient(err) {
			t.Fatalf("paced refusal %v is transient, want the retry ladder to leave it", err)
		}
	}
	gh.assertNoSend(t)
	assertWaits(t, clock)
	if got := usage.count(); got != metered {
		t.Fatalf("metered %d calls after the refusals, want %d", got, metered)
	}
	if got := usage.skipCount(); got != 0 {
		t.Fatalf("metered %d skips for paced refusals, want 0 (pacing is neither a breaker nor a floor skip)", got)
	}
	if got := strings.Count(logs.String(), "refusing background reads"); got != 1 {
		t.Fatalf("pacing refusal logged %d times, want 1:\n%s", got, logs.String())
	}
	if w, paused := p.limits.pausedUntil(quotaPool{identity: userIdentity.key, quota: quotaGraphQL}); paused {
		t.Fatalf("a paced refusal paused the breaker until %v", w.until)
	}
	if _, err := p.gh(context.Background(), prViewArgs(2)...); err != nil {
		t.Fatalf("foreground read after a paced refusal: %v", err)
	}
	gh.recvSend(t)
}

func TestPacerHonoursContextDeadline(t *testing.T) {
	clock := newFakeClock()
	gh := newPacedGH(clock)
	p := newPacedProvider(gh, zerolog.Nop())
	t0 := clock.Now()
	drainBucket(t, p, gh)

	deadlineCtx, cancelDeadline := context.WithDeadline(backgroundCtx(), clock.Now().Add(500*time.Millisecond))
	defer cancelDeadline()
	if err := ghBounded(t, deadlineCtx, p, prViewArgs(20)); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("read whose deadline ends before its slot err = %v, want vcs.ErrRateLimited", err)
	}
	gh.assertNoSend(t)
	assertWaits(t, clock)

	// The refused read returned its slot, so the next read gets the first one.
	cancelCtx, cancelRead := context.WithCancel(backgroundCtx())
	errs := make(chan error, 2)
	goGH(cancelCtx, p, prViewArgs(21), errs)
	blockUntilTimers(t, clock, 1)
	assertWaits(t, clock, time.Second)
	cancelRead()
	if err := recvErr(t, errs); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancelled mid-wait err = %v, want context.Canceled", err)
	}
	gh.assertNoSend(t)

	// The cancelled read returned its slot too.
	goGH(backgroundCtx(), p, prViewArgs(22), errs)
	waitCtx, cancelWait := context.WithTimeout(context.Background(), pacerTestWait)
	defer cancelWait()
	if err := clock.WaitUntil(waitCtx, func() bool { return len(clock.Waits()) == 2 }); err != nil {
		t.Fatalf("waiting for the next read to park: %v", err)
	}
	assertWaits(t, clock, time.Second, time.Second)
	clock.Advance(time.Second)
	if at := gh.recvSend(t); !at.Equal(t0.Add(time.Second)) {
		t.Fatalf("next read sent at %v, want 1s", at.Sub(t0))
	}
	if err := recvErr(t, errs); err != nil {
		t.Fatalf("next read: %v", err)
	}
}

func TestBurstRefusalStillPausesPacedCalls(t *testing.T) {
	clock := newFakeClock()
	gh := newPacedGH(clock)
	p := newPacedProvider(gh, zerolog.Nop())
	t0 := clock.Now()
	drainBucket(t, p, gh)
	const parked = 3
	errs := make(chan error, parked)
	for i := range parked {
		goGH(backgroundCtx(), p, prViewArgs(30+i), errs)
	}
	blockUntilTimers(t, clock, parked)

	gh.refusing.Store(true)
	_, err := p.gh(context.Background(), prViewArgs(40)...)
	if !errors.Is(err, vcs.ErrRateLimited) || !isBurstRefusal(err) {
		t.Fatalf("per-minute refusal err = %v, want a burst vcs.ErrRateLimited", err)
	}
	gh.recvSend(t)
	pauseEnd := t0.Add(rateLimitBurstPause)
	assertResetAt(t, err, pauseEnd)

	clock.Advance(parked * time.Second)
	for range parked {
		if err := recvErr(t, errs); !errors.Is(err, vcs.ErrRateLimited) || !isBurstRefusal(err) {
			t.Fatalf("parked read woken into the burst pause err = %v, want the breaker's burst refusal", err)
		}
	}
	if _, err := p.gh(context.Background(), prViewArgs(41)...); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("foreground call during the burst pause err = %v, want vcs.ErrRateLimited", err)
	}
	gh.assertNoSend(t)

	gh.refusing.Store(false)
	clock.Set(pauseEnd)
	if _, err := p.gh(context.Background(), prViewArgs(42)...); err != nil {
		t.Fatalf("foreground call after the pause: %v", err)
	}
	gh.recvSend(t)
	if _, err := p.gh(backgroundCtx(), prViewArgs(43)...); err != nil {
		t.Fatalf("background read after the pause: %v", err)
	}
	gh.recvSend(t)
}

func TestPacedRefusalIsNotTransient(t *testing.T) {
	resetAt := newFakeClock().Now().Add(time.Minute)
	paced := &rateLimitedError{quota: quotaGraphQL, resetAt: resetAt, paced: true}
	for _, err := range []error{paced, fmt.Errorf("get pr: %w", paced)} {
		if isGitHubTransient(err) {
			t.Errorf("isGitHubTransient(%v) = true, want a paced refusal left to the caller", err)
		}
	}
	burst := &rateLimitedError{quota: quotaGraphQL, resetAt: resetAt, burst: true}
	if !isGitHubTransient(burst) {
		t.Errorf("isGitHubTransient(%v) = false, want a burst refusal waited out", burst)
	}
}

func TestProviderWithoutCallPacingNeverWaits(t *testing.T) {
	clock := newFakeClock()
	gh := newPacedGH(clock)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithAfterFunc(clock.After))
	t0 := clock.Now()
	for i := range 100 {
		if _, err := p.gh(backgroundCtx(), prViewArgs(i)...); err != nil {
			t.Fatalf("background read %d: %v", i, err)
		}
		if at := gh.recvSend(t); !at.Equal(t0) {
			t.Fatalf("background read %d sent at %v, want t0", i, at.Sub(t0))
		}
	}
	assertWaits(t, clock)
}

// TestPacerBucketsArePerIdentity drives the user's GraphQL bucket deep into
// debt with foreground writes and checks a background read on an
// installation's pool still takes a slot at once, while one on the user's
// pool is refused: GitHub limits each identity separately.
func TestPacerBucketsArePerIdentity(t *testing.T) {
	clock := newFakeClock()
	p := newPacedProvider(newPacedGH(clock), zerolog.Nop())
	user := quotaPool{identity: userIdentity.key, quota: quotaGraphQL}
	installation := quotaPool{identity: "installation:42", quota: quotaGraphQL}
	for range 15 { // 75 points against a bucket of 10: 65 in debt.
		if _, err := p.admit(context.Background(), user, graphQLMutationArgs); err != nil {
			t.Fatalf("foreground write admit: %v", err)
		}
	}

	waited, err := p.admit(backgroundCtx(), installation, prViewArgs(1))
	if err != nil || waited {
		t.Fatalf("installation background read admit = (waited %v, %v), want an immediate slot", waited, err)
	}
	_, err = p.admit(backgroundCtx(), user, prViewArgs(1))
	var limited *rateLimitedError
	if !errors.As(err, &limited) || !limited.paced || limited.identity != user.identity {
		t.Fatalf("user background read admit err = %v, want a paced refusal naming the user pool", err)
	}
}
