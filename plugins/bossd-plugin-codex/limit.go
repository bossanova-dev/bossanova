package main

import (
	"context"
	"regexp"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/statusdetect"
)

// codexLimitPatterns is the Codex-provider usage-cap banner grammar (D8).
// It is corpus-derived from lib/bossalib/agenterr/taxonomy.go's usagePatterns,
// narrowed to the phrasings Codex renders when a plan cap is hit: the machine
// token "usage_limit_reached", the periodic "weekly/daily/monthly limit" plan
// banners, the human "usage limit" phrasing, and credit exhaustion. All
// patterns are case-insensitive and fail-safe: the shared
// statusdetect.DetectUsageLimit anchoring matches them only inside the status
// region below the input box, so transcript prose quoting a limit message never
// trips them.
var codexLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage_limit_reached`),
	regexp.MustCompile(`(?i)usage limit`),
	regexp.MustCompile(`(?i)(?:daily|weekly|monthly) limit`),
	regexp.MustCompile(`(?i)\d+-hour limit`),
	regexp.MustCompile(`(?i)hit your .{0,40}limit`),
	regexp.MustCompile(`(?i)credits? exhausted`),
	regexp.MustCompile(`(?i)out of credits`),
}

// DetectUsageLimit reports whether the supplied tmux pane bytes show Codex's
// usage-cap banner in the CLI-owned status region below the bottom-most prompt
// marker, and (when the banner carries a parseable reset time) the resolved
// reset timestamp. Codex reuses the SHARED status-region anchoring and tail
// slicing in bossalib/statusdetect (identical to claude), contributing only
// codexLimitPatterns and injecting BOS-164's agenterr.ParseResetTime. reset_at
// is left absent when no reset time parses (limited-but-reset-unknown).
func (s *Server) DetectUsageLimit(_ context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) { //nolint:unparam // interface implementation
	limited, resetAt, hasReset := statusdetect.DetectUsageLimit(req.PaneContent, codexLimitPatterns, agenterr.ParseResetTime)
	resp := &bossanovav1.DetectUsageLimitResponse{Limited: limited}
	if hasReset {
		resp.ResetAt = timestamppb.New(resetAt)
	}
	return resp, nil
}
