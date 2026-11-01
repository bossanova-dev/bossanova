package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
)

// lowHeadroomGH refuses GraphQL reads as exhausted until healthy is set, while
// rate_limit reports the given graphql limit and remaining.
func lowHeadroomGH(clock *clocktest.Clock, limit, remaining int, resetIn time.Duration, healthy *bool) *scriptedGH {
	return &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBodyWithLimit(clock, limit, remaining, resetIn), nil
		case isPRView(args) && !*healthy:
			return "", errors.New(graphQLExhausted)
		case isPRView(args):
			return draftPRView, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
}

// TestRefusalWithLowHeadroomProbePauses covers GitHub refusing a call while a
// fresh probe still claims headroom below half the limit: the quota pauses
// until the earlier of its reset and the fallback pause, without invoking gh,
// and one Warn records the disagreement.
func TestRefusalWithLowHeadroomProbePauses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     int
		remaining int
		resetIn   time.Duration
		pause     time.Duration
	}{
		{name: "reset after fallback", limit: 5000, remaining: 100, resetIn: 40 * time.Minute, pause: rateLimitFallbackPause},
		{name: "reset before fallback", limit: 5000, remaining: 2499, resetIn: 2 * time.Minute, pause: 2 * time.Minute},
		{name: "missing limit defaults to 5000", limit: 0, remaining: 2000, resetIn: time.Hour, pause: rateLimitFallbackPause},
		{name: "reset already past", limit: 5000, remaining: 1, resetIn: -time.Minute, pause: rateLimitFallbackPause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			healthy := false
			gh := lowHeadroomGH(clock, tc.limit, tc.remaining, tc.resetIn, &healthy)
			var logs bytes.Buffer
			p := New(zerolog.New(&logs), WithRunGH(gh.run), WithNowFunc(clock.Now))
			ctx := context.Background()

			if _, err := p.GetPRStatus(ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
				t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
			}
			healthy = true
			clock.Advance(tc.pause - time.Second)
			if _, err := p.GetPRStatus(ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
				t.Fatalf("call inside the pause err = %v, want vcs.ErrRateLimited", err)
			}
			if got := gh.count(isPRView); got != 1 {
				t.Fatalf("gh pr view ran %d times inside the pause, want 1", got)
			}
			clock.Advance(2 * time.Second)
			if _, err := p.GetPRStatus(ctx, "o/r", 7); err != nil {
				t.Fatalf("call after the pause: %v", err)
			}
			if got := countWarnsWith(logs.String(), `"remaining":`); got != 1 {
				t.Fatalf("headroom disagreement logged %d Warn lines, want 1:\n%s", got, logs.String())
			}
		})
	}
}

// TestRefusalWithHalfHeadroomDoesNotPause pins the boundary: a fresh probe at
// or above half the limit means the reset landed, so nothing pauses.
func TestRefusalWithHalfHeadroomDoesNotPause(t *testing.T) {
	clock := newFakeClock()
	healthy := false
	gh := lowHeadroomGH(clock, 5000, 2500, 40*time.Minute, &healthy)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))

	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	healthy = true
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); err != nil {
		t.Fatalf("second call: %v (half the limit remained, nothing should pause)", err)
	}
}

// TestRateLimitResetAtFromBreaker pins that callers find the window end
// through vcs.RateLimitResetAt on both the refused call and a skipped one.
func TestRateLimitResetAtFromBreaker(t *testing.T) {
	clock := newFakeClock()
	healthy := false
	gh := lowHeadroomGH(clock, 5000, 0, 20*time.Minute, &healthy)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	want := time.Unix(clock.Now().Add(20*time.Minute).Unix(), 0)

	_, refused := p.GetPRStatus(context.Background(), "o/r", 7)
	_, skipped := p.GetPRStatus(context.Background(), "o/r", 7)
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times, want 1 (the second call is skipped)", got)
	}
	for name, err := range map[string]error{"refused": refused, "skipped": skipped} {
		got, ok := vcs.RateLimitResetAt(err)
		if !ok || !got.Equal(want) {
			t.Errorf("%s: RateLimitResetAt = %v, %v; want %v", name, got, ok, want)
		}
	}
}

func countWarnsWith(logs, fragment string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"level":"warn"`) && strings.Contains(line, fragment) {
			n++
		}
	}
	return n
}
