package views

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestHomeFilterSlashOpensInput(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.sessions = []*pb.Session{{Id: "one", Title: "Alpha"}, {Id: "two", Title: "Beta"}}
	h.buildTableRows()
	model, _ := h.Update(keyString("/"))
	h = model.(HomeModel)
	if !h.textEntryActive() || !strings.Contains(h.renderSessionTable(), "type to filter") {
		t.Fatal("slash did not open filter")
	}
	model, _ = h.Update(tea.KeyPressMsg{Code: 'B'})
	h = model.(HomeModel)
	if h.selectedSessionID() != "two" {
		t.Fatalf("selected %q, want two", h.selectedSessionID())
	}
}

func filteredHome(t *testing.T) HomeModel {
	t.Helper()
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.width, h.height = 120, 30
	h.repoCount = 1
	h.sessions = []*pb.Session{
		{Id: "hidden", Title: "Hidden", RepoDisplayName: "other"},
		{Id: "review", Title: "Alpha review", RepoDisplayName: "app", BranchName: "feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Please check the diff"}},
		{Id: "progress", Title: "Alpha build", RepoDisplayName: "app", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}},
	}
	h.buildTableRows()
	return h
}

func homeKey(h HomeModel, msg tea.Msg) (HomeModel, tea.Cmd) {
	model, cmd := h.Update(msg)
	return model.(HomeModel), cmd
}

func TestHomeFilterFieldsAndBoardMapping(t *testing.T) {
	pr := int32(42)
	tracker := "ENG-7"
	s := &pb.Session{Id: "one", RepoDisplayName: "MyRepo", Title: "Title", BranchName: "Branch", TrackerId: &tracker, PrNumber: &pr, DisplayLabel: "Stopped"}
	h := NewHomeModel(nil, context.Background(), nil)
	h.sessions = []*pb.Session{s}
	if &h.boardSessions()[0] != &h.sessions[0] {
		t.Fatal("empty query must return original slice")
	}
	for _, q := range []string{"myrepo", "TITLE", "branch", "#42", "eng-7", "stopped"} {
		h.filter.SeedApplied(q)
		if len(h.boardSessions()) != 1 {
			t.Errorf("missing match %q", q)
		}
	}
	h = filteredHome(t)
	h.filter.SeedApplied("alpha")
	h.buildTableRows()
	if len(h.sessions) != 3 || len(h.boardSessions()) != 2 {
		t.Fatal("filter narrowed full state")
	}
	if h.tableDataRowCount() != len(h.table.Rows()) {
		t.Fatal("row counts differ")
	}
	for i, row := range h.primarySessionRows() {
		h.table.SetCursor(row)
		if got := h.selectedSession(); got != h.boardSessions()[i] {
			t.Fatalf("cursor %d selects %v", row, got)
		}
	}
	for _, want := range []string{"NEEDS YOU · 1", "IN PROGRESS · 1", "(2 of 3)"} {
		if !strings.Contains(h.renderSessionTable(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if h.neighborSessionID("review") != "progress" {
		t.Fatal("neighbor must be visible")
	}
}

func TestHomeFilterLifecycleAndPaste(t *testing.T) {
	h := filteredHome(t)
	h, _ = homeKey(h, keyString("/"))
	for _, letter := range "qnslr" {
		h, _ = homeKey(h, tea.KeyPressMsg{Code: letter})
	}
	if h.filter.Query() != "qnslr" || !h.filter.Active() {
		t.Fatal("action keys escaped input")
	}
	if strings.Contains(h.renderSessionTable(), "[q]uit") {
		t.Fatal("typing advertises quit")
	}
	h, _ = homeKey(h, keyString("esc"))
	h, _ = homeKey(h, keyString("/"))
	h, _ = homeKey(h, tea.PasteMsg{Content: "Alpha"})
	if h.filter.Query() != "Alpha" || h.selectedSessionID() != "review" {
		t.Fatal("paste did not filter/reset cursor")
	}
	h, _ = homeKey(h, tea.KeyPressMsg{Code: tea.KeyDown})
	if h.selectedSessionID() != "progress" {
		t.Fatal("down did not skip auxiliary/header rows")
	}
	h, _ = homeKey(h, keyString("enter"))
	if !h.filter.Applied() || h.filter.Active() {
		t.Fatal("enter did not commit")
	}
	_, cmd := homeKey(h, keyString("enter"))
	if cmd == nil {
		t.Fatal("second enter did not select")
	}
	if msg, ok := cmd().(switchViewMsg); !ok || msg.view != ViewChatPicker {
		t.Fatal("second enter did not open chat picker")
	}
	h, _ = homeKey(h, keyString("/"))
	if h.filter.Query() != "Alpha" {
		t.Fatal("reopen lost query")
	}
	h, _ = homeKey(h, keyString("esc"))
	h, _ = homeKey(h, keyString("/"))
	h, _ = homeKey(h, keyString("enter"))
	if h.filter.Engaged() {
		t.Fatal("empty commit stayed engaged")
	}
}

func TestHomeFilterNoMatchAndHiddenInputs(t *testing.T) {
	h := filteredHome(t)
	h.filter.SeedApplied("absent")
	h.buildTableRows()
	screen := h.renderSessionTable()
	if !strings.Contains(screen, "No sessions match filter.") || strings.Contains(screen, "[enter] select") || strings.Contains(screen, "no active sessions") {
		t.Fatalf("bad no-match view: %s", screen)
	}
	if h.selectedSession() != nil {
		t.Fatal("no-match has selection")
	}
	for _, mode := range []string{"loading", "error", "empty"} {
		m := filteredHome(t)
		m.filter.Activate()
		switch mode {
		case "loading":
			m.loading = true
		case "error":
			m.err = fmt.Errorf("offline")
		case "empty":
			m.sessions = nil
		}
		m = m.dismissInputsIfHidden()
		if m.filter.Engaged() {
			t.Errorf("%s stranded filter", mode)
		}
		m, _ = homeKey(m, keyString("/"))
		if m.filter.Active() {
			t.Errorf("%s opened hidden filter", mode)
		}
	}
}

func TestHomeFilterReorderRefused(t *testing.T) {
	h := filteredHome(t)
	h.filter.SeedApplied("alpha")
	h.buildTableRows()
	before := h.sessions[0]
	h, cmd := h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_UP)
	if cmd != nil || h.sessions[0] != before || h.status != "Clear the filter to reorder sessions" || !h.statusErr {
		t.Fatal("filtered reorder was not refused")
	}
}

func TestHomeFilterRebuildPreservesAppliedQuery(t *testing.T) {
	a := NewApp(nil, nil)
	a.home = filteredHome(t)
	a.home.filter.Activate()
	a.home.filter.input.SetValue("alpha")
	home := a.newHomeModel()
	home.buildTableRows()
	if !home.filter.Applied() || home.textEntryActive() || home.filter.Query() != "alpha" || len(home.boardSessions()) != 2 {
		t.Fatal("rebuild lost applied query")
	}
}

func TestHomeFilterPollKeepsFullState(t *testing.T) {
	h := filteredHome(t)
	h.filter.SeedApplied("alpha")
	h.buildTableRows()
	h.table.SetCursor(h.primarySessionRows()[1])
	h.archivingOverrideIDs["hidden"] = struct{}{}
	h.archiveInFlightIDs["hidden"] = struct{}{}
	incoming := append([]*pb.Session(nil), h.sessions...)
	model, cmd := h.applySessionList(sessionListMsg{sessions: incoming})
	h = model.(HomeModel)
	if cmd != nil {
		t.Fatal("unchanged hidden sessions notified again")
	}
	if h.selectedSessionID() != "progress" || h.filter.Query() != "alpha" {
		t.Fatal("poll lost filtered selection/query")
	}
	if _, ok := h.archivingOverrideIDs["hidden"]; !ok {
		t.Fatal("poll lost hidden archive override")
	}
	h.filter.Activate()
	model, _ = h.applySessionList(sessionListMsg{})
	if model.(HomeModel).filter.Engaged() {
		t.Fatal("empty poll stranded filter")
	}
	h = filteredHome(t)
	h.filter.Activate()
	for i := 0; i < pollFailureThreshold; i++ {
		model, _ = h.handleSessionListError(fmt.Errorf("offline"))
		h = model.(HomeModel)
	}
	if h.filter.Engaged() {
		t.Fatal("error stranded filter")
	}
}

func TestHomeFilterAuxiliaryRowsStayWithSession(t *testing.T) {
	h := filteredHome(t)
	extras := responsiveHomeSessions()
	for _, sess := range extras {
		sess.RepoDisplayName = "alpha"
	}
	h.sessions = append(h.sessions, extras...)
	h.daemonWaitingReasons = map[string]string{"long": "awaiting ready_for_review on acme/widget#123"}
	h.filter.SeedApplied("alpha")
	h.buildTableRows()
	if got := h.tableDataRowCount(); got != len(h.table.Rows()) {
		t.Fatalf("count %d differs from %d rendered rows", got, len(h.table.Rows()))
	}
	for _, id := range []string{"eps", "warn", "long"} {
		row, ok := h.tableCursorForSessionID(id)
		if !ok {
			t.Fatalf("missing %s", id)
		}
		h.table.SetCursor(row)
		if h.selectedSessionID() != id {
			t.Fatalf("cursor selects %s, want %s", h.selectedSessionID(), id)
		}
		sess := h.selectedSession()
		if h.subRowCount(sess) == 0 {
			t.Fatalf("%s fixture lost auxiliary rows", id)
		}
		h, _ = homeKey(h, tea.KeyPressMsg{Code: tea.KeyDown})
		if h.table.Cursor() == row+1 {
			t.Fatalf("navigation stranded on %s auxiliary row", id)
		}
	}
	h.filter.SeedApplied("localhost:3000")
	h.buildTableRows()
	if len(h.boardSessions()) != 0 {
		t.Fatal("endpoint sub-row text unexpectedly matched")
	}
}

func TestHomeFilterDonePollDismissesInvisibleInput(t *testing.T) {
	h := filteredHome(t)
	h.filter.Activate()
	model, _ := h.applySessionList(sessionListMsg{sessions: []*pb.Session{{Id: "done", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}}}})
	h = model.(HomeModel)
	if h.filter.Engaged() {
		t.Fatal("DONE-only poll stranded filter behind empty state")
	}
}

func TestHomeFilterMatchesDisplayedFixStatus(t *testing.T) {
	for _, tc := range []struct {
		name, label, query string
		status             pb.DisplayStatus
	}{
		{"failing checks", "⨯ failing", "FAILING", pb.DisplayStatus_DISPLAY_STATUS_FAILING},
		{"merge conflict", "⨯ conflict", "conflict", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.loading = false
			h.sessions = []*pb.Session{{Id: "target", DisplayLabel: tc.label, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, DisplayStatus: tc.status, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX}}, {Id: "other", Title: "Unrelated"}}
			h.filter.SeedApplied(tc.query)
			h.buildTableRows()
			if h.selectedSessionID() != "target" || len(h.boardSessions()) != 1 {
				t.Fatalf("query %q did not match displayed FIX status %q", tc.query, tc.label)
			}
			h.filter.SeedApplied("fix")
			h.buildTableRows()
			if len(h.boardSessions()) != 0 {
				t.Fatal("undisplayed FIX label unexpectedly matched")
			}
		})
	}
}
