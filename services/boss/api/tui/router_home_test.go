package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeHomeCloudAccessClient struct {
	status           *pb.CloudAccessStatus
	statusErr        error
	checkoutURL      string
	checkoutErr      error
	portalURL        string
	portalErr        error
	refresh          *pb.CloudAccessStatus
	refreshErr       error
	returnURLs       []string
	cancelURLs       []string
	portalReturnURLs []string
	statusCalls      int
	checkouts        int
	portals          int
	refreshes        int
}

func TestDeferredArchive_RowRendersFromArchivePendingAndStopsWhenCleared(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.Sessions = []*pb.Session{archivePendingSession("s1", true)}
	a.home.MarkArchiving("s1")
	a.home.BuildTableRows()

	model, _ := a.Update(chatpicker.ArchiveResultMsg{SessionID: "s1", Deferred: true})
	got := model.(App)
	if got.home.IsArchiving("s1") || got.home.ArchiveInFlight("s1") {
		t.Fatal("a deferred archive must drop the local archiving override")
	}
	if !strings.Contains(got.home.Table.Rows()[0][4], "archiving") {
		t.Fatalf("row status = %q, want archiving from archive_pending", got.home.Table.Rows()[0][4])
	}

	model, _ = got.Update(sessionListMsg{Sessions: []*pb.Session{archivePendingSession("s1", false)}})
	got = model.(App)
	if row := got.home.Table.Rows()[0][4]; strings.Contains(row, "archiving") {
		t.Fatalf("row status = %q after archive_pending cleared, want the normal status", row)
	}
	if !strings.Contains(got.home.Table.Rows()[0][4], "working") {
		t.Fatalf("row status = %q, want working", got.home.Table.Rows()[0][4])
	}
}

func TestHomeHiddenEndpointRowsFromSnapshot(t *testing.T) {
	previous := NewHomeModel(nil, context.Background(), nil)
	previous.Settings.DisplayOpenPorts = true
	previous.Sessions = []*pb.Session{endpointSession("first", 3000, 5173), endpointSession("second", 8080)}
	app := App{ctx: context.Background(), home: previous, userSettings: config.Settings{DisplayOpenPorts: false}}
	h := app.newHomeModel()
	h.Loading = false
	h.RepoCount = 1
	h.BuildTableRows()
	if got := len(h.Table.Rows()); got != 2 {
		t.Fatalf("rows = %d, want one per session", got)
	}
	for i, sess := range h.Sessions {
		if got := h.SubRowCount(sess); got != 0 {
			t.Fatalf("subrows = %d, want 0", got)
		}
		if got := h.TableCursorForSessionIndex(i); got != i {
			t.Fatalf("session %d maps to row %d", i, got)
		}
	}
	if strings.Contains(h.View().Content, ":3000") {
		t.Fatal("stale endpoint snapshot rendered while hidden")
	}
}
