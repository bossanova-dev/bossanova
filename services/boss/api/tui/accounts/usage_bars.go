package accounts

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// usageWindowBarCell renders a list usage cell. A muted cell (a disabled row)
// greys its text and uses the muted meter, which keeps the fill visible where
// muting the whole cell would flatten every meter into one empty-looking grey.
func usageWindowBarCell(u *pb.UsageSnapshot, fraction float64, reset *timestamppb.Timestamp, now time.Time, muted bool) string {
	pct, countdown, ok := accountUtilParts(u, fraction, reset, now)
	if !ok {
		return "—"
	}
	// Right-align the percent so the countdowns line up down the column.
	text := fmt.Sprintf(" %4s", pct)
	if countdown != "" {
		text += " (" + countdown + ")"
	}
	if muted {
		return ui.RenderMutedMeter(fraction, 5) + lipgloss.NewStyle().Foreground(ui.ColorMuted).Render(text)
	}
	return ui.RenderMeter(fraction, 5) + text
}

func visibleModelWindows(u *pb.UsageSnapshot) []*pb.UsageModelWindow {
	var windows []*pb.UsageModelWindow
	for _, window := range u.GetModelWindows() {
		if window.GetUtil() > 0 {
			windows = append(windows, window)
		}
	}
	slices.SortFunc(windows, func(a, b *pb.UsageModelWindow) int { return strings.Compare(a.GetModelName(), b.GetModelName()) })
	return windows
}

func accountSyncLabel(a *pb.AgentAccount) string {
	if a.GetSyncedAgentAccountId() != "" {
		return "synced"
	}
	return "local"
}
