package views

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

//go:embed testdata/home-attention-*.golden
var homeAttentionGoldens embed.FS

func TestHomeAttentionBoard(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width, h.height = 120, 50
	h.sessions = []*pb.Session{
		{Id: "progress", Title: "Building feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}},
		{Id: "review", Title: "Review feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Review the diff carefully", Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_APPROVE}}},
		{Id: "fix", Title: "Repair feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Log in again"}},
		{Id: "done", Title: "Finished feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}},
	}
	h.buildTableRows()
	screen := ansi.Strip(h.table.View())
	for _, want := range []string{"NEEDS YOU · 2", "IN PROGRESS · 1", "Review the diff", "building"} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q in:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "Finished feature") || strings.Contains(screen, "◌") {
		t.Fatalf("unexpected finished row or glyph:\n%s", screen)
	}
	if got := h.selectedSessionID(); got != "fix" {
		t.Errorf("selected %s, want fix", got)
	}
}

func TestHomeAttentionGolden(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.width, h.height = width, 60
			h.sessions = fixtures.AttentionSessions()
			h.buildTableRows()
			// Review is the first non-fix row; its action bar must change with selection.
			row, _ := h.tableCursorForSessionID("attention-0")
			h.table.SetCursor(row)
			h.buildTableRows()
			left, nav, quit := h.sessionTableFooterActions()
			screen := ansi.Strip(h.table.View()) + "\n" + strings.Join(append(append(left, nav...), quit...), "  ") + "\n"
			path := filepath.Join("testdata", fmt.Sprintf("home-attention-%d.golden", width))
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(screen), 0600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := homeAttentionGoldens.ReadFile(filepath.ToSlash(path))
			if err != nil {
				t.Fatal(err)
			}
			if screen != string(want) {
				t.Fatalf("board differs from %s:\n%s", path, screen)
			}
			for _, line := range strings.Split(screen, "\n") {
				if ansi.StringWidth(line) > width && !strings.Contains(line, "[n]") {
					t.Errorf("line exceeds %d columns: %q", width, line)
				}
			}
			if h.tableDataRowCount() != len(h.table.Rows()) {
				t.Fatal("height accounting differs from rendered rows")
			}
			for _, r := range h.table.Rows() {
				if len(r) != 5 {
					t.Fatalf("expected five columns, got %d", len(r))
				}
				if r[0] == "" && r[1] == "" && r[3] == "" && r[4] == "" && !strings.Contains(ansi.Strip(r[2]), " · ") {
					if ansi.StringWidth(r[2]) > h.attentionSentenceWidth() {
						t.Fatalf("sentence exceeds NAME: %q", r[2])
					}
				}
			}
		})
	}
}

func TestHomeAttentionOrderingAndSections(t *testing.T) {
	sessions := fixtures.AttentionSessions()
	got := attentionBoard(sessions)
	var ids []string
	for _, s := range got {
		ids = append(ids, s.Id)
	}
	want := []string{"attention-2", "attention-0", "attention-3", "attention-4", "attention-5", "attention-6", "attention-7", "attention-1", "attention-8"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order=%v, want %v", ids, want)
	}
	// A later fix remains above an older question; other Needs you rows sort by since.
	sessions[0].Attention.Since = timestamppb.New(time.Unix(1, 0))
	if attentionBoard(sessions)[0].Id != "attention-2" {
		t.Fatal("fix must sort first")
	}
	for _, kind := range []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS} {
		h := NewHomeModel(nil, context.Background(), nil)
		h.width = 120
		h.height = 30
		h.sessions = []*pb.Session{{Id: "one", Attention: &pb.SessionAttention{Kind: kind, Sentence: "Read this", ProgressLabel: "building"}}}
		h.buildTableRows()
		screen := ansi.Strip(h.table.View())
		if strings.Contains(screen, "NEEDS YOU") == strings.Contains(screen, "IN PROGRESS") {
			t.Fatalf("expected exactly one section: %s", screen)
		}
	}
}

func TestHomeAttentionSentenceAndLegacyHints(t *testing.T) {
	s := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: strings.Repeat("word ", 100)}, SetupError: "stale warning"}
	for _, width := range []int{1, 16, 40, 60} {
		lines := attentionSentenceLines(s, width)
		if len(lines) != 4 || !strings.HasSuffix(lines[3], "…") {
			t.Fatalf("width %d lines=%q", width, lines)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d overflow %q", width, line)
			}
		}
	}
	block := ansi.Strip(selectedSessionWarningBlock(s, nil, 30))
	if !strings.Contains(block, "word") || strings.Contains(block, "stale warning") {
		t.Fatalf("details=%s", block)
	}
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 120
	h.height = 30
	h.sessions = []*pb.Session{s}
	h.buildTableRows()
	if strings.Contains(ansi.Strip(h.table.View()), "stale warning") {
		t.Fatal("attention row leaked legacy warning")
	}
	// With no attention, the composite and warning remain available.
	s.Attention = nil
	s.DisplayLabel = "ready"
	h.buildTableRows()
	screen := ansi.Strip(h.table.View())
	if !strings.Contains(screen, "stale warning") || !strings.Contains(screen, "ready") || strings.Contains(screen, "NEEDS YOU") {
		t.Fatalf("legacy behavior: %s", screen)
	}
}

func TestHomeAttentionFooterAndCursor(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 120
	h.height = 50
	h.sessions = fixtures.AttentionSessions()
	h.buildTableRows()
	left, _, _ := h.sessionTableFooterActions()
	if !strings.Contains(strings.Join(left, " "), "[r] resume [x] dismiss [enter] open the failed chat") {
		t.Fatalf("fix footer=%v", left)
	}
	row, _ := h.tableCursorForSessionID("attention-0")
	h.table.SetCursor(row)
	h.buildTableRows()
	left, _, _ = h.sessionTableFooterActions()
	if want := []string{"[n]ew session", "[y] approve", "[b] send back", "[g] github", "[v] video"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("review footer=%v", left)
	}

	h.table.SetCursor(h.tableCursorForSessionIndex(0))
	for _, want := range []string{"attention-0", "attention-3", "attention-4", "attention-5", "attention-6", "attention-7", "attention-1", "attention-8"} {
		next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		h = next.(HomeModel)
		if got := h.selectedSessionID(); got != want {
			t.Fatalf("down selected %s, want %s", got, want)
		}
	}
	next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	h = next.(HomeModel)
	if got := h.selectedSessionID(); got != "attention-1" {
		t.Fatalf("up selected %s, want attention-1", got)
	}
}

func TestHomeAttentionNotificationEdges(t *testing.T) {
	makeSession := func(kind pb.AttentionKind, key string) *pb.Session {
		return &pb.Session{Id: "one", PrNumber: func() *int32 { n := int32(42); return &n }(), Attention: &pb.SessionAttention{Kind: kind, AlertKey: key, Sentence: "Please approve this change"}}
	}
	progress := makeSession(pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "")
	review := makeSession(pb.AttentionKind_ATTENTION_KIND_REVIEW, "")
	fix := makeSession(pb.AttentionKind_ATTENTION_KIND_FIX, "old")
	changed := proto.CloneOf(fix)
	changed.Attention.AlertKey = "new"
	for _, tc := range []struct {
		name          string
		before, after *pb.Session
		want          int
	}{
		{"enter review", progress, review, 1}, {"same review", review, review, 0}, {"new alert", fix, changed, 1},
		{"same fix", fix, fix, 0}, {"factory merge", progress, makeSession(pb.AttentionKind_ATTENTION_KIND_DONE, ""), 0},
		{"CI progress", progress, progress, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(newlyNeedsYouSessions([]*pb.Session{tc.before}, []*pb.Session{tc.after})); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		kind pb.AttentionKind
		want string
	}{
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, "#42 needs you · review · Please approve this change"},
		{pb.AttentionKind_ATTENTION_KIND_FIX, "#42 needs a fix · Please approve this change"},
		{pb.AttentionKind_ATTENTION_KIND_QUESTION, "Please approve this change"},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, "#42 is ready to merge"},
	} {
		if got := attentionNotificationBody(makeSession(tc.kind, "")); got != tc.want {
			t.Errorf("body=%q want %q", got, tc.want)
		}
	}
}

func TestHomeAttentionStatus(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	for _, tc := range []struct {
		kind pb.AttentionKind
		want string
	}{
		{pb.AttentionKind_ATTENTION_KIND_FIX, "fix"},
		{pb.AttentionKind_ATTENTION_KIND_QUESTION, "? question"},
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, "review"},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, "merge"},
		{pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "building"},
	} {
		s := &pb.Session{DisplayLabel: "passing", DisplaySpinner: true, Attention: &pb.SessionAttention{Kind: tc.kind, ProgressLabel: "building"}}
		if got := ansi.Strip(h.renderSessionStatus(s)); got != tc.want {
			t.Errorf("kind %v: got %q want %q", tc.kind, got, tc.want)
		}
	}
}

func TestHomeAttentionMixedDaemon(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 120
	h.height = 30
	h.sessions = []*pb.Session{
		{Id: "legacy", Title: "Legacy row", DisplayLabel: "working", SetupError: "legacy setup warning"},
		{Id: "review", Title: "Review row", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Read the diff"}},
	}
	h.buildTableRows()
	screen := ansi.Strip(h.table.View())
	for _, want := range []string{"NEEDS YOU · 1", "IN PROGRESS · 1", "working", "legacy setup warning"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("missing %q: %s", want, screen)
		}
	}
	row, _ := h.tableCursorForSessionID("legacy")
	h.table.SetCursor(row)
	h.buildTableRows()
	if h.selectedSessionID() != "legacy" {
		t.Fatal("legacy selection lost after grouping")
	}
}
