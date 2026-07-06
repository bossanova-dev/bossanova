package main

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestDetectUsageLimitFiresOnRealPaneFixture asserts DetectUsageLimit reports
// limited=true and a resolved reset_at when the real-rendered Claude pane shows
// a usage-cap banner in the status region below the input box.
func TestDetectUsageLimitFiresOnRealPaneFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/limit.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := &Server{logger: zerolog.Nop()}
	resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: data})
	if err != nil {
		t.Fatalf("DetectUsageLimit: %v", err)
	}
	if !resp.GetLimited() {
		t.Fatalf("expected limited=true on real limit fixture")
	}
	if resp.GetResetAt() == nil {
		t.Errorf("expected reset_at set (banner carries \"resets in 5 hours\")")
	}
}

// TestDetectUsageLimitIgnoresProseAbovePrompt is the D13 anchoring criterion:
// the identical banner text appearing only as transcript prose ABOVE the
// bottom-most prompt marker must not trigger detection.
func TestDetectUsageLimitIgnoresProseAbovePrompt(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/limit_prose_above.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := &Server{logger: zerolog.Nop()}
	resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: data})
	if err != nil {
		t.Fatalf("DetectUsageLimit: %v", err)
	}
	if resp.GetLimited() {
		t.Errorf("expected limited=false when the banner is only prose above the prompt marker")
	}
}

// TestDetectUsageLimitIgnoresWorkingPane confirms an ordinary working pane with
// no status-region banner is not limited (fail-safe: ambiguity never limits).
func TestDetectUsageLimitIgnoresWorkingPane(t *testing.T) {
	pane := []byte("⏺ Reading files…\n\n⎿ Read config.go (42 lines)\n\n❯ \n  claude-opus-4 · ~/code/bossanova · ready\n")
	s := &Server{logger: zerolog.Nop()}
	resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: pane})
	if err != nil {
		t.Fatalf("DetectUsageLimit: %v", err)
	}
	if resp.GetLimited() {
		t.Errorf("expected limited=false on a plain working pane")
	}
}
