package home

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestHomeShortSessionTitle_Sessions_AC151(t *testing.T) {
	id, url := "BOS-1", "https://linear.app/example/issue/BOS-1"
	title := "[BOS-1] Improve session titles for the home board and web tailword"
	sess := &pb.Session{Title: title, TrackerId: &id, TrackerUrl: &url}
	h := Model{layout: &homeTableLayout{project: func(row table.Row) table.Row { return row }}}
	for _, tt := range []struct {
		name     string
		selected bool
		status   pb.DisplayStatus
	}{
		{"normal", false, pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED},
		{"selected", true, pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED},
		{"merged", false, pb.DisplayStatus_DISPLAY_STATUS_MERGED},
		{"closed", false, pb.DisplayStatus_DISPLAY_STATUS_CLOSED},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sess.DisplayStatus = tt.status
			name := h.primaryRow(sess, 0, tt.selected)[2]
			if got, want := ansi.Strip(name), "BOS-1 · Improve session titles for the home…"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
			if !strings.Contains(name, "\x1b]8;;"+url) {
				t.Errorf("missing tracker hyperlink: %q", name)
			}
		})
	}
	if !strings.Contains(homeFilterHaystack(sess), "tailword") {
		t.Fatal("filter lost full title tail")
	}
	if sess.GetTitle() != title {
		t.Fatal("list rendering changed stored title")
	}
}
