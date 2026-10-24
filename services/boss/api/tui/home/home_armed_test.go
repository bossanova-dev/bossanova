package home

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/bossalib/displaystatus"
)

func TestHomeArmedVerifyFixture(t *testing.T) {
	w := fixtures.ArmedVerifyWorld()
	sess := w.Sessions[0]
	h := NewModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(Model)
	updated, _ = h.Update(SessionListMsg{
		Sessions:             w.Sessions,
		HeartbeatStatuses:    map[string]string{sess.GetId(): ui.StatusWaiting},
		DaemonWaitingReasons: map[string]string{sess.GetId(): fixtures.ArmedVerifyReason},
	})
	h = updated.(Model)
	home := stripANSI(h.View().Content)
	if !strings.Contains(home, sess.GetTitle()) {
		t.Fatalf("Home is missing the armed session row:\n%s", home)
	}
	if strings.Contains(home, fixtures.ArmedVerifyReason) {
		t.Fatalf("Home must keep hiding the routine wait (#2901):\n%s", home)
	}
	var row string
	for _, line := range strings.Split(home, "\n") {
		if strings.Contains(line, sess.GetTitle()) {
			row = line
		}
	}
	if !strings.Contains(row, displaystatus.BlockedLabel) || strings.Contains(row, displaystatus.WaitingLabel) {
		t.Fatalf("parked row reads %q, want %q and no %q", row, displaystatus.BlockedLabel, displaystatus.WaitingLabel)
	}
	if hint := "blocked: open checklist items"; !strings.Contains(home, hint) {
		t.Fatalf("Home is missing the park hint %q:\n%s", hint, home)
	}
}
