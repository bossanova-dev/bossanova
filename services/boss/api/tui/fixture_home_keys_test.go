package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// renameKeyHome is a three-session board with its rows already built, so the
// table cursor has somewhere to move to.
func renameKeyHome(t *testing.T) HomeModel {
	t.Helper()
	h := NewHomeModel(nil, context.Background(), nil)
	h.Loading = false
	h.RepoCount = 1
	h.Width = 120
	h.Height = 30
	h.Sessions = []*pb.Session{
		{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
		{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
		{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
	}
	h.BuildTableRows()

	if got := len(h.Table.Rows()); got != len(h.Sessions) {
		t.Fatalf("fixture built %d rows for %d sessions; the cursor assertions assume one row each", got, len(h.Sessions))
	}
	if h.Table.Cursor() != 0 {
		t.Fatalf("fixture starts at cursor %d, want 0", h.Table.Cursor())
	}
	return h
}

func homeFromKey(t *testing.T, model tea.Model) HomeModel {
	t.Helper()
	h, ok := model.(HomeModel)
	if !ok {
		t.Fatalf("handler returned %T, want views.HomeModel", model)
	}
	return h
}

// mustModel adapts the (tea.Model, tea.Cmd) handlers for the assertions that
// only care about the model.
func mustModel(model tea.Model, _ tea.Cmd) tea.Model { return model }
