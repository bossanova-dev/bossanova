package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
)

// capFixedNow anchors reset-time parsing deterministically: Wednesday.
var capFixedNow = time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)

// isolateCaptureDir redirects agenterr.CaptureBanner's appstate-resolved output
// (HOME on darwin, XDG_STATE_HOME on linux) into a throwaway dir so usage-cap
// tests never accumulate redacted banner snapshots in the developer's or CI's
// real state directory on every `go test`.
func isolateCaptureDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
}

// padTail grows a marker line to a realistic ~8KB PostExit tail by prefixing
// benign JSONL so the classifier sees the banner at the end of a large buffer,
// mirroring the real earlyOutputCap window.
func padTail(marker string) []byte {
	var b strings.Builder
	filler := `{"type":"item.completed","item":{"type":"assistant_message","text":"working on it"}}` + "\n"
	for b.Len() < 8*1024 {
		b.WriteString(filler)
	}
	b.WriteString(marker)
	return []byte(b.String())
}

func TestClassifyUsageCap(t *testing.T) {
	isolateCaptureDir(t)
	tests := []struct {
		name          string
		tail          []byte
		wantLimited   bool
		wantResetZero bool
	}{
		{
			name:          "usage cap with parseable reset",
			tail:          padTail("stream error: usage_limit_reached, resets at 15:00"),
			wantLimited:   true,
			wantResetZero: false,
		},
		{
			name:          "usage cap without parseable reset",
			tail:          padTail("stream error: usage_limit_reached, try again later"),
			wantLimited:   true,
			wantResetZero: true,
		},
		{
			name:        "auth tail is not usage-limited",
			tail:        padTail(`{"type":"turn.failed","error":{"message":"401 Unauthorized: Missing bearer or basic authentication"}}`),
			wantLimited: false,
		},
		{
			name:        "fail-safe: benign prose mentioning limit",
			tail:        padTail("we are approaching the token budget limit for this file, refactoring"),
			wantLimited: false,
		},
		{
			name:        "fail-safe: unrelated crash",
			tail:        padTail("panic: runtime error: index out of range [3] with length 2"),
			wantLimited: false,
		},
		{
			name:        "fail-safe: spinner noise",
			tail:        padTail("⠋ thinking… ⠙ thinking… ⠹ thinking…"),
			wantLimited: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyUsageCap(zerolog.Nop(), tt.tail, capFixedNow)
			if !tt.wantLimited {
				if err != nil {
					t.Fatalf("classifyUsageCap = %v, want nil", err)
				}
				return
			}
			var ul agenterr.ErrUsageLimited
			if !errors.As(err, &ul) {
				t.Fatalf("classifyUsageCap = %v, want ErrUsageLimited", err)
			}
			if tt.wantResetZero && !ul.ResetAt.IsZero() {
				t.Errorf("ResetAt = %v, want zero", ul.ResetAt)
			}
			if !tt.wantResetZero && ul.ResetAt.IsZero() {
				t.Errorf("ResetAt is zero, want a parsed reset time")
			}
		})
	}
}

// TestClassifyUsageCapResetMatchesTaxonomy asserts the ResetAt threaded onto the
// sentinel is exactly what agenterr.Classify produced (no drift in the plugin).
func TestClassifyUsageCapResetMatchesTaxonomy(t *testing.T) {
	isolateCaptureDir(t)
	tail := padTail("usage_limit_reached, resets at 15:00")
	err := classifyUsageCap(zerolog.Nop(), tail, capFixedNow)
	var ul agenterr.ErrUsageLimited
	if !errors.As(err, &ul) {
		t.Fatalf("want ErrUsageLimited, got %v", err)
	}
	c := agenterr.Classify(string(tail), capFixedNow)
	if c.ResetAt == nil {
		t.Fatal("taxonomy produced no ResetAt for a 15:00 reset")
	}
	if !ul.ResetAt.Equal(*c.ResetAt) {
		t.Errorf("sentinel ResetAt = %v, taxonomy = %v", ul.ResetAt, *c.ResetAt)
	}
}

// TestPostExitAuthWinsOverUsage is the end-to-end regression: a tail that
// carries BOTH an auth marker and a usage marker must map to ErrAuthRequired
// (auth checked first), never ErrUsageLimited.
func TestPostExitAuthWinsOverUsage(t *testing.T) {
	tail := padTail("usage_limit_reached; also 401 Unauthorized: Missing bearer or basic authentication")
	// Mirror the runner PostExit ordering.
	var got error
	if detectAuthFailure(tail) {
		got = ErrAuthRequired
	} else {
		got = classifyUsageCap(zerolog.Nop(), tail, capFixedNow)
	}
	if !errors.Is(got, ErrAuthRequired) {
		t.Fatalf("got %v, want ErrAuthRequired when both markers present", got)
	}
	var ul agenterr.ErrUsageLimited
	if errors.As(got, &ul) {
		t.Error("auth tail wrongly classified as ErrUsageLimited")
	}
}
