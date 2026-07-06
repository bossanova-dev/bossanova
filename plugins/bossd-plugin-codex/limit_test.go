package main

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestDetectUsageLimitFiresOnRealPaneFixture asserts the codex DetectUsageLimit
// reports limited=true and a resolved reset_at when the real-rendered pane shows
// a usage-cap banner in the status region below the "›" input box.
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
		t.Errorf("expected reset_at set (banner carries \"resets in 3 hours\")")
	}
}

// TestDetectUsageLimitIgnoresProseAbovePrompt is the D13 anchoring criterion:
// a usage_limit_reached banner appearing only as transcript prose ABOVE the
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

// TestDetectUsageLimitIgnoresQuestionPane confirms the codex question/approval
// pane fixture (no status-region limit banner) is not limited.
func TestDetectUsageLimitIgnoresQuestionPane(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Skip("no question.txt fixture; skipping negative assertion")
	}
	s := &Server{logger: zerolog.Nop()}
	resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: data})
	if err != nil {
		t.Fatalf("DetectUsageLimit: %v", err)
	}
	if resp.GetLimited() {
		t.Errorf("expected limited=false on a codex question pane")
	}
}
