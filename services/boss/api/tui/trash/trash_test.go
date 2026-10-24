package trash

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type trashStubClient struct {
	sessions    []*pb.Session
	removedIDs  []string
	restoredIDs []string
	emptyCalled bool
	failOnID    string // when set, RemoveSession returns an error for this id

	// Resurrect stream script (BOS-984): the setup-output lines the stream
	// replays before its terminal frame, the non-fatal setup error that frame
	// carries, and an error returned instead of opening the stream at all.
	resurrectSetupLines []string
	resurrectSetupError string
	resurrectErr        error
}

func (s *trashStubClient) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return s.sessions, nil
}

func (s *trashStubClient) RemoveSession(_ context.Context, id string) error {
	if s.failOnID != "" && id == s.failOnID {
		return fmt.Errorf("remove failed for %s", id)
	}
	s.removedIDs = append(s.removedIDs, id)
	return nil
}

func (s *trashStubClient) ResurrectSession(_ context.Context, id string) (rpc.ResurrectSessionStream, error) {
	s.restoredIDs = append(s.restoredIDs, id)
	if s.resurrectErr != nil {
		return nil, s.resurrectErr
	}
	frames := make([]*pb.ResurrectSessionResponse, 0, len(s.resurrectSetupLines)+1)
	for _, line := range s.resurrectSetupLines {
		frames = append(frames, &pb.ResurrectSessionResponse{
			Event: &pb.ResurrectSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: line}},
		})
	}
	frames = append(frames, &pb.ResurrectSessionResponse{
		Event: &pb.ResurrectSessionResponse_SessionResurrected{
			SessionResurrected: &pb.SessionResurrected{
				Session:    &pb.Session{Id: id},
				SetupError: s.resurrectSetupError,
			},
		},
	})
	return &stubResurrectStream{frames: frames}, nil
}

// stubResurrectStream replays a scripted frame list as a
// rpc.ResurrectSessionStream.
type stubResurrectStream struct {
	frames []*pb.ResurrectSessionResponse
	i      int
	closed bool
}

func (s *stubResurrectStream) Receive() bool {
	if s.i >= len(s.frames) {
		return false
	}
	s.i++
	return true
}

func (s *stubResurrectStream) Msg() *pb.ResurrectSessionResponse { return s.frames[s.i-1] }

func (s *stubResurrectStream) Err() error { return nil }

func (s *stubResurrectStream) Close() error {
	s.closed = true
	return nil
}

func (s *trashStubClient) EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error) {
	s.emptyCalled = true
	return int32(len(s.sessions)), nil
}

func TestTrashModel_FilterMatchesVisibleColumns(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	cases := []struct {
		name      string
		query     string
		wantNames []string
	}{
		{name: "repo", query: "payments", wantNames: []string{"Fix checkout"}},
		{name: "name", query: "dark", wantNames: []string{"feature/dark-mode"}},
		{name: "pr", query: "#102", wantNames: []string{"Fix checkout"}},
		{name: "archived", query: display.RelativeTime(trashArchiveTime(0)), wantNames: []string{"Fix checkout", "feature/dark-mode", "Docs refresh"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = m.filter.Activate()
			m.filter.Input.SetValue(tc.query)
			m.buildTable()

			if got := len(m.table.Rows()); got != len(tc.wantNames) {
				t.Fatalf("rows = %d, want %d", got, len(tc.wantNames))
			}
			for i, want := range tc.wantNames {
				if got := m.table.Rows()[i][2]; got != want {
					t.Fatalf("row %d name = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestTrashBuildTable_FitsColumnsToTerminalWidth(t *testing.T) {
	prNumber := int32(12345678)
	sessions := []*pb.Session{{
		Id:              "session-1",
		RepoDisplayName: strings.Repeat("r", 20),
		Title:           strings.Repeat("n", 60),
		BranchName:      strings.Repeat("b", 60),
		PrNumber:        &prNumber,
		ArchivedAt:      timestamppb.New(trashArchiveTime(0)),
	}}
	wantTitles := map[int][]string{
		0:   {" ", "REPO", "NAME", "PR", "ARCHIVED"},
		60:  {" ", "NAME"},
		72:  {" ", "NAME"},
		80:  {" ", "NAME", "ARCHIVED"},
		100: {" ", "NAME", "PR", "ARCHIVED"},
		140: {" ", "REPO", "NAME", "PR", "ARCHIVED"},
	}
	var unfitted []table.Column
	for _, width := range []int{0, 60, 72, 80, 100, 140} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := newLoadedTrashModel(sessions)
			m.width = width
			m.buildTable()
			assertTableRowsMatchColumns(t, m.table.Columns(), m.table.Rows())
			if width > 0 && ui.ColumnsWidth(m.table.Columns()) > width {
				t.Fatalf("columns width = %d, want <= terminal width %d", ui.ColumnsWidth(m.table.Columns()), width)
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

func TestTrashModel_FilterRendersBetweenTableAndActionBar(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	_ = m.filter.Activate()
	m.filter.Input.SetValue("checkout")
	m.buildTable()

	view := m.View().Content
	assertAppearsBefore(t, view, "Fix checkout", "(1 of 3)")
	assertAppearsBefore(t, view, "(1 of 3)", "type to search")
}

func TestTrashModel_FilterKeyLifecycle(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	m = updateTrash(t, m, keyString("/"))
	if !m.filter.Active() {
		t.Fatal("filter should be active after /")
	}

	m = updateTrash(t, m, tea.KeyPressMsg{Code: 'd'})
	if got := len(m.table.Rows()); got != 2 {
		t.Fatalf("rows after typing d = %d, want 2", got)
	}

	m = updateTrash(t, m, keyString("enter"))
	if !m.filter.Applied() || m.filter.Active() {
		t.Fatalf("after enter: active=%v applied=%v, want active=false applied=true", m.filter.Active(), m.filter.Applied())
	}

	m = updateTrash(t, m, keyString("/"))
	if !m.filter.Active() || m.filter.Query() != "d" {
		t.Fatalf("after second /: active=%v query=%q, want active=true query=d", m.filter.Active(), m.filter.Query())
	}

	m = updateTrash(t, m, keyString("esc"))
	if m.filter.Engaged() {
		t.Fatal("filter should clear after esc while active")
	}

	m = updateTrash(t, m, keyString("/"))
	m = updateTrash(t, m, tea.KeyPressMsg{Code: 'p'})
	m = updateTrash(t, m, keyString("enter"))
	if !m.filter.Applied() {
		t.Fatal("filter should be applied before applied esc")
	}
	m = updateTrash(t, m, keyString("esc"))
	if m.filter.Engaged() || m.cancel {
		t.Fatalf("applied esc should clear filter without cancel: engaged=%v cancel=%v", m.filter.Engaged(), m.cancel)
	}
}

func TestTrashModel_EmptyQueryCommitClearsFilter(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	m = updateTrash(t, m, keyString("/"))
	m = updateTrash(t, m, keyString("enter"))

	if m.filter.Engaged() {
		t.Fatal("empty query commit should clear filter")
	}
	if got := len(m.table.Rows()); got != len(trashFixtureSessions()) {
		t.Fatalf("rows = %d, want all sessions", got)
	}
}

func TestTrashModel_RestoreUsesSelectedFilteredSession(t *testing.T) {
	client := &trashStubClient{sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("docs")
	_ = m.filter.Commit()
	m.buildTable()

	model, cmd := m.Update(keyString("r"))
	m = model.(Model)
	if !m.restoring {
		t.Fatal("model should enter restoring state")
	}
	// The first command opens the stream (BOS-984); the restore result arrives
	// on a later frame.
	msg := cmd().(RestoreStreamOpenedMsg)
	if msg.ID != "sess-3" {
		t.Fatalf("restored id = %q, want sess-3", msg.ID)
	}
	if got := client.restoredIDs; len(got) != 1 || got[0] != "sess-3" {
		t.Fatalf("client restored IDs = %v, want [sess-3]", got)
	}
}

func TestTrashModel_DeleteConfirmationUsesSelectedFilteredSession(t *testing.T) {
	client := &trashStubClient{sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("docs")
	_ = m.filter.Commit()
	m.buildTable()

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	if !m.deleting {
		t.Fatal("model should enter deleting state")
	}
	msg := cmd().(SessionDeletedMsg)
	if msg.ID != "sess-3" {
		t.Fatalf("deleted id = %q, want sess-3", msg.ID)
	}
	if got := client.removedIDs; len(got) != 1 || got[0] != "sess-3" {
		t.Fatalf("client removed IDs = %v, want [sess-3]", got)
	}
}

func TestTrashModel_SingleDeleteTitleContainingAllDoesNotEnterDeleteAllState(t *testing.T) {
	client := &trashStubClient{
		sessions: []*pb.Session{
			{
				Id:              "sess-all",
				RepoDisplayName: "ops",
				Title:           "remove all logs",
				ArchivedAt:      timestamppb.New(trashArchiveTime(0)),
			},
			{
				Id:              "sess-next",
				RepoDisplayName: "ops",
				Title:           "next cleanup",
				ArchivedAt:      timestamppb.New(trashArchiveTime(-1 * time.Minute)),
			},
		},
	}
	m := newLoadedTrashModelWithClient(client)

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	if m.deletingAll {
		t.Fatal("single delete should not enter delete-all state")
	}
	if !m.deleting {
		t.Fatal("single delete should enter deleting state")
	}

	msg := cmd().(SessionDeletedMsg)
	m = updateTrashMsg(t, m, msg)
	if m.deletingAll {
		t.Fatal("single delete should not leave delete-all state after completion")
	}
	if got := trashSessionIDs(m.sessions); fmt.Sprint(got) != "[sess-next]" {
		t.Fatalf("remaining sessions = %v, want [sess-next]", got)
	}
	if strings.Contains(m.View().Content, "Deleting all") {
		t.Fatalf("single delete left delete-all view active: %q", m.View().Content)
	}

	m = updateTrash(t, m, keyString("d"))
	if !m.confirm.Active {
		t.Fatal("single delete should leave actions usable for remaining rows")
	}
}

func TestTrashModel_FilteredDeleteAllDeletesOnlyMatchedSessions(t *testing.T) {
	client := &trashStubClient{sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("docs")
	_ = m.filter.Commit()
	m.buildTable()

	m = updateTrash(t, m, keyString("a"))
	if !m.confirm.Active {
		t.Fatal("model should confirm filtered delete all")
	}
	if got := m.View().Content; !strings.Contains(got, "Permanently delete 1 matching archived sessions?") {
		t.Fatalf("confirmation copy missing filtered scope: %q", got)
	}

	// Changing the filter after the confirm prompt is raised must not change the
	// batch: the IDs were captured when [a] was pressed (the "docs" match).
	m.filter.Input.SetValue("payments")
	m.buildTable()

	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	m = drainDeleteAll(t, m, cmd)

	if client.emptyCalled {
		t.Fatal("filtered delete all should not call EmptyTrash")
	}
	if got := fmt.Sprint(client.removedIDs); got != "[sess-3]" {
		t.Fatalf("removed IDs = %v, want [sess-3]", client.removedIDs)
	}
	if got := trashSessionIDs(m.sessions); fmt.Sprint(got) != "[sess-1 sess-2]" {
		t.Fatalf("remaining sessions = %v, want [sess-1 sess-2]", got)
	}
	if m.deletingAll {
		t.Fatal("delete-all state should clear once the batch drains")
	}
}

func TestTrashModel_DeleteAllShowsProgressCount(t *testing.T) {
	client := &trashStubClient{sessions: trashSharedRepoSessions()}
	m := newLoadedTrashModelWithClient(client)

	m = updateTrash(t, m, keyString("a"))
	if !m.confirm.Active {
		t.Fatal("model should confirm delete all")
	}

	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	// Before any deletion completes the view reads as "currently deleting the
	// first of three".
	if m.deleteDone != 0 || m.deleteTotal != 3 {
		t.Fatalf("initial batch counters = %d/%d, want 0/3", m.deleteDone, m.deleteTotal)
	}
	if got := m.View().Content; !strings.Contains(got, "Deleting 1/3…") {
		t.Fatalf("initial progress frame missing counter: %q", got)
	}

	// First deletion completes; the counter advances to the second of three.
	msg := cmd().(DeleteProgressMsg)
	model, cmd = m.Update(msg)
	m = model.(Model)
	if m.deleteDone != 1 || m.deleteTotal != 3 {
		t.Fatalf("post-first counters = %d/%d, want 1/3", m.deleteDone, m.deleteTotal)
	}
	if got := m.View().Content; !strings.Contains(got, "Deleting 2/3…") {
		t.Fatalf("progress frame missing advanced counter: %q", got)
	}

	// Drain the remaining deletions; the trash ends up empty.
	m = drainDeleteAll(t, m, cmd)
	if m.deletingAll {
		t.Fatal("delete-all state should clear once the batch drains")
	}
	if m.deleteTotal != 0 || m.deleteDone != 0 {
		t.Fatalf("counters not reset after drain = %d/%d", m.deleteDone, m.deleteTotal)
	}
	if got := m.View().Content; !strings.Contains(got, "Trash is empty.") {
		t.Fatalf("view should settle to empty state: %q", got)
	}
}

func TestTrashModel_ListsOnlyArchivedSessionsAndDeleteAllSparesLiveOnes(t *testing.T) {
	archived := trashSharedRepoSessions()[:2]
	live := []*pb.Session{
		{Id: "live-x", RepoDisplayName: "alpha", Title: "still running"},
		{Id: "live-y", RepoDisplayName: "beta", Title: "also running"},
	}
	client := &trashStubClient{
		sessions: []*pb.Session{live[0], archived[0], live[1], archived[1]},
	}
	m := NewModel(client, context.Background())
	m.width, m.height = 120, 40
	m = updateTrashMsg(t, m, m.fetchArchived()())

	if got := fmt.Sprint(trashSessionIDs(m.sessions)); got != "[sess-a sess-b]" {
		t.Fatalf("trash sessions = %s, want [sess-a sess-b]", got)
	}
	view := m.View().Content
	for _, sess := range live {
		if strings.Contains(view, sess.Title) {
			t.Fatalf("trash lists live session %q:\n%s", sess.Title, view)
		}
	}

	m = updateTrash(t, m, keyString("a"))
	if got := m.View().Content; !strings.Contains(got, "Permanently delete all 2 archived sessions?") {
		t.Fatalf("confirmation should count only archived sessions: %q", got)
	}
	model, cmd := m.Update(keyString("enter"))
	drainDeleteAll(t, model.(Model), cmd)
	if got := fmt.Sprint(client.removedIDs); got != "[sess-a sess-b]" {
		t.Fatalf("removed IDs = %s, want [sess-a sess-b]", got)
	}
}

func TestTrashModel_OnlyLiveSessionsShowsEmptyTrash(t *testing.T) {
	client := &trashStubClient{
		sessions: []*pb.Session{{Id: "live-x", RepoDisplayName: "alpha", Title: "still running"}},
	}
	m := NewModel(client, context.Background())
	m.width, m.height = 120, 40
	m = updateTrashMsg(t, m, m.fetchArchived()())

	if got := m.View().Content; !strings.Contains(got, "Trash is empty.") || strings.Contains(got, "still running") {
		t.Fatalf("trash with only live sessions should be empty:\n%s", got)
	}
}

func TestTrashModel_DeleteAllProgressStopsOnError(t *testing.T) {
	client := &trashStubClient{
		sessions: trashSharedRepoSessions(),
		failOnID: "sess-b",
	}
	m := newLoadedTrashModelWithClient(client)

	m = updateTrash(t, m, keyString("a"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	m = drainDeleteAll(t, m, cmd)

	if got := fmt.Sprint(client.removedIDs); got != "[sess-a]" {
		t.Fatalf("removed IDs = %v, want [sess-a] (stops at first failure)", client.removedIDs)
	}
	if m.err == nil {
		t.Fatal("model should surface the delete error")
	}
	if m.deletingAll {
		t.Fatal("delete-all state should clear on error")
	}
	if got := trashSessionIDs(m.sessions); fmt.Sprint(got) != "[sess-b sess-c]" {
		t.Fatalf("remaining sessions = %v, want [sess-b sess-c] (only succeeded pruned)", got)
	}
}

func TestTrashModel_UnfilteredDeleteAllUsesRemoveSessionNotEmptyTrash(t *testing.T) {
	client := &trashStubClient{sessions: trashSharedRepoSessions()}
	m := newLoadedTrashModelWithClient(client)

	m = updateTrash(t, m, keyString("a"))
	if !m.confirm.Active {
		t.Fatal("model should confirm unfiltered delete all")
	}
	if got := m.View().Content; !strings.Contains(got, "Permanently delete all 3 archived sessions?") {
		t.Fatalf("confirmation copy missing unfiltered scope: %q", got)
	}

	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	m = drainDeleteAll(t, m, cmd)

	if client.emptyCalled {
		t.Fatal("unfiltered delete all should drive RemoveSession, not EmptyTrash")
	}
	if got := fmt.Sprint(client.removedIDs); got != "[sess-a sess-b sess-c]" {
		t.Fatalf("removed IDs = %v, want [sess-a sess-b sess-c]", client.removedIDs)
	}
	if len(m.sessions) != 0 {
		t.Fatalf("remaining sessions = %v, want none", trashSessionIDs(m.sessions))
	}
}

func TestTrashModel_PasteUpdatesActiveFilter(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	m = updateTrash(t, m, keyString("/"))
	if !m.filter.Active() {
		t.Fatal("filter should be active after /")
	}

	m = updateTrash(t, m, tea.PasteMsg{Content: "docs"})

	if got := m.filter.Query(); got != "docs" {
		t.Fatalf("query after paste = %q, want docs", got)
	}
	if got := len(m.table.Rows()); got != 1 {
		t.Fatalf("rows after paste = %d, want 1", got)
	}
	if got := m.table.Rows()[0][2]; got != "Docs refresh" {
		t.Fatalf("matched row name = %q, want Docs refresh", got)
	}
}

func TestTrashModel_FilteredDeleteAllPartialFailurePrunesOnlySucceeded(t *testing.T) {
	client := &trashStubClient{
		sessions: trashSharedRepoSessions(),
		failOnID: "sess-b",
	}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("alpha")
	_ = m.filter.Commit()
	m.buildTable()
	if got := len(m.table.Rows()); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}

	m = updateTrash(t, m, keyString("a"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	m = drainDeleteAll(t, m, cmd)

	if client.emptyCalled {
		t.Fatal("filtered delete all should not call EmptyTrash")
	}
	if got := fmt.Sprint(client.removedIDs); got != "[sess-a]" {
		t.Fatalf("removed IDs = %v, want [sess-a] (stops at first failure)", client.removedIDs)
	}
	if m.err == nil {
		t.Fatal("model should surface the delete error")
	}
	if m.deletingAll {
		t.Fatal("delete-all state should clear on error")
	}
	if got := trashSessionIDs(m.sessions); fmt.Sprint(got) != "[sess-b sess-c]" {
		t.Fatalf("remaining sessions = %v, want [sess-b sess-c] (only succeeded pruned)", got)
	}
}

func TestTrashModel_EscCancelsErrorScreenWithAppliedFilter(t *testing.T) {
	client := &trashStubClient{
		sessions: trashSharedRepoSessions(),
		failOnID: "sess-a",
	}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("alpha")
	_ = m.filter.Commit()
	m.buildTable()
	if !m.filter.Applied() {
		t.Fatal("filter should be applied before failed delete")
	}

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	msg := cmd().(SessionDeletedMsg)
	if msg.Err == nil {
		t.Fatal("delete should report an error")
	}
	m = updateTrashMsg(t, m, msg)
	if m.err == nil {
		t.Fatal("model should show the error screen")
	}

	m = updateTrash(t, m, keyString("esc"))
	if !m.cancel {
		t.Fatal("esc should leave the error screen even when a filter is applied")
	}
}

func TestTrashModel_CursorClampsAfterDeletingLastFilteredRow(t *testing.T) {
	client := &trashStubClient{sessions: trashSharedRepoSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("alpha")
	_ = m.filter.Commit()
	m.buildTable()

	m.table.SetCursor(2)
	if got := m.selectedSession(); got == nil || got.Id != "sess-c" {
		t.Fatalf("pre-delete selection = %v, want sess-c", got)
	}

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	msg := cmd().(SessionDeletedMsg)
	if msg.ID != "sess-c" {
		t.Fatalf("deleted id = %q, want sess-c", msg.ID)
	}
	m = updateTrashMsg(t, m, msg)

	got := m.selectedSession()
	if got == nil {
		t.Fatal("selection should clamp onto a remaining row, got nil")
	}
	if got.Id != "sess-b" {
		t.Fatalf("clamped selection = %q, want sess-b", got.Id)
	}
}

func TestTrashModel_EmptyingTrashClearsAppliedFilter(t *testing.T) {
	sessions := []*pb.Session{{
		Id:              "sess-only",
		RepoDisplayName: "solo",
		Title:           "Only one",
		ArchivedAt:      timestamppb.New(trashArchiveTime(0)),
	}}
	client := &trashStubClient{sessions: sessions}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.Input.SetValue("solo")
	_ = m.filter.Commit()
	m.buildTable()
	if !m.filter.Applied() {
		t.Fatal("filter should be applied")
	}

	// Delete the only session; the model should drop the now-meaningless filter.
	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(Model)
	msg := cmd().(SessionDeletedMsg)
	m = updateTrashMsg(t, m, msg)

	if m.filter.Engaged() {
		t.Fatal("empty trash should clear the applied filter")
	}

	// First esc should now cancel directly, not silently clear a hidden filter.
	m = updateTrash(t, m, keyString("esc"))
	if !m.cancel {
		t.Fatal("esc should cancel the view once trash is empty")
	}
}

func TestTrashModel_ZeroMatchFilterSuppressesRowActions(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	_ = m.filter.Activate()
	m.filter.Input.SetValue("no-match")
	m.buildTable()

	if got := len(m.table.Rows()); got != 0 {
		t.Fatalf("rows = %d, want 0", got)
	}
	view := m.View().Content
	if !strings.Contains(view, "No archived sessions match search.") {
		t.Fatalf("view missing no-match state: %q", view)
	}
	assertAppearsBefore(t, view, "No archived sessions match search.", "(0 of 3)")
	assertAppearsBefore(t, view, "(0 of 3)", "type to search")
	if strings.Contains(view, "[d]elete") || strings.Contains(view, "[r]estore") || strings.Contains(view, "[a] delete all") {
		t.Fatalf("view should suppress row actions with zero matches: %q", view)
	}
}

func assertAppearsBefore(t *testing.T, view, first, second string) {
	t.Helper()

	firstIndex := strings.Index(view, first)
	if firstIndex < 0 {
		t.Fatalf("view missing %q: %q", first, view)
	}
	secondIndex := strings.Index(view, second)
	if secondIndex < 0 {
		t.Fatalf("view missing %q: %q", second, view)
	}
	if firstIndex >= secondIndex {
		t.Fatalf("%q should appear before %q in view: %q", first, second, view)
	}
}

func newLoadedTrashModel(sessions []*pb.Session) Model {
	return newLoadedTrashModelWithClient(&trashStubClient{sessions: sessions})
}

func newLoadedTrashModelWithClient(client *trashStubClient) Model {
	m := NewModel(client, context.Background())
	m.loading = false
	m.sessions = client.sessions
	m.width = 120
	m.height = 40
	m.buildTable()
	return m
}

func updateTrash(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	return updateTrashMsg(t, m, msg)
}

func updateTrashMsg(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	model, _ := m.Update(msg)
	got, ok := model.(Model)
	if !ok {
		t.Fatalf("model type = %T, want TrashModel", model)
	}
	return got
}

// drainDeleteAll runs the delete-all command chain to completion: it executes
// each returned command, feeds the resulting DeleteProgressMsg back into the
// model, and follows the next command until the chain ends (queue empty or a
// mid-batch error clears it).
func drainDeleteAll(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg, ok := cmd().(DeleteProgressMsg)
		if !ok {
			t.Fatalf("expected DeleteProgressMsg from delete-all chain")
		}
		model, next := m.Update(msg)
		m = model.(Model)
		cmd = next
	}
	return m
}

func keyString(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEsc}
	case "/":
		return tea.KeyPressMsg{Code: '/'}
	default:
		if len(s) == 1 {
			return tea.KeyPressMsg{Code: []rune(s)[0]}
		}
		panic("unsupported key: " + s)
	}
}

func trashFixtureSessions() []*pb.Session {
	pr102 := int32(102)
	return []*pb.Session{
		{
			Id:              "sess-1",
			RepoDisplayName: "payments",
			Title:           "Fix checkout",
			BranchName:      "fix-checkout",
			PrNumber:        &pr102,
			ArchivedAt:      timestamppb.New(trashArchiveTime(0)),
		},
		{
			Id:              "sess-2",
			RepoDisplayName: "web",
			BranchName:      "feature/dark-mode",
			ArchivedAt:      timestamppb.New(trashArchiveTime(-1 * time.Minute)),
		},
		{
			Id:              "sess-3",
			RepoDisplayName: "docs",
			Title:           "Docs refresh",
			BranchName:      "docs-refresh",
			ArchivedAt:      timestamppb.New(trashArchiveTime(-2 * time.Minute)),
		},
	}
}

// trashSharedRepoSessions returns three sessions that all match the query
// "alpha", so a single filter selects every row. Used for multi-row delete-all
// and cursor-clamp tests where ordering must be deterministic (sess-a, -b, -c).
func trashSharedRepoSessions() []*pb.Session {
	return []*pb.Session{
		{Id: "sess-a", RepoDisplayName: "alpha", Title: "one", ArchivedAt: timestamppb.New(trashArchiveTime(0))},
		{Id: "sess-b", RepoDisplayName: "alpha", Title: "two", ArchivedAt: timestamppb.New(trashArchiveTime(-1 * time.Minute))},
		{Id: "sess-c", RepoDisplayName: "alpha", Title: "three", ArchivedAt: timestamppb.New(trashArchiveTime(-2 * time.Minute))},
	}
}

func trashArchiveTime(offset time.Duration) time.Time {
	return time.Now().Add(-2 * time.Hour).Add(offset)
}

func trashSessionIDs(sessions []*pb.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.Id)
	}
	return ids
}

// driveRestore runs a restore to completion: press [r], then pump every command
// the model returns until the terminal SessionRestoredMsg lands. It returns the
// model at each step so a test can assert what the user SAW mid-restore, which
// is the whole point of BOS-984's progress frames.
func driveRestore(t *testing.T, m Model) (steps []Model, final Model) {
	t.Helper()
	model, cmd := m.Update(keyString("r"))
	m = model.(Model)
	steps = append(steps, m)
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if msg == nil {
			break
		}
		model, cmd = m.Update(msg)
		m = model.(Model)
		steps = append(steps, m)
		if _, done := msg.(SessionRestoredMsg); done {
			break
		}
	}
	return steps, m
}

// TestTrashModel_RestoreRendersSetupProgress pins the user-visible half of
// BOS-984: while the repo's setup script runs, the restore shows what it is
// doing instead of an undifferentiated spinner.
func TestTrashModel_RestoreRendersSetupProgress(t *testing.T) {
	client := &trashStubClient{
		sessions:            trashFixtureSessions(),
		resurrectSetupLines: []string{"recreating worktree", "installing dependencies"},
	}
	m := newLoadedTrashModelWithClient(client)

	steps, final := driveRestore(t, m)

	var sawProgress bool
	for _, step := range steps {
		if !step.restoring {
			continue
		}
		view := step.View().Content
		if strings.Contains(view, "installing dependencies") {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Fatal("no rendered frame showed the setup script's progress; " +
			"the restore was a silent spinner for its whole duration")
	}
	if final.restoring {
		t.Fatal("model still restoring after the terminal frame")
	}
	if final.err != nil {
		t.Fatalf("restore reported an error: %v", final.err)
	}
	if final.restoreLine != "" {
		t.Fatalf("progress line %q survived the terminal frame", final.restoreLine)
	}
}

// TestTrashModel_RestoreSetupFailureRendersAsAWarningNotAnError pins AC3 in the
// UI: a restore whose setup script failed is a success with a caveat. It must
// NOT raise the error banner, which is how a failed restore (or a deadline)
// renders — conflating them is exactly what left the user unable to tell them
// apart.
func TestTrashModel_RestoreSetupFailureRendersAsAWarningNotAnError(t *testing.T) {
	client := &trashStubClient{
		sessions:            trashFixtureSessions(),
		resurrectSetupError: "setup script exited 1",
	}
	m := newLoadedTrashModelWithClient(client)

	_, final := driveRestore(t, m)

	if final.err != nil {
		t.Fatalf("a setup failure must not raise the error banner, got %v", final.err)
	}
	if final.restoreWarning == "" {
		t.Fatal("setup failure was dropped; the user is told nothing about missing dependencies")
	}
	view := final.View().Content
	if !strings.Contains(view, "setup script exited 1") {
		t.Fatalf("view does not surface the setup failure:\n%s", view)
	}
	if final.restoredID == "" {
		t.Fatal("the session did come back; it must be recorded as restored")
	}
}

// TestTrashModel_RestoreFailureRendersAsAnError is the contrast case: when the
// resurrect itself fails, the error banner is correct and the warning line must
// stay empty.
func TestTrashModel_RestoreFailureRendersAsAnError(t *testing.T) {
	client := &trashStubClient{
		sessions:     trashFixtureSessions(),
		resurrectErr: errors.New("resurrect worktree: fatal: invalid reference"),
	}
	m := newLoadedTrashModelWithClient(client)

	_, final := driveRestore(t, m)

	if final.err == nil {
		t.Fatal("a failed resurrect must raise the error banner")
	}
	if final.restoreWarning != "" {
		t.Fatalf("restoreWarning = %q; a failed restore is not a setup failure", final.restoreWarning)
	}
	if final.restoredID != "" {
		t.Fatalf("restoredID = %q; a failed restore restored nothing", final.restoredID)
	}
}

// TestTrashModel_FastRestoreShowsNoProgressFlicker pins the third U3 scenario: a
// repo with no setup script produces no progress frames, so the restore renders
// the plain spinner and nothing else. Progress must be evidence of work, not
// decoration.
func TestTrashModel_FastRestoreShowsNoProgressFlicker(t *testing.T) {
	client := &trashStubClient{sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	steps, final := driveRestore(t, m)

	for _, step := range steps {
		if step.restoreLine != "" {
			t.Fatalf("progress line %q appeared with no setup output to show", step.restoreLine)
		}
		if step.restoring && !strings.Contains(step.View().Content, "Restoring…") {
			t.Fatal("the restoring frame lost its spinner label")
		}
	}
	if final.err != nil {
		t.Fatalf("restore failed: %v", final.err)
	}
	if final.restoreWarning != "" {
		t.Fatalf("restoreWarning = %q on a clean restore", final.restoreWarning)
	}
}

// ListSessionsWithReadFailures satisfies the BossClient seam: this fake reads
// one place, so it never reports a partial read.
func (s *trashStubClient) ListSessionsWithReadFailures(ctx context.Context, req *pb.ListSessionsRequest, opts rpc.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	sessions, err := s.ListSessions(ctx, req, opts)
	return sessions, nil, err
}

// MoveSession satisfies the BossClient seam (BOS-1231). This stub is not part
// of a reorder test, so a call is a bug in the view under test.
func (s *trashStubClient) MoveSession(context.Context, *pb.MoveSessionRequest) (*pb.Session, bool, error) {
	panic("unused")
}

// DismissSessionAlert satisfies the BossClient seam (BOS-1537). This stub is
// not part of any dismiss test.
func (*trashStubClient) DismissSessionAlert(context.Context, string, string) (*pb.Session, error) {
	return nil, nil
}

func TestTrashModel_RestoringRow(t *testing.T) {
	client := &trashStubClient{sessions: trashFixtureSessions()}
	client.sessions[0].DisplaySettingUp = true
	m := newLoadedTrashModelWithClient(client)
	if got := m.View().Content; !strings.Contains(got, "Fix checkout · restoring") {
		t.Fatalf("restoring marker absent: %s", got)
	}
	m = updateTrash(t, m, keyString("d"))
	if m.confirm.Active || !strings.Contains(m.View().Content, "restore in progress") {
		t.Fatal("restoring row must refuse delete and explain why")
	}
	_, cmd := m.Update(keyString("r"))
	if cmd == nil {
		t.Fatal("restoring row must allow reattach")
	}
	opened := cmd().(RestoreStreamOpenedMsg)
	if opened.ID != client.sessions[0].Id || len(client.restoredIDs) != 1 {
		t.Fatal("reattach must open resurrect for selected row")
	}
	client.sessions[0].DisplaySettingUp = false
	m.buildTable()
	if strings.Contains(m.View().Content, "· restoring") {
		t.Fatal("settled row retained restoring marker")
	}
}

type cancellationRestoreClient struct {
	*trashStubClient
	contexts []context.Context
	block    bool
}

func (c *cancellationRestoreClient) ResurrectSession(ctx context.Context, id string) (rpc.ResurrectSessionStream, error) {
	c.contexts = append(c.contexts, ctx)
	if c.block {
		return &cancellationRestoreStream{ctx: ctx}, nil
	}
	return c.trashStubClient.ResurrectSession(ctx, id)
}

type cancellationRestoreStream struct{ ctx context.Context }

func (s *cancellationRestoreStream) Receive() bool                     { <-s.ctx.Done(); return false }
func (s *cancellationRestoreStream) Msg() *pb.ResurrectSessionResponse { return nil }
func (s *cancellationRestoreStream) Err() error                        { return s.ctx.Err() }
func (s *cancellationRestoreStream) Close() error                      { return nil }

func TestTrashModel_EscDetachesBlockedRestore(t *testing.T) {
	client := &cancellationRestoreClient{trashStubClient: &trashStubClient{sessions: trashFixtureSessions()}, block: true}
	m := newLoadedTrashModel(client.sessions)
	m.client = client
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx = ctx
	model, open := m.Update(keyString("r"))
	m = model.(Model)
	model, read := m.Update(open())
	m = model.(Model)
	done := make(chan tea.Msg, 1)
	go func() { done <- read() }()
	m = updateTrash(t, m, keyString("esc"))
	select {
	case msg := <-done:
		model, cmd := m.Update(msg)
		if cmd != nil || model.(Model).err != nil {
			t.Fatal("abandoned stream result must be ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("Esc did not cancel blocked stream receive")
	}
	if !m.Cancelled() {
		t.Fatal("Esc must leave restore")
	}
}

func TestTrashModel_TerminalRestoreCancelsWatcher(t *testing.T) {
	client := &cancellationRestoreClient{trashStubClient: &trashStubClient{sessions: trashFixtureSessions()}}
	m := newLoadedTrashModel(client.sessions)
	m.client = client
	_, final := driveRestore(t, m)
	if final.RestoredSessionID() == "" {
		t.Fatal("restore did not finish")
	}
	if err := client.contexts[0].Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal frame did not cancel watcher: %v", err)
	}
}

func TestTrashModel_DropsAbandonedRestoreFrames(t *testing.T) {
	client := &trashStubClient{sessions: trashFixtureSessions(), resurrectSetupLines: []string{"old progress"}}
	m := newLoadedTrashModelWithClient(client)
	model, open := m.Update(keyString("r"))
	m = model.(Model)
	oldOpened := open().(RestoreStreamOpenedMsg)
	model, read := m.Update(oldOpened)
	m = model.(Model)
	oldProgress := read()
	_, terminal := m.Update(oldProgress)
	oldTerminal := terminal()
	model, open = m.Update(keyString("r"))
	m = model.(Model)
	newOpened := open().(RestoreStreamOpenedMsg)
	model, _ = m.Update(newOpened)
	m = model.(Model)
	for _, msg := range []tea.Msg{oldOpened, oldProgress, oldTerminal} {
		model, cmd := m.Update(msg)
		m = model.(Model)
		if cmd != nil || !m.restoring || m.restoreLine != "" || m.RestoredSessionID() != "" {
			t.Fatalf("stale %T drove newer restore", msg)
		}
	}
}
