package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/ghusage"
)

// GitHubUsageReader counts the GitHub calls and refused attempts in the usage
// ledger over the window ending at now.
type GitHubUsageReader interface {
	Read(now time.Time, window time.Duration) (ghusage.Usage, error)
}

// GetGitHubUsage reports the per-label, per-quota, per-identity GitHub call and
// skip counts the usage ledger holds for the requested window, the last hour by
// default.
func (s *Server) GetGitHubUsage(_ context.Context, req *connect.Request[bossanovav1.GetGitHubUsageRequest]) (*connect.Response[bossanovav1.GetGitHubUsageResponse], error) {
	if s.gitHubUsage == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github usage ledger not configured"))
	}
	window, err := gitHubUsageWindow(req.Msg.GetWindowSeconds())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	usage, err := s.gitHubUsage.Read(time.Now(), window)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read github usage: %w", err))
	}
	resp := &bossanovav1.GetGitHubUsageResponse{
		Rows:            make([]*bossanovav1.GitHubUsageRow, 0, len(usage.Counts)),
		WindowStartedAt: timestamppb.New(usage.WindowStart),
		WindowEndedAt:   timestamppb.New(usage.WindowEnd),
	}
	for _, c := range usage.Counts {
		resp.Rows = append(resp.Rows, &bossanovav1.GitHubUsageRow{
			Label:            c.Label,
			Quota:            c.Quota,
			Identity:         c.Identity,
			CallCount:        c.Calls,
			NotModifiedCount: c.NotModified,
			BreakerSkipCount: c.BreakerSkips,
			FloorSkipCount:   c.FloorSkips,
		})
	}
	return connect.NewResponse(resp), nil
}

// gitHubUsageWindow resolves the requested window: zero means the default
// hour, and anything past what the ledger keeps is refused rather than
// silently answered from a truncated ledger.
func gitHubUsageWindow(seconds int64) (time.Duration, error) {
	maxSeconds := int64(ghusage.MaxWindow / time.Second)
	switch {
	case seconds == 0:
		return ghusage.DefaultWindow, nil
	case seconds < 0 || seconds > maxSeconds:
		return 0, fmt.Errorf("window_seconds must be between 1 and %d, got %d", maxSeconds, seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}
