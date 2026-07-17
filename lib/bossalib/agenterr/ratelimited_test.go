package agenterr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestErrRateLimitedError(t *testing.T) {
	got := ErrRateLimited{}.Error()
	if got == "" {
		t.Fatal("Error() is empty")
	}
	// The message must contain "429" so it re-classifies as KindRateLimited via
	// Classify on the bossd round-trip.
	if !contains(got, "429") {
		t.Errorf("Error() = %q, want it to contain 429", got)
	}
}

// TestErrRateLimitedRoundTrips is the load-bearing contract: the string surfaced
// through exit_error must re-classify as KindRateLimited so the bossd intercept
// treats it as a rotation trigger.
func TestErrRateLimitedRoundTrips(t *testing.T) {
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	c := Classify(ErrRateLimited{}.Error(), now)
	if c.Kind != KindRateLimited {
		t.Fatalf("Classify(ErrRateLimited{}.Error()).Kind = %v, want KindRateLimited", c.Kind)
	}
	if c.ResetAt != nil {
		t.Errorf("ResetAt = %v, want nil (rate-limited carries no reset)", *c.ResetAt)
	}
}

func TestErrRateLimitedErrorsAs(t *testing.T) {
	// Wrap through fmt.Errorf to prove errors.As unwraps to the value type.
	wrapped := fmt.Errorf("run failed: %w", ErrRateLimited{})

	var rl ErrRateLimited
	if !errors.As(wrapped, &rl) {
		t.Fatal("errors.As did not extract ErrRateLimited from a wrapped error")
	}

	// A non-rate-limited error must not match.
	var rl2 ErrRateLimited
	if errors.As(errors.New("some other failure"), &rl2) {
		t.Error("errors.As matched ErrRateLimited on an unrelated error")
	}
}
