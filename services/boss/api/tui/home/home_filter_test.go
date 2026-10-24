package home

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHomeFilterSlashOpensInput(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	h.Loading = false
	h.Sessions = []*pb.Session{{Id: "one", Title: "Alpha"}, {Id: "two", Title: "Beta"}}
	h.BuildTableRows()
	model, _ := h.Update(keyString("/"))
	h = model.(Model)
	if !h.TextEntryActive() || !strings.Contains(h.RenderSessionTable(), "type to search") {
		t.Fatal("slash did not open filter")
	}
	model, _ = h.Update(tea.KeyPressMsg{Code: 'B'})
	h = model.(Model)
	if h.SelectedSessionID() != "two" {
		t.Fatalf("selected %q, want two", h.SelectedSessionID())
	}
}

func filteredHome(t *testing.T) Model {
	t.Helper()
	h := NewModel(nil, context.Background(), nil)
	h.Loading = false
	h.Width, h.Height = 120, 30
	h.RepoCount = 1
	h.Sessions = []*pb.Session{
		{Id: "hidden", Title: "Hidden", RepoDisplayName: "other"},
		{Id: "review", Title: "Alpha review", RepoDisplayName: "app", BranchName: "feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Please check the diff"}},
		{Id: "progress", Title: "Alpha build", RepoDisplayName: "app", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}},
	}
	h.BuildTableRows()
	return h
}

func homeKey(h Model, msg tea.Msg) (Model, tea.Cmd) {
	model, cmd := h.Update(msg)
	return model.(Model), cmd
}

func TestHomeFilterFieldsAndBoardMapping(t *testing.T) {
	pr := int32(42)
	tracker := "ENG-7"
	s := &pb.Session{Id: "one", RepoDisplayName: "MyRepo", Title: "Title", BranchName: "Branch", TrackerId: &tracker, PrNumber: &pr, DisplayLabel: "Stopped"}
	h := NewModel(nil, context.Background(), nil)
	h.Sessions = []*pb.Session{s}
	if &h.BoardSessions()[0] != &h.Sessions[0] {
		t.Fatal("empty query must return original slice")
	}
	for _, q := range []string{"myrepo", "TITLE", "branch", "#42", "eng-7", "stopped"} {
		h.Filter.SeedApplied(q)
		if len(h.BoardSessions()) != 1 {
			t.Errorf("missing match %q", q)
		}
	}
	h = filteredHome(t)
	h.Filter.SeedApplied("alpha")
	h.BuildTableRows()
	if len(h.Sessions) != 3 || len(h.BoardSessions()) != 2 {
		t.Fatal("filter narrowed full state")
	}
	if h.TableDataRowCount() != len(h.Table.Rows()) {
		t.Fatal("row counts differ")
	}
	for i, row := range h.primarySessionRows() {
		h.Table.SetCursor(row)
		if got := h.SelectedSession(); got != h.BoardSessions()[i] {
			t.Fatalf("cursor %d selects %v", row, got)
		}
	}
	for _, want := range []string{"NEEDS YOU", "IN PROGRESS", "(2 of 3)"} {
		if !strings.Contains(h.RenderSessionTable(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if h.NeighborSessionID("review") != "progress" {
		t.Fatal("neighbor must be visible")
	}
}

func TestHomeFilterLifecycleAndPaste(t *testing.T) {
	h := filteredHome(t)
	h, _ = homeKey(h, keyString("/"))
	for _, letter := range "qnslr" {
		h, _ = homeKey(h, tea.KeyPressMsg{Code: letter})
	}
	if h.Filter.Query() != "qnslr" || !h.Filter.Active() {
		t.Fatal("action keys escaped input")
	}
	if strings.Contains(h.RenderSessionTable(), "[q]uit") {
		t.Fatal("typing advertises quit")
	}
	h, _ = homeKey(h, keyString("esc"))
	h, _ = homeKey(h, keyString("/"))
	h, _ = homeKey(h, tea.PasteMsg{Content: "Alpha"})
	if h.Filter.Query() != "Alpha" || h.SelectedSessionID() != "review" {
		t.Fatal("paste did not filter/reset cursor")
	}
	h, _ = homeKey(h, tea.KeyPressMsg{Code: tea.KeyDown})
	if h.SelectedSessionID() != "progress" {
		t.Fatal("down did not skip auxiliary/header rows")
	}
	h, _ = homeKey(h, keyString("enter"))
	if !h.Filter.Applied() || h.Filter.Active() {
		t.Fatal("enter did not commit")
	}
	_, cmd := homeKey(h, keyString("enter"))
	if cmd == nil {
		t.Fatal("second enter did not select")
	}
	if msg, ok := cmd().(ui.SwitchViewMsg); !ok || msg.View != ui.ViewChatPicker {
		t.Fatal("second enter did not open chat picker")
	}
	h, _ = homeKey(h, keyString("/"))
	if h.Filter.Query() != "Alpha" {
		t.Fatal("reopen lost query")
	}
	h, _ = homeKey(h, keyString("esc"))
	h, _ = homeKey(h, keyString("/"))
	h, _ = homeKey(h, keyString("enter"))
	if h.Filter.Engaged() {
		t.Fatal("empty commit stayed engaged")
	}
}

func TestHomeFilterNoMatchAndHiddenInputs(t *testing.T) {
	h := filteredHome(t)
	h.Filter.SeedApplied("absent")
	h.BuildTableRows()
	screen := h.RenderSessionTable()
	if !strings.Contains(screen, "No sessions match search.") || strings.Contains(screen, "[enter] select") || strings.Contains(screen, "no active sessions") {
		t.Fatalf("bad no-match view: %s", screen)
	}
	if h.SelectedSession() != nil {
		t.Fatal("no-match has selection")
	}
	for _, mode := range []string{"loading", "error", "empty"} {
		m := filteredHome(t)
		m.Filter.Activate()
		switch mode {
		case "loading":
			m.Loading = true
		case "error":
			m.Err = fmt.Errorf("offline")
		case "empty":
			m.Sessions = nil
		}
		m = m.dismissInputsIfHidden()
		if m.Filter.Engaged() {
			t.Errorf("%s stranded filter", mode)
		}
		m, _ = homeKey(m, keyString("/"))
		if m.Filter.Active() {
			t.Errorf("%s opened hidden filter", mode)
		}
	}
}

func TestHomeFilterReorderRefused(t *testing.T) {
	h := filteredHome(t)
	h.Filter.SeedApplied("alpha")
	h.BuildTableRows()
	before := h.Sessions[0]
	h, cmd := h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_UP)
	if cmd != nil || h.Sessions[0] != before || h.Status != "Clear the search to reorder sessions" || !h.StatusErr() {
		t.Fatal("filtered reorder was not refused")
	}
}

func TestHomeFilterPollKeepsFullState(t *testing.T) {
	h := filteredHome(t)
	h.Filter.SeedApplied("alpha")
	h.BuildTableRows()
	h.Table.SetCursor(h.primarySessionRows()[1])
	h.ArchivingOverrideIDs["hidden"] = struct{}{}
	h.ArchiveInFlightIDs["hidden"] = struct{}{}
	incoming := append([]*pb.Session(nil), h.Sessions...)
	model, cmd := h.ApplySessionList(SessionListMsg{Sessions: incoming})
	h = model.(Model)
	if cmd != nil {
		t.Fatal("unchanged hidden sessions notified again")
	}
	if h.SelectedSessionID() != "progress" || h.Filter.Query() != "alpha" {
		t.Fatal("poll lost filtered selection/query")
	}
	if _, ok := h.ArchivingOverrideIDs["hidden"]; !ok {
		t.Fatal("poll lost hidden archive override")
	}
	h.Filter.Activate()
	model, _ = h.ApplySessionList(SessionListMsg{})
	if model.(Model).Filter.Engaged() {
		t.Fatal("empty poll stranded filter")
	}
	h = filteredHome(t)
	h.Filter.Activate()
	for i := 0; i < pollFailureThreshold; i++ {
		model, _ = h.handleSessionListError(fmt.Errorf("offline"))
		h = model.(Model)
	}
	if h.Filter.Engaged() {
		t.Fatal("error stranded filter")
	}
}

func TestHomeFilterAuxiliaryRowsStayWithSession(t *testing.T) {
	h := filteredHome(t)
	h.Settings.DisplayOpenPorts = true
	extras := responsiveHomeSessions()
	for _, sess := range extras {
		sess.RepoDisplayName = "alpha"
	}
	h.Sessions = append(h.Sessions, extras...)
	h.DaemonWaitingReasons = map[string]string{"long": "awaiting ready_for_review on acme/widget#123"}
	h.Filter.SeedApplied("alpha")
	h.BuildTableRows()
	if got := h.TableDataRowCount(); got != len(h.Table.Rows()) {
		t.Fatalf("count %d differs from %d rendered rows", got, len(h.Table.Rows()))
	}
	for _, id := range []string{"eps", "warn", "long"} {
		row, ok := h.TableCursorForSessionID(id)
		if !ok {
			t.Fatalf("missing %s", id)
		}
		h.Table.SetCursor(row)
		if h.SelectedSessionID() != id {
			t.Fatalf("cursor selects %s, want %s", h.SelectedSessionID(), id)
		}
		sess := h.SelectedSession()
		if h.SubRowCount(sess) == 0 {
			t.Fatalf("%s fixture lost auxiliary rows", id)
		}
		h, _ = homeKey(h, tea.KeyPressMsg{Code: tea.KeyDown})
		if h.Table.Cursor() == row+1 {
			t.Fatalf("navigation stranded on %s auxiliary row", id)
		}
	}
	h.Filter.SeedApplied("localhost:3000")
	h.BuildTableRows()
	if len(h.BoardSessions()) != 0 {
		t.Fatal("endpoint sub-row text unexpectedly matched")
	}
}

func TestHomeFilterArchivedPollDismissesInvisibleInput(t *testing.T) {
	h := filteredHome(t)
	h.Filter.Activate()
	model, _ := h.ApplySessionList(SessionListMsg{Sessions: []*pb.Session{{Id: "archived", ArchivedAt: timestamppb.Now(), Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}}}})
	h = model.(Model)
	if h.Filter.Engaged() {
		t.Fatal("archived-only poll stranded filter behind empty state")
	}
}

func TestHomeFilterMatchesDisplayedFixStatus(t *testing.T) {
	for _, tc := range []struct {
		name, label, query string
		Status             pb.DisplayStatus
	}{
		{"failing checks", "⨯ failing", "FAILING", pb.DisplayStatus_DISPLAY_STATUS_FAILING},
		{"merge conflict", "⨯ conflict", "conflict", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewModel(nil, context.Background(), nil)
			h.Loading = false
			h.Sessions = []*pb.Session{{Id: "target", DisplayLabel: tc.label, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, DisplayStatus: tc.Status, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX}}, {Id: "other", Title: "Unrelated"}}
			h.Filter.SeedApplied(tc.query)
			h.BuildTableRows()
			if h.SelectedSessionID() != "target" || len(h.BoardSessions()) != 1 {
				t.Fatalf("query %q did not match displayed FIX status %q", tc.query, tc.label)
			}
			h.Filter.SeedApplied("fix")
			h.BuildTableRows()
			if len(h.BoardSessions()) != 0 {
				t.Fatal("undisplayed FIX label unexpectedly matched")
			}
		})
	}
}
