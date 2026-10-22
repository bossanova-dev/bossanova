package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// fakeClock is a settable clock for breaker and cache tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

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

func rateLimitBody(clock *fakeClock, graphQLRemaining int, resetIn time.Duration) string {
	return fmt.Sprintf(`{"resources":{"core":{"remaining":4999,"reset":%d},"graphql":{"remaining":%d,"reset":%d}}}`,
		clock.Now().Add(time.Hour).Unix(), graphQLRemaining, clock.Now().Add(resetIn).Unix())
}

// TestBreakerPausesExhaustedQuotaForEveryCaller is the core contract: once
// GitHub reports the GraphQL quota spent, every later GraphQL call fails fast
// with vcs.ErrRateLimited without contacting GitHub, until the reset GitHub
// reported — and then calls resume on their own.
func TestBreakerPausesExhaustedQuotaForEveryCaller(t *testing.T) {
	clock := newFakeClock()
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
	clock := newFakeClock()
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
	clock := newFakeClock()
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
	clock := newFakeClock()
	calls := 0
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			return rateLimitBody(clock, 5000, time.Hour), nil
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

// TestBreakerIgnoresAuthAndSecondaryLimits pins the two look-alikes that must
// not open the breaker: the anonymous limit (an auth failure) and a secondary
// rate limit (short, retried by the transient ladder).
func TestBreakerIgnoresAuthAndSecondaryLimits(t *testing.T) {
	for _, msg := range []string{
		"gh api: HTTP 403: API rate limit exceeded for 203.0.113.7. (But here's the good news: Authenticated requests get a higher rate limit.)",
		"gh pr view: HTTP 403: You have exceeded a secondary rate limit",
	} {
		t.Run(msg, func(t *testing.T) {
			gh := &scriptedGH{reply: func(args []string) (string, error) {
				if isRateLimitProbe(args) {
					t.Fatal("probed rate_limit for an error that is not a spent primary quota")
				}
				return "", errors.New(msg)
			}}
			p := New(zerolog.Nop(), WithRunGH(gh.run))
			_, err := p.GetPRStatus(context.Background(), "o/r", 7)
			if errors.Is(err, vcs.ErrRateLimited) {
				t.Fatalf("err = %v classified as an exhausted quota", err)
			}
		})
	}
}
