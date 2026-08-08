package views

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newRepoListModelForTest builds a RepoListModel with the spinner and table
// wired up the way NewRepoListModel does, but without a live client so the
// pure Update/View logic can be exercised directly.
func newRepoListModelForTest(repos []*pb.Repo) RepoListModel {
	return RepoListModel{
		spinner: newStatusSpinner(),
		table:   newBossTable(nil, nil, 0),
		repos:   repos,
	}
}

func updateRepoList(t *testing.T, m RepoListModel, msg tea.Msg) (RepoListModel, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	rm, ok := updated.(RepoListModel)
	if !ok {
		t.Fatalf("Update returned %T, want RepoListModel", updated)
	}
	return rm, cmd
}

// TestRepoListUpdate_SpinnerTickRebuildsTableWhileDeleting pins repo_list.go:132
// (`if m.deletingRepoID != ""`): a spinner tick rebuilds the table only while a
// delete is in flight, so the row picks up the "deleting" status animation.
func TestRepoListUpdate_SpinnerTickRebuildsTableWhileDeleting(t *testing.T) {
	repos := []*pb.Repo{{Id: "repo-1", DisplayName: "alpha", LocalPath: "/tmp/alpha"}}

	// A delete in flight: the tick rebuilds the table from the empty initial one.
	m := newRepoListModelForTest(repos)
	m.deletingRepoID = "repo-1"
	m, _ = updateRepoList(t, m, spinner.TickMsg{})
	if got := len(m.table.Rows()); got != 1 {
		t.Fatalf("with deletingRepoID, table rows = %d, want 1 (buildTable should run)", got)
	}

	// No delete in flight: the tick leaves the table untouched (still empty).
	idle := newRepoListModelForTest(repos)
	idle, _ = updateRepoList(t, idle, spinner.TickMsg{})
	if got := len(idle.table.Rows()); got != 0 {
		t.Fatalf("without deletingRepoID, table rows = %d, want 0 (buildTable should be skipped)", got)
	}
}

// TestRepoListUpdate_HighlightsRepoAfterLoad pins repo_list.go:145 and :147
// (`if m.highlightRepoID != ""` and `if repo.Id == m.highlightRepoID`): after a
// load the requested repo is highlighted and the one-shot highlight is cleared.
func TestRepoListUpdate_HighlightsRepoAfterLoad(t *testing.T) {
	repos := []*pb.Repo{
		{Id: "repo-1", DisplayName: "alpha", LocalPath: "/a"},
		{Id: "repo-2", DisplayName: "bravo", LocalPath: "/b"},
		{Id: "repo-3", DisplayName: "charlie", LocalPath: "/c"},
	}

	// Highlight the third repo: the cursor lands on it and the request is cleared.
	m := RepoListModel{spinner: newStatusSpinner(), table: newBossTable(nil, nil, 0), highlightRepoID: "repo-3"}
	m, _ = updateRepoList(t, m, repoListLoadedMsg{repos: repos})
	if got := m.table.Cursor(); got != 2 {
		t.Fatalf("cursor after highlight = %d, want 2 (repo-3 after name sort)", got)
	}
	if m.highlightRepoID != "" {
		t.Fatalf("highlightRepoID = %q, want cleared after use", m.highlightRepoID)
	}

	// No highlight requested: the cursor stays at the default top row.
	plain := RepoListModel{spinner: newStatusSpinner(), table: newBossTable(nil, nil, 0)}
	plain, _ = updateRepoList(t, plain, repoListLoadedMsg{repos: repos})
	if got := plain.table.Cursor(); got != 0 {
		t.Fatalf("cursor without highlight = %d, want 0", got)
	}
}

// TestRepoListUpdate_RepoRemovedClearsDeletingOnMatch pins repo_list.go:158 and
// :162 (`if msg.id == m.deletingRepoID` and `if msg.err != nil`): a failed
// removal for the in-flight repo clears the deleting marker, records the error,
// and does not schedule a reload.
func TestRepoListUpdate_RepoRemovedClearsDeletingOnMatch(t *testing.T) {
	wantErr := errors.New("boom")

	m := newRepoListModelForTest([]*pb.Repo{{Id: "repo-1", DisplayName: "alpha", LocalPath: "/a"}})
	m.deletingRepoID = "repo-1"
	m.confirming = true
	m, _ = updateRepoList(t, m, repoRemovedMsg{id: "repo-1", err: wantErr})
	if m.deletingRepoID != "" {
		t.Fatalf("deletingRepoID = %q, want cleared on matching id", m.deletingRepoID)
	}
	if m.err == nil {
		t.Fatalf("err = nil, want the removal error recorded")
	}
	if m.confirming {
		t.Fatalf("confirming = true, want reset to false")
	}
	if m.loading {
		t.Fatalf("loading = true, want false on error (no reload)")
	}

	// A result for a different repo leaves the in-flight marker untouched.
	other := newRepoListModelForTest([]*pb.Repo{{Id: "repo-1", DisplayName: "alpha", LocalPath: "/a"}})
	other.deletingRepoID = "repo-1"
	other, _ = updateRepoList(t, other, repoRemovedMsg{id: "repo-2", err: wantErr})
	if other.deletingRepoID != "repo-1" {
		t.Fatalf("deletingRepoID = %q, want preserved on non-matching id", other.deletingRepoID)
	}
}

// TestRepoListUpdate_RepoRemovedSuccessTriggersReload pins repo_list.go:162 on
// its success side: a removal with no error flips the model back to loading and
// returns a reload command rather than surfacing an error.
func TestRepoListUpdate_RepoRemovedSuccessTriggersReload(t *testing.T) {
	m := newRepoListModelForTest([]*pb.Repo{{Id: "repo-1", DisplayName: "alpha", LocalPath: "/a"}})
	m.deletingRepoID = "repo-1"
	m, cmd := updateRepoList(t, m, repoRemovedMsg{id: "repo-1"})
	if !m.loading {
		t.Fatalf("loading = false, want true after a successful removal")
	}
	if cmd == nil {
		t.Fatalf("cmd = nil, want a reload command")
	}
	if m.err != nil {
		t.Fatalf("err = %v, want nil on success", m.err)
	}
}

// TestRepoListUpdate_DeleteKeyGatesOnReposAndNotDeleting pins repo_list.go:183
// (`if len(m.repos) > 0 && m.deletingRepoID == ""`): 'd' opens the confirm
// prompt only when there is a repo to delete and no delete already running.
func TestRepoListUpdate_DeleteKeyGatesOnReposAndNotDeleting(t *testing.T) {
	repos := []*pb.Repo{{Id: "repo-1", DisplayName: "alpha", LocalPath: "/a"}}

	ready := newRepoListModelForTest(repos)
	ready, _ = updateRepoList(t, ready, keyPress('d'))
	if !ready.confirming {
		t.Fatalf("confirming = false, want true when a repo exists and none is deleting")
	}

	empty := newRepoListModelForTest(nil)
	empty, _ = updateRepoList(t, empty, keyPress('d'))
	if empty.confirming {
		t.Fatalf("confirming = true, want false when there are no repos")
	}

	inFlight := newRepoListModelForTest(repos)
	inFlight.deletingRepoID = "repo-1"
	inFlight, _ = updateRepoList(t, inFlight, keyPress('d'))
	if inFlight.confirming {
		t.Fatalf("confirming = true, want false while a delete is already in flight")
	}
}

// TestRepoListUpdate_EnterRequiresRepos pins repo_list.go:188 (`if len(m.repos)
// > 0`): enter opens repo settings for the highlighted repo when one exists and
// is a no-op on an empty list.
func TestRepoListUpdate_EnterRequiresRepos(t *testing.T) {
	repos := []*pb.Repo{{Id: "repo-1", DisplayName: "alpha", LocalPath: "/a"}}
	m := newRepoListModelForTest(repos)
	_, cmd := updateRepoList(t, m, specialKeyPress(tea.KeyEnter))
	if cmd == nil {
		t.Fatalf("enter with repos returned nil cmd, want a switchView command")
	}
	sv, ok := cmd().(switchViewMsg)
	if !ok {
		t.Fatalf("enter cmd produced %T, want switchViewMsg", cmd())
	}
	if sv.view != ViewRepoSettings || sv.sessionID != "repo-1" {
		t.Fatalf("switchViewMsg = {view:%v, id:%q}, want {ViewRepoSettings, repo-1}", sv.view, sv.sessionID)
	}

	empty := newRepoListModelForTest(nil)
	if _, cmd = updateRepoList(t, empty, specialKeyPress(tea.KeyEnter)); cmd != nil {
		t.Fatalf("enter with no repos returned a cmd, want nil")
	}
}

// TestRepoListView_RendersError pins repo_list.go:231 (`if m.err != nil`): a
// load error is surfaced in the view ahead of the loading and table branches.
func TestRepoListView_RendersError(t *testing.T) {
	m := newRepoListModelForTest(nil)
	m.err = errors.New("kaboom")
	content := m.View().Content
	if !strings.Contains(content, "Error:") || !strings.Contains(content, "kaboom") {
		t.Fatalf("view did not surface the error; got:\n%s", content)
	}
}

func TestRepoListBuildTable_FitsColumnsToTerminalWidth(t *testing.T) {
	repos := []*pb.Repo{{
		Id:          "repo-1",
		DisplayName: strings.Repeat("n", 30),
		LocalPath:   "/" + strings.Repeat("p", 59),
	}}
	// The status column is deliberately unlabelled (empty title) so it is not
	// visible in the steady state; it still participates in the responsive fit
	// at priority 1, so it outlives PATH as the terminal narrows.
	wantTitles := map[int][]string{
		0:   {" ", "NAME", "PATH", ""},
		60:  {" ", "NAME", ""},
		72:  {" ", "NAME", ""},
		80:  {" ", "NAME", ""},
		100: {" ", "NAME", ""},
		140: {" ", "NAME", "PATH", ""},
	}
	var unfitted []table.Column
	for _, width := range []int{0, 60, 72, 80, 100, 140} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := newRepoListModelForTest(repos)
			m.width = width
			m.buildTable()
			assertTableRowsMatchColumns(t, m.table.Columns(), m.table.Rows())
			if width > 0 && columnsWidth(m.table.Columns()) > width {
				t.Fatalf("columns width = %d, want <= terminal width %d", columnsWidth(m.table.Columns()), width)
			}
			if !slices.Contains(columnTitles(m.table.Columns()), "NAME") {
				t.Fatalf("titles = %v, want priority-0 NAME retained", columnTitles(m.table.Columns()))
			}
			if width == 0 {
				unfitted = append([]table.Column(nil), m.table.Columns()...)
			}
			if got := columnTitles(m.table.Columns()); !slices.Equal(got, wantTitles[width]) {
				t.Fatalf("width %d titles = %v, want %v", width, got, wantTitles[width])
			}
			if width == 140 && !reflect.DeepEqual(m.table.Columns(), unfitted) {
				t.Fatalf("140-column set = %#v, want byte-identical unfitted %#v", m.table.Columns(), unfitted)
			}
		})
	}
}

func assertTableRowsMatchColumns(t *testing.T, cols []table.Column, rows []table.Row) {
	t.Helper()
	for i, row := range rows {
		if len(row) != len(cols) {
			t.Fatalf("row %d has %d cells for %d columns", i, len(row), len(cols))
		}
	}
}
