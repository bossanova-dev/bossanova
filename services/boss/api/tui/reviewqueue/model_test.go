package reviewqueue

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestQueueCrossRepositorySortAndCounts_Sessions_AC209301(t *testing.T) {
	low := queueRow("low", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 2, 30)
	moderate := queueRow("moderate", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 8, 10)
	blocked := queueRow("blocked", pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED, 4, 20)
	low.Session.TrackerRank = &pb.TrackerRank{Tier: 1}
	moderate.Session.TrackerRank = &pb.TrackerRank{Tier: 4}
	c := &queueScreenClient{repos: []*pb.Repo{{Id: "one", OriginUrl: "https://github.com/test/one"}, {Id: "two", OriginUrl: "https://github.com/test/two"}, {Id: "local"}}, responses: map[string]*pb.ListReviewQueueResponse{
		"one": {Rows: []*pb.ReviewQueueRow{low, blocked}, LevelCounts: []*pb.ReviewQueueLevelCount{{ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, SessionCount: 2}, {ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, SessionCount: 1}}},
		"two": {Rows: []*pb.ReviewQueueRow{moderate}, LevelCounts: []*pb.ReviewQueueLevelCount{{ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, SessionCount: 3}, {ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED, SessionCount: 4}, {ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, SessionCount: 6}}}}}
	m := queueModel(c)
	m = queueRun(t, m, m.load())
	if !reflect.DeepEqual(c.queueCalls, []string{"one", "two"}) {
		t.Fatalf("queue calls: %v", c.queueCalls)
	}
	if !strings.Contains(m.header(), "merging now 5 · in repair 4 · need you 6 · deep review 1") {
		t.Fatalf("counts: %s", m.header())
	}
	if m.Order() != pb.ReviewQueueSort_REVIEW_QUEUE_SORT_PRIORITY || !strings.Contains(m.header(), "order: priority") {
		t.Fatalf("default order: %s", m.header())
	}
	labels := []string{"priority", "most confident", "least confident", "smallest", "oldest", "priority"}
	m.table.SetCursor(1)
	selected := m.selected().GetSession().GetId()
	wants := [][]string{{"low", "moderate", "blocked"}, {"moderate", "low", "blocked"}, {"blocked", "low", "moderate"}, {"low", "blocked", "moderate"}, {"moderate", "blocked", "low"}, {"low", "moderate", "blocked"}}
	for i, want := range wants {
		if !strings.Contains(m.header(), "order: "+labels[i]) {
			t.Errorf("cycle %d header: %s", i, m.header())
		}
		got := []string{}
		for _, r := range m.rows {
			got = append(got, r.GetSession().GetId())
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("order cycle %d: got %v want %v", i, got, want)
		}
		if m.selected().GetSession().GetId() != selected {
			t.Errorf("cycle %d lost selection", i)
		}
		var cmd tea.Cmd
		m, cmd = queueKey(m, 'o')
		if cmd != nil {
			t.Fatal("order unexpectedly fetched rows")
		}
	}
}

func TestQueueHealthAndPartialFailures_Sessions_AC209303(t *testing.T) {
	for _, allFailed := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "all"}[allFailed], func(t *testing.T) {
			c := &queueScreenClient{repos: []*pb.Repo{{Id: "one", DisplayName: "One", OriginUrl: "https://github.com/test/one"}, {Id: "two", DisplayName: "Two", OriginUrl: "https://github.com/test/two"}}, responses: map[string]*pb.ListReviewQueueResponse{"one": {Rows: []*pb.ReviewQueueRow{queueRow("survivor", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10)}}}, queueErrs: map[string]error{"two": errors.New("queue unavailable")}, healthErrs: map[string]error{"one": errors.New("older daemon")}, health: map[string]*pb.GetFactoryHealthResponse{"two": {Problems: []*pb.FactoryHealthProblem{{Text: "worker unavailable"}}}}}
			if allFailed {
				delete(c.responses, "one")
				c.queueErrs["one"] = errors.New("queue unavailable")
			}
			m := queueModel(c)
			m = queueRun(t, m, m.load())
			view := ansi.Strip(m.View().Content)
			if !strings.Contains(view, "Two: queue unavailable") || strings.Contains(view, "No PRs are waiting") {
				t.Fatalf("failed queue shown as empty: %s", view)
			}
			if !allFailed && !strings.Contains(view, "Change survivor") {
				t.Fatalf("healthy repo missing: %s", view)
			}
			if len(m.health) != 1 || m.health[0] != "Two: worker unavailable" {
				t.Fatalf("health: %v", m.health)
			}
		})
	}
}

func TestQueueAuditSizeAndHealthLayout(t *testing.T) {
	audit := queueRow("audit", pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, 0, 10)
	audit.IsAuditSample = true
	audit.Session.PrChangedFileCount = 0
	m := queueModel(&queueScreenClient{}, audit)
	rendered := strings.Join(m.table.Rows()[0], "|")
	if !strings.Contains(rendered, "sample") || !strings.Contains(rendered, "|—|") {
		t.Fatalf("audit and unknown size: %s", rendered)
	}
	rows := make([]*pb.ReviewQueueRow, 50)
	for i := range rows {
		rows[i] = queueRow("row", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10)
	}
	m = queueModel(&queueScreenClient{}, rows...)
	without := m.TableHeight()
	if m.healthLine() != "" {
		t.Fatal("reserved health without problems")
	}
	m.health = []string{strings.Repeat("worker unavailable ", 30)}
	m.buildTable()
	if m.TableHeight() != without-1 {
		t.Fatalf("health line overhead: %d without, %d with", without, m.TableHeight())
	}
	if lipgloss.Height(m.healthLine()) != 1 || lipgloss.Width(m.healthLine()) > m.table.Width() {
		t.Fatal("health strip exceeds one table-width line")
	}
}

func TestQueueResizePreservesVisibleSelection(t *testing.T) {
	rows := make([]*pb.ReviewQueueRow, 60)
	for i := range rows {
		rows[i] = queueRow("row", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10)
	}
	m := queueModel(&queueScreenClient{}, rows...)
	m.table.SetCursor(45)
	columns := func(m Model) map[string]bool {
		out := map[string]bool{}
		for _, c := range m.table.Columns() {
			out[c.Title] = true
		}
		return out
	}
	if !columns(m)["SIZE"] {
		t.Fatal("wide table omitted size")
	}
	m = queueUpdate(m, tea.WindowSizeMsg{Width: 75, Height: 15})
	if columns(m)["SIZE"] {
		t.Fatal("size survived narrow resize")
	}
	if !columns(m)["PR"] || !columns(m)["LEVEL"] {
		t.Fatal("mandatory columns dropped")
	}
	if m.table.Cursor() != 45 {
		t.Fatalf("resize moved cursor: %d", m.table.Cursor())
	}
	start, end := m.table.VisibleRange()
	if start > 45 || end <= 45 {
		t.Fatalf("selected row outside viewport [%d,%d)", start, end)
	}
}

func TestQueueHealthFailureOmitsStripAndKeepsRows(t *testing.T) {
	c := &queueScreenClient{repos: []*pb.Repo{{Id: "one", OriginUrl: "https://github.com/test/one"}}, responses: map[string]*pb.ListReviewQueueResponse{"one": {Rows: []*pb.ReviewQueueRow{queueRow("available", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10)}}}, healthErrs: map[string]error{"one": errors.New("unimplemented")}}
	m := queueModel(c)
	m = queueRun(t, m, m.load())
	if len(m.rows) != 1 || len(m.repoErrs) != 0 || m.err != nil {
		t.Fatalf("health failure disrupted queue: rows=%d errors=%v err=%v", len(m.rows), m.repoErrs, m.err)
	}
	if m.healthLine() != "" {
		t.Fatal("failed health rendered a strip")
	}
}

func TestQueueRefreshPreservesSelectedPR(t *testing.T) {
	a := queueRow("a", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
	b := queueRow("b", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 20)
	m := queueModel(&queueScreenClient{}, a, b)
	m.table.SetCursor(1)
	replacement := queueRow("b", pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, 1, 20)
	replacement.Session.TrackerRank = &pb.TrackerRank{Tier: 1}
	m = queueUpdate(m, LoadedMsg{Rows: []*pb.ReviewQueueRow{a, replacement}})
	if m.selected().GetSession().GetId() != "b" || m.table.Cursor() != 0 {
		t.Fatalf("refresh lost selection: cursor=%d selected=%v", m.table.Cursor(), m.selected())
	}
}

// A parked PR whose chat asks a question keeps its row's asks, proof page and
// wait from its verify park, not from the question its attention now shows.
func TestQueueRowReadsItsParkWhileTheChatAsks_Sessions_AC209301(t *testing.T) {
	row := queueRow("asking", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
	parkedAt := time.Now().Add(-3 * time.Hour)
	row.Session.VerifyPark = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Asks: []string{"Migrations"}, ProofUrl: "https://proof.test/asking", Since: timestamppb.New(parkedAt)}
	row.Session.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION, Since: timestamppb.Now()}
	m := queueModel(&queueScreenClient{}, row)
	rendered := strings.Join(m.table.Rows()[0], "|")
	if !strings.Contains(rendered, "Migrations") || !strings.Contains(rendered, "3h") {
		t.Fatalf("row does not show its park's asks and wait: %s", rendered)
	}
	if !strings.Contains(ansi.Strip(m.footer()), "[p]roof") {
		t.Fatalf("footer misses the park's proof page: %s", m.footer())
	}
}
