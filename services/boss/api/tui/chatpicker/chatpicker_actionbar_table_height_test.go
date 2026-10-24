package chatpicker

import (
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestChatPickerTableHeightReservesFoldedFooter(t *testing.T) {
	chats := make([]*pb.AgentChat, 100)
	for i := range chats {
		chats[i] = &pb.AgentChat{AgentSessionId: "stopped"}
	}
	picker := Model{
		chats:           chats,
		daemonStatuses:  map[string]string{"stopped": ui.StatusStopped},
		newTabSupported: true,
		sessionID:       "session",
		width:           72,
		height:          24,
	}
	left, middle, back := picker.chatListActionGroups()
	footerLines := ui.ActionBarLineCount(picker.width, left, middle, back)
	if footerLines < 2 {
		t.Fatalf("chat footer line count = %d, want folded action bar", footerLines)
	}

	want := ui.ClampedTableHeight(len(chats), picker.height,
		ui.BannerOverhead+1+ui.ActionBarPadY+footerLines)
	if got := picker.tableHeight(); got != want {
		t.Fatalf("tableHeight() = %d, want %d", got, want)
	}
}
