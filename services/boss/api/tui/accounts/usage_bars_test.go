package accounts

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestUsageBars_Accounts_AC26(t *testing.T) {
	now := time.Now()
	reset := timestamppb.New(now.Add(4 * time.Hour))
	u := &pb.UsageSnapshot{FetchedAt: timestamppb.New(now)}
	low := ansi.Strip(usageWindowBarCell(u, .06, reset, now, false))
	high := ansi.Strip(usageWindowBarCell(u, 1, reset, now, false))
	if !strings.Contains(low, "6%") || !strings.Contains(high, "100%") {
		t.Fatalf("cells lost their percent: %q, %q", low, high)
	}
	if strings.Index(low, "(") != strings.Index(high, "(") {
		t.Errorf("countdowns misaligned across rows:\n%s\n%s", low, high)
	}
	for _, u := range []*pb.UsageSnapshot{nil, {}, {FetchedAt: timestamppb.Now(), Status: "unsupported"}} {
		if got := usageWindowBarCell(u, .5, nil, time.Now(), false); got != "—" {
			t.Errorf("unavailable = %q", got)
		}
	}
}

func TestVisibleModelWindows(t *testing.T) {
	u := &pb.UsageSnapshot{ModelWindows: []*pb.UsageModelWindow{{ModelName: "Zed", Util: .2}, nil, {ModelName: "Hidden"}, {ModelName: "Fable", Util: .9}}}
	windows := visibleModelWindows(u)
	if len(windows) != 2 || windows[0].GetModelName() != "Fable" || windows[1].GetModelName() != "Zed" {
		t.Fatalf("windows = %v", windows)
	}
	if len(visibleModelWindows(nil)) != 0 {
		t.Fatal("nil snapshot has windows")
	}
}
