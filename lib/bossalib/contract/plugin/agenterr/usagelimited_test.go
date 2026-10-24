package agenterr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestErrUsageLimitedError(t *testing.T) {
	reset := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		err     ErrUsageLimited
		wantSub string
	}{
		{
			name:    "with reset time embeds it",
			err:     ErrUsageLimited{ResetAt: reset},
			wantSub: reset.Format(time.RFC3339),
		},
		{
			name:    "zero reset omits reset clause",
			err:     ErrUsageLimited{},
			wantSub: "usage limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got == "" {
				t.Fatal("Error() is empty")
			}
			if !contains(got, tt.wantSub) {
				t.Errorf("Error() = %q, want substring %q", got, tt.wantSub)
			}
		})
	}
	// Zero-reset message must NOT claim a reset time.
	if contains(ErrUsageLimited{}.Error(), "resets at") {
		t.Errorf("zero-reset Error() should not mention a reset time: %q", ErrUsageLimited{}.Error())
	}
}

func TestErrUsageLimitedErrorsAs(t *testing.T) {
	reset := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	// Wrap through fmt.Errorf to prove errors.As unwraps to the value type.
	wrapped := fmt.Errorf("run failed: %w", ErrUsageLimited{ResetAt: reset})

	var ul ErrUsageLimited
	if !errors.As(wrapped, &ul) {
		t.Fatal("errors.As did not extract ErrUsageLimited from a wrapped error")
	}
	if !ul.ResetAt.Equal(reset) {
		t.Errorf("extracted ResetAt = %v, want %v", ul.ResetAt, reset)
	}

	// A non-usage-limited error must not match.
	var ul2 ErrUsageLimited
	if errors.As(errors.New("some other failure"), &ul2) {
		t.Error("errors.As matched ErrUsageLimited on an unrelated error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return sub == ""
}
