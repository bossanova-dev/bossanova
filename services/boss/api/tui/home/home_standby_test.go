package home

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/fixtures"
)

func TestHomeBuildHandoffStandbyFixture(t *testing.T) {
	w := fixtures.BuildHandoffStandbyWorld()
	sess := w.Sessions[0]
	h := NewModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(Model)
	for _, sessionReason := range []string{fixtures.BuildHandoffVerifyReason, fixtures.BuildHandoffStandbyReason} {
		updated, _ = h.Update(SessionListMsg{
			Sessions:             w.Sessions,
			HeartbeatStatuses:    map[string]string{sess.GetId(): ui.StatusWaiting},
			DaemonWaitingReasons: map[string]string{sess.GetId(): sessionReason},
		})
		h = updated.(Model)
		home := stripANSI(h.View().Content)
		if strings.Contains(home, "awaiting checks_failed") {
			t.Fatalf("Home shows the standby wait as the session hint:\n%s", home)
		}
	}
}
