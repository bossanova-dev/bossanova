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

// TestDetectUsageLimitFiresOnLiveBannerRenderings asserts the real
// claudeLimitPatterns detect the two states a user actually gets stuck in with
// the current Claude Code CLI: the cap notice rendered inline above the input
// box, the interactive decision modal, and the modal when the banner text has
// scrolled off (only the "Stop and wait for limit to reset" option is visible).
func TestDetectUsageLimitFiresOnLiveBannerRenderings(t *testing.T) {
	for _, fixture := range []string{
		"testdata/panes/limit_inline_banner.txt",
		"testdata/panes/limit_decision_modal.txt",
		"testdata/panes/limit_modal_banner_scrolled_off.txt",
	} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			s := &Server{logger: zerolog.Nop()}
			resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: data})
			if err != nil {
				t.Fatalf("DetectUsageLimit: %v", err)
			}
			if !resp.GetLimited() {
				t.Fatalf("expected limited=true for %s", fixture)
			}
		})
	}
}

// TestDetectUsageLimitFiresOnAnchoredRateLimitBanner is the BOS-406 interactive
// criterion: a 429 / rate-limit banner rendered in the status region below the
// prompt marker trips detection, so a mid-stream rate limit reaching the tmux
// pane flips CHAT_STATUS_LIMITED.
func TestDetectUsageLimitFiresOnAnchoredRateLimitBanner(t *testing.T) {
	for _, banner := range []string{
		"API Error: Request rejected (429) · This request would exceed your account's rate limit. Please try again later.",
		"This request would exceed your account's rate limit",
	} {
		pane := []byte("⏺ working on the task…\n\n❯ \n  " + banner + "\n")
		s := &Server{logger: zerolog.Nop()}
		resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: pane})
		if err != nil {
			t.Fatalf("DetectUsageLimit: %v", err)
		}
		if !resp.GetLimited() {
			t.Errorf("expected limited=true for anchored rate-limit banner %q", banner)
		}
	}
}

// TestDetectUsageLimitIgnoresBareRateLimitProse confirms the interactive anchor
// is deliberately tight: a bare "rate limit" mention in pane prose (an agent
// discussing rate limits) must NOT trip CHAT_STATUS_LIMITED.
func TestDetectUsageLimitIgnoresBareRateLimitProse(t *testing.T) {
	pane := []byte("⏺ Let me add rate limit handling to the client…\n\n❯ \n  claude-opus-4 · ~/code/bossanova · ready\n")
	s := &Server{logger: zerolog.Nop()}
	resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: pane})
	if err != nil {
		t.Fatalf("DetectUsageLimit: %v", err)
	}
	if resp.GetLimited() {
		t.Errorf("expected limited=false for a bare 'rate limit' prose mention")
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
