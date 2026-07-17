package main

import (
	"context"
	"regexp"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/statusdetect"
)

// claudeLimitPatterns is the Claude-provider usage-cap banner grammar (D8).
// It is corpus-derived from lib/bossalib/agenterr/taxonomy.go's usagePatterns,
// narrowed to the human-readable phrasings Claude Code renders in its status
// region when a plan/rate cap is hit ("usage limit", "5-hour limit", "hit your
// … limit", credit exhaustion). All patterns are case-insensitive and
// fail-safe: they match only inside the status region below the input box (the
// shared statusdetect.DetectUsageLimit anchoring), so transcript prose quoting a
// limit message never trips them.
var claudeLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage limit`),
	regexp.MustCompile(`(?i)usage_limit_reached`),
	regexp.MustCompile(`(?i)(?:daily|weekly|monthly) limit`),
	regexp.MustCompile(`(?i)\d+-hour limit`),
	regexp.MustCompile(`(?i)hit your .{0,40}limit`),
	regexp.MustCompile(`(?i)credits? exhausted`),
	regexp.MustCompile(`(?i)out of credits`),
	// Rate-limit (429) banner (BOS-406). Deliberately tightly anchored — NOT a
	// bare "rate limit", which would false-trigger on ordinary pane prose (an
	// agent discussing rate limits). Only the Claude 429 banner shape matches,
	// and only inside the status region below the input box.
	regexp.MustCompile(`(?i)request rejected \(429\)`),
	regexp.MustCompile(`(?i)would exceed your .*rate limit`),
	// The interactive usage-limit decision modal's first option. It lets the
	// modal be detected even when the "hit your … limit" banner above it has
	// scrolled off the visible pane; statusdetect only scans the whole screen
	// for these once it has confirmed the blocking modal is up (isDecisionModal),
	// so this cannot fire on ordinary prose.
	regexp.MustCompile(`(?i)stop and wait for .{0,30}limit`),
}

// DetectUsageLimit reports whether the supplied tmux pane bytes show Claude's
// usage-cap banner in the CLI-owned status region below the bottom-most prompt
// marker, and (when the banner carries a parseable reset time) the resolved
// reset timestamp. The shared status-region anchoring and tail slicing live in
// bossalib/statusdetect; this plugin contributes only claudeLimitPatterns and
// injects BOS-164's agenterr.ParseResetTime as the reset parser. reset_at is
// left absent when no reset time parses (limited-but-reset-unknown).
func (s *Server) DetectUsageLimit(_ context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) { //nolint:unparam // interface implementation
	limited, resetAt, hasReset := statusdetect.DetectUsageLimit(req.PaneContent, claudeLimitPatterns, agenterr.ParseResetTime)
	resp := &bossanovav1.DetectUsageLimitResponse{Limited: limited}
	if hasReset {
		resp.ResetAt = timestamppb.New(resetAt)
	}
	return resp, nil
}
