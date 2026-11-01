package vcs

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/kit/clocktest"
)

type resetErr struct{ at time.Time }

func (e resetErr) Error() string        { return "quota exhausted" }
func (e resetErr) Is(target error) bool { return target == ErrRateLimited }
func (e resetErr) ResetAt() time.Time   { return e.at }

func TestIsRateLimited(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},
		{"sentinel wrapped", fmt.Errorf("read: %w", ErrRateLimited), true},
		{"provider error", resetErr{}, true},
		{"grpc resource exhausted", grpcstatus.Error(codes.ResourceExhausted, "quota"), true},
		{"grpc resource exhausted wrapped", fmt.Errorf("host: %w", grpcstatus.Error(codes.ResourceExhausted, "quota")), true},
		{"grpc other code", grpcstatus.Error(codes.Unavailable, "down"), false},
	} {
		if got := IsRateLimited(tc.err); got != tc.want {
			t.Errorf("%s: IsRateLimited = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRateLimitLogRepeatsUntilTheKnownReset(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	l := RateLimitLog{Now: clock.Now}
	err := fmt.Errorf("get PR: %w", resetErr{at: clock.Now().Add(20 * time.Minute)})

	if l.Repeat("site", err) {
		t.Fatal("first rate-limited error reported as a repeat")
	}
	clock.Advance(19 * time.Minute)
	if !l.Repeat("site", err) {
		t.Fatal("second error inside the window not reported as a repeat")
	}
	if l.Repeat("other site", err) {
		t.Fatal("a different site shares the first site's window")
	}
	clock.Advance(time.Minute)
	if l.Repeat("site", err) {
		t.Fatal("error after the window ended reported as a repeat")
	}
}

func TestRateLimitLogFallsBackToFiveMinutes(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	l := RateLimitLog{Now: clock.Now}
	for name, err := range map[string]error{
		"no reset":   ErrRateLimited,
		"past reset": resetErr{at: clock.Now().Add(-time.Minute)},
		"grpc":       grpcstatus.Error(codes.ResourceExhausted, "quota"),
	} {
		site := "site " + name
		if l.Repeat(site, err) {
			t.Fatalf("%s: first error reported as a repeat", name)
		}
		clock.Advance(5*time.Minute - time.Second)
		if !l.Repeat(site, err) {
			t.Fatalf("%s: error inside the 5-minute window not a repeat", name)
		}
		clock.Advance(time.Second)
		if l.Repeat(site, err) {
			t.Fatalf("%s: error after the 5-minute window reported as a repeat", name)
		}
	}
}

func TestRateLimitLogIgnoresOtherErrors(t *testing.T) {
	var l RateLimitLog // zero value is usable and uses the wall clock
	for i := 0; i < 3; i++ {
		if l.Repeat("site", errors.New("network down")) {
			t.Fatal("non-rate-limited error reported as a repeat")
		}
	}
	if l.Repeat("site", ErrRateLimited) {
		t.Fatal("a non-rate-limited error opened a window")
	}
}

func TestRateLimitLogLevel(t *testing.T) {
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	l := RateLimitLog{Now: clock.Now}
	if got := l.Level("site", errors.New("boom")); got != zerolog.WarnLevel {
		t.Fatalf("level for an ordinary error = %v, want warn", got)
	}
	if got := l.Level("site", ErrRateLimited); got != zerolog.WarnLevel {
		t.Fatalf("level for the first refusal = %v, want warn", got)
	}
	if got := l.Level("site", ErrRateLimited); got != zerolog.DebugLevel {
		t.Fatalf("level for a repeat = %v, want debug", got)
	}
}
