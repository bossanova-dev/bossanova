package status

import (
	"testing"
	"time"

	"github.com/recurser/bossd/internal/adapter/tmux"
)

func TestTmuxReaperKeepsServePane_ChatPicker_AC250701(t *testing.T) {
	fx := armedFixture(t, false)
	name := tmux.ServeSessionName(reaperRepoID, reaperSessionID)
	fx.addLivePane(name, time.Hour, ptr(reaperDaemonID))
	fx.sweep()
	fx.now = fx.now.Add(2 * time.Minute)
	fx.sweep()
	if len(fx.killed) != 0 {
		t.Fatalf("serve pane reaped: %v", fx.killed)
	}
}
