package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
)

const (
	burstLogLine  = "github per-minute rate limit hit; pausing calls briefly"
	hourlyLogLine = "pausing calls until the quota resets"
)

func noJitter(time.Duration) time.Duration { return 0 }

// perMinuteGH refuses GraphQL `pr view` and `pr list` with GitHub's per-minute
// wording while refusing is set, answers REST calls, and reports graphQL
// remaining of 5000 from rate_limit with the reset 40 minutes away.
func perMinuteGH(clock *clocktest.Clock, remaining int, refusing *atomic.Bool) *scriptedGH {
	return &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBodyWithLimit(clock, 5000, remaining, 40*time.Minute), nil
		case isRESTCall(args):
			return `[]`, nil
		case refusing.Load():
			return "", errors.New(graphQLExhausted)
		case isPRView(args):
			return draftPRView, nil
		case isOpenPRList(args):
			return emptySnapshot, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
}

func refusingFlag() *atomic.Bool {
	var b atomic.Bool
	b.Store(true)
	return &b
}

func assertResetAt(t *testing.T, err error, want time.Time) {
	t.Helper()
	got, ok := vcs.RateLimitResetAt(err)
	if !ok || !got.Equal(want) {
		t.Fatalf("RateLimitResetAt(%v) = %v, %v; want %v", err, got, ok, want)
	}
}

func TestPerMinuteRefusalPausesBrieflyNotUntilReset_ScheduledJobs_AC230401(t *testing.T) {
	clock := newFakeClock()
	refusing := refusingFlag()
	gh := perMinuteGH(clock, 4699, refusing)
	var logs bytes.Buffer
	p := New(zerolog.New(&logs), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))
	ctx := context.Background()
	start := clock.Now()

	_, err := primaryPRRead(p, ctx, "o/r", 7)
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "per-minute rate limit") || strings.Contains(msg, "quota exhausted") {
		t.Fatalf("refused call text = %q, want the per-minute limit named and no exhausted quota", msg)
	}
	assertResetAt(t, err, start.Add(rateLimitBurstPause))

	clock.Advance(rateLimitBurstPause - time.Second)
	_, err = primaryPRRead(p, ctx, "o/r", 7)
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("call inside the burst pause err = %v, want vcs.ErrRateLimited", err)
	}
	assertResetAt(t, err, start.Add(rateLimitBurstPause))
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times inside the burst pause, want 1", got)
	}
	if _, err := p.GetReviewObservation(ctx, "o/r", 7); err != nil {
		t.Fatalf("REST call during a GraphQL burst pause: %v", err)
	}

	refusing.Store(false)
	clock.Advance(time.Second)
	if _, err := primaryPRRead(p, ctx, "o/r", 7); err != nil {
		t.Fatalf("call after the burst pause: %v", err)
	}
	if got := countWarnsWith(logs.String(), burstLogLine); got != 1 {
		t.Fatalf("burst Warn logged %d times, want 1:\n%s", got, logs.String())
	}
	if got := countWarnsWith(logs.String(), hourlyLogLine); got != 0 {
		t.Fatalf("hourly Warn logged %d times for a per-minute refusal, want 0:\n%s", got, logs.String())
	}
}

// The reading is cached before the refusal, so a refusal that wrote an
// inferred zero into it (the replaced behaviour) would fail both checks.
func TestPerMinuteRefusalLeavesQuotaStatusAtGitHubsFigures_ScheduledJobs_AC230401(t *testing.T) {
	clock := newFakeClock()
	refusing := refusingFlag()
	gh := perMinuteGH(clock, 4699, refusing)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter),
		WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor))
	ctx := context.Background()

	if _, err := p.QuotaStatus(ctx); err != nil {
		t.Fatalf("QuotaStatus before the refusal: %v", err)
	}
	clock.Advance(5 * time.Second)
	if _, err := primaryPRRead(p, ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	quotas, err := p.QuotaStatus(ctx)
	if err != nil {
		t.Fatalf("QuotaStatus after the refusal: %v", err)
	}
	var graphql vcs.Quota
	for _, q := range quotas {
		if q.Name == quotaGraphQL {
			graphql = q
		}
	}
	if graphql.Remaining != 4699 || graphql.Limit != 5000 {
		t.Fatalf("QuotaStatus graphql = %d/%d after a per-minute refusal, want GitHub's 4699/5000", graphql.Remaining, graphql.Limit)
	}

	refusing.Store(false)
	clock.Advance(rateLimitBurstPause)
	if _, err := primaryPRRead(p, vcs.WithCachedReads(ctx), "o/r", 7); err != nil {
		t.Fatalf("background read after the burst pause: %v (the floor must read GitHub's figures)", err)
	}
}

func TestRepeatedBurstRefusalsBackOffToTheFallbackCap(t *testing.T) {
	clock := newFakeClock()
	refusing := refusingFlag()
	gh := perMinuteGH(clock, 4699, refusing)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))
	ctx := context.Background()

	refuse := func(want time.Duration) {
		t.Helper()
		now := clock.Now()
		_, err := primaryPRRead(p, ctx, "o/r", 7)
		assertResetAt(t, err, now.Add(want))
		clock.Advance(want)
	}
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, rateLimitFallbackPause, rateLimitFallbackPause} {
		refuse(want)
	}
	if got := gh.count(isPRView); got != 5 {
		t.Fatalf("gh pr view ran %d times, want 5 (one per ended pause)", got)
	}

	refusing.Store(false)
	if _, err := primaryPRRead(p, ctx, "o/r", 7); err != nil {
		t.Fatalf("call after the capped pause: %v", err)
	}
	refusing.Store(true)
	refuse(rateLimitBurstPause)
}

func TestBurstPauseJitterStaysUnderTheCap(t *testing.T) {
	if got := burstPause(1, 10*time.Second); got != rateLimitBurstPause+10*time.Second {
		t.Fatalf("first burst pause with 10s jitter = %v, want %v", got, rateLimitBurstPause+10*time.Second)
	}
	if got := burstPause(4, 14*time.Second); got != rateLimitFallbackPause {
		t.Fatalf("capped burst pause with jitter = %v, want %v", got, rateLimitFallbackPause)
	}
	if got := defaultJitter(rateLimitBurstJitter); got < 0 || got >= rateLimitBurstJitter {
		t.Fatalf("defaultJitter = %v, want within [0, %v)", got, rateLimitBurstJitter)
	}
}

func TestSecondaryRefusalBurstPausesWithoutProbing(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			t.Error("probed rate_limit for a secondary rate limit")
		}
		return "", errors.New("gh pr view: HTTP 403: You have exceeded a secondary rate limit")
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))
	start := clock.Now()

	_, err := primaryPRRead(p, context.Background(), "o/r", 7)
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("secondary refusal err = %v, want vcs.ErrRateLimited", err)
	}
	assertResetAt(t, err, start.Add(rateLimitBurstPause))
	clock.Advance(30 * time.Second)
	if _, err := primaryPRRead(p, context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("call inside the secondary burst pause err = %v, want vcs.ErrRateLimited", err)
	}
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times inside the burst pause, want 1", got)
	}
}

func TestHourlyExhaustionStillPausesUntilReset(t *testing.T) {
	clock := newFakeClock()
	gh := perMinuteGH(clock, 0, refusingFlag())
	var logs bytes.Buffer
	p := New(zerolog.New(&logs), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter))
	ctx := context.Background()
	reset := time.Unix(clock.Now().Add(40*time.Minute).Unix(), 0)

	_, err := primaryPRRead(p, ctx, "o/r", 7)
	assertResetAt(t, err, reset)
	if isGitHubTransient(err) {
		t.Fatalf("hourly exhaustion %v is transient; it lasts until the reset", err)
	}
	clock.Advance(39 * time.Minute)
	_, err = primaryPRRead(p, ctx, "o/r", 7)
	assertResetAt(t, err, reset)
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times before the reset, want 1", got)
	}
	quotas, err := p.QuotaStatus(ctx)
	if err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	if got := quotas[len(quotas)-1]; got.Name != quotaGraphQL || got.Remaining != 0 {
		t.Fatalf("QuotaStatus graphql = %+v, want GitHub's remaining 0", got)
	}
	if got := countWarnsWith(logs.String(), hourlyLogLine); got != 1 {
		t.Fatalf("hourly Warn logged %d times, want 1:\n%s", got, logs.String())
	}
	if got := countWarnsWith(logs.String(), burstLogLine); got != 0 {
		t.Fatalf("burst Warn logged %d times for a spent quota, want 0:\n%s", got, logs.String())
	}
}

func TestBurstRefusalIsTransientForTheRetryLadder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining int
		wantErr   bool
		wantLists int
		wantSleep int
	}{
		// 30s lands inside the 60s burst pause and is refused without GitHub;
		// 30s+60s is past it and reaches GitHub.
		{name: "burst retries past the pause", remaining: 4699, wantLists: 2, wantSleep: 2},
		{name: "hourly stops at once", remaining: 0, wantErr: true, wantLists: 1, wantSleep: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			refusing := refusingFlag()
			gh := perMinuteGH(clock, tc.remaining, refusing)
			sleeps := 0
			p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithJitterFunc(noJitter),
				WithSleepFunc(func(d time.Duration) {
					sleeps++
					refusing.Store(false)
					clock.Advance(d)
				}))

			_, err := p.ListOpenPRs(context.Background(), "o/r")
			if (err != nil) != tc.wantErr {
				t.Fatalf("ListOpenPRs err = %v, want error %v", err, tc.wantErr)
			}
			isList := isOpenPRList
			if got := gh.count(isList); got != tc.wantLists || sleeps != tc.wantSleep {
				t.Fatalf("gh pr list ran %d times with %d sleeps, want %d and %d", got, sleeps, tc.wantLists, tc.wantSleep)
			}
		})
	}
}
