package views

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type trashStubClient struct {
	*stubClient

	sessions    []*pb.Session
	removedIDs  []string
	restoredIDs []string
	emptyCalled bool
	failOnID    string // when set, RemoveSession returns an error for this id
}

func (s *trashStubClient) ListSessions(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
	return s.sessions, nil
}

func (s *trashStubClient) RemoveSession(_ context.Context, id string) error {
	if s.failOnID != "" && id == s.failOnID {
		return fmt.Errorf("remove failed for %s", id)
	}
	s.removedIDs = append(s.removedIDs, id)
	return nil
}

func (s *trashStubClient) ResurrectSession(_ context.Context, id string) (*pb.Session, error) {
	s.restoredIDs = append(s.restoredIDs, id)
	return &pb.Session{Id: id}, nil
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
		{name: "archived", query: RelativeTime(trashArchiveTime(0)), wantNames: []string{"Fix checkout", "feature/dark-mode", "Docs refresh"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = m.filter.Activate()
			m.filter.input.SetValue(tc.query)
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

func TestTrashModel_FilterRendersBetweenTableAndActionBar(t *testing.T) {
	m := newLoadedTrashModel(trashFixtureSessions())

	_ = m.filter.Activate()
	m.filter.input.SetValue("checkout")
	m.buildTable()

	view := m.View().Content
	assertAppearsBefore(t, view, "Fix checkout", "(1 of 3)")
	assertAppearsBefore(t, view, "(1 of 3)", "type to filter")
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
	client := &trashStubClient{stubClient: &stubClient{}, sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("docs")
	_ = m.filter.Commit()
	m.buildTable()

	model, cmd := m.Update(keyString("r"))
	m = model.(TrashModel)
	if !m.restoring {
		t.Fatal("model should enter restoring state")
	}
	msg := cmd().(sessionRestoredMsg)
	if msg.id != "sess-3" {
		t.Fatalf("restored id = %q, want sess-3", msg.id)
	}
	if got := client.restoredIDs; len(got) != 1 || got[0] != "sess-3" {
		t.Fatalf("client restored IDs = %v, want [sess-3]", got)
	}
}

func TestTrashModel_DeleteConfirmationUsesSelectedFilteredSession(t *testing.T) {
	client := &trashStubClient{stubClient: &stubClient{}, sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("docs")
	_ = m.filter.Commit()
	m.buildTable()

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(TrashModel)
	if !m.deleting {
		t.Fatal("model should enter deleting state")
	}
	msg := cmd().(sessionDeletedMsg)
	if msg.id != "sess-3" {
		t.Fatalf("deleted id = %q, want sess-3", msg.id)
	}
	if got := client.removedIDs; len(got) != 1 || got[0] != "sess-3" {
		t.Fatalf("client removed IDs = %v, want [sess-3]", got)
	}
}

func TestTrashModel_FilteredDeleteAllDeletesOnlyMatchedSessions(t *testing.T) {
	client := &trashStubClient{stubClient: &stubClient{}, sessions: trashFixtureSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("docs")
	_ = m.filter.Commit()
	m.buildTable()

	m = updateTrash(t, m, keyString("a"))
	if !m.confirmingAll {
		t.Fatal("model should confirm filtered delete all")
	}
	if got := m.View().Content; !strings.Contains(got, "Permanently delete 1 filtered archived sessions?") {
		t.Fatalf("confirmation copy missing filtered scope: %q", got)
	}

	m.filter.input.SetValue("payments")
	m.buildTable()

	model, cmd := m.Update(keyString("enter"))
	m = model.(TrashModel)
	msg := cmd().(allSessionsDeletedMsg)
	m = updateTrashMsg(t, m, msg)

	if client.emptyCalled {
		t.Fatal("filtered delete all should not call EmptyTrash")
	}
	if got := fmt.Sprint(client.removedIDs); got != "[sess-3]" {
		t.Fatalf("removed IDs = %v, want [sess-3]", client.removedIDs)
	}
	if got := trashSessionIDs(m.sessions); fmt.Sprint(got) != "[sess-1 sess-2]" {
		t.Fatalf("remaining sessions = %v, want [sess-1 sess-2]", got)
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
		stubClient: &stubClient{},
		sessions:   trashSharedRepoSessions(),
		failOnID:   "sess-b",
	}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("alpha")
	_ = m.filter.Commit()
	m.buildTable()
	if got := len(m.table.Rows()); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}

	m = updateTrash(t, m, keyString("a"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(TrashModel)
	msg := cmd().(allSessionsDeletedMsg)
	if msg.err == nil {
		t.Fatal("batch delete should report the mid-loop failure")
	}
	m = updateTrashMsg(t, m, msg)

	if client.emptyCalled {
		t.Fatal("filtered delete all should not call EmptyTrash")
	}
	if got := fmt.Sprint(client.removedIDs); got != "[sess-a]" {
		t.Fatalf("removed IDs = %v, want [sess-a] (stops at first failure)", client.removedIDs)
	}
	if m.err == nil {
		t.Fatal("model should surface the delete error")
	}
	if got := trashSessionIDs(m.sessions); fmt.Sprint(got) != "[sess-b sess-c]" {
		t.Fatalf("remaining sessions = %v, want [sess-b sess-c] (only succeeded pruned)", got)
	}
}

func TestTrashModel_EscCancelsErrorScreenWithAppliedFilter(t *testing.T) {
	client := &trashStubClient{
		stubClient: &stubClient{},
		sessions:   trashSharedRepoSessions(),
		failOnID:   "sess-a",
	}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("alpha")
	_ = m.filter.Commit()
	m.buildTable()
	if !m.filter.Applied() {
		t.Fatal("filter should be applied before failed delete")
	}

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(TrashModel)
	msg := cmd().(sessionDeletedMsg)
	if msg.err == nil {
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
	client := &trashStubClient{stubClient: &stubClient{}, sessions: trashSharedRepoSessions()}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("alpha")
	_ = m.filter.Commit()
	m.buildTable()

	m.table.SetCursor(2)
	if got := m.selectedSession(); got == nil || got.Id != "sess-c" {
		t.Fatalf("pre-delete selection = %v, want sess-c", got)
	}

	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(TrashModel)
	msg := cmd().(sessionDeletedMsg)
	if msg.id != "sess-c" {
		t.Fatalf("deleted id = %q, want sess-c", msg.id)
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
	client := &trashStubClient{stubClient: &stubClient{}, sessions: sessions}
	m := newLoadedTrashModelWithClient(client)

	_ = m.filter.Activate()
	m.filter.input.SetValue("solo")
	_ = m.filter.Commit()
	m.buildTable()
	if !m.filter.Applied() {
		t.Fatal("filter should be applied")
	}

	// Delete the only session; the model should drop the now-meaningless filter.
	m = updateTrash(t, m, keyString("d"))
	model, cmd := m.Update(keyString("enter"))
	m = model.(TrashModel)
	msg := cmd().(sessionDeletedMsg)
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
	m.filter.input.SetValue("no-match")
	m.buildTable()

	if got := len(m.table.Rows()); got != 0 {
		t.Fatalf("rows = %d, want 0", got)
	}
	view := m.View().Content
	if !strings.Contains(view, "No archived sessions match filter.") {
		t.Fatalf("view missing no-match state: %q", view)
	}
	assertAppearsBefore(t, view, "No archived sessions match filter.", "(0 of 3)")
	assertAppearsBefore(t, view, "(0 of 3)", "type to filter")
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

func newLoadedTrashModel(sessions []*pb.Session) TrashModel {
	return newLoadedTrashModelWithClient(&trashStubClient{stubClient: &stubClient{}, sessions: sessions})
}

func newLoadedTrashModelWithClient(client *trashStubClient) TrashModel {
	m := NewTrashModel(client, context.Background())
	m.loading = false
	m.sessions = client.sessions
	m.width = 120
	m.height = 40
	m.buildTable()
	return m
}

func updateTrash(t *testing.T, m TrashModel, msg tea.Msg) TrashModel {
	t.Helper()
	return updateTrashMsg(t, m, msg)
}

func updateTrashMsg(t *testing.T, m TrashModel, msg tea.Msg) TrashModel {
	t.Helper()
	model, _ := m.Update(msg)
	got, ok := model.(TrashModel)
	if !ok {
		t.Fatalf("model type = %T, want TrashModel", model)
	}
	return got
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
