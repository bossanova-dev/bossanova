package tui

import (
	"testing"

	"github.com/recurser/boss/api/tui/chatpicker"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestChatPickerDeferredArchiveRoutesHome(t *testing.T) {
	t.Run("deferred success routes back to the list", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewChatPicker
		a.chatPicker = pickerSessionFixture("s1", &pb.Session{Id: "s1"})
		a.home.MarkArchiving("s1")

		model, _ := a.Update(chatpicker.ArchiveResultMsg{SessionID: "s1", Deferred: true})
		got := model.(App)
		if got.activeView != ui.ViewHome {
			t.Fatalf("activeView = %v after a deferred archive, want ViewHome", got.activeView)
		}
	})
}
