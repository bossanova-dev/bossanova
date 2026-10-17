package views

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"
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
		{Id: "review", Title: "Review feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Database migration", Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}}},
		{Id: "fix", Title: "Repair feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Log in again"}},
		{Id: "done", Title: "Finished feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}},
	}
	h.buildTableRows()
	screen := ansi.Strip(h.table.View())
	for _, want := range []string{"NEEDS YOU", "IN PROGRESS", "Database migration", "building", "Finished feature"} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q in:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "◌") {
		t.Fatalf("unexpected glyph:\n%s", screen)
	}
	if got := h.selectedSessionID(); got != "fix" {
		t.Errorf("selected %s, want fix", got)
	}
}

// TestHomeAttentionBoardKeepsFinishedRowsStruckThrough pins the board's
// pre-attention treatment of a resolved PR: a session whose PR closed or merged
// stays in IN PROGRESS, struck through, and only archiving removes it. Hiding
// it lost a session whose chat was still in use after its PR closed.
func TestHomeAttentionBoardKeepsFinishedRowsStruckThrough(t *testing.T) {
	done := &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}
	muted := pb.DisplayIntent_DISPLAY_INTENT_MUTED
	h := NewHomeModel(nil, context.Background(), nil)
	h.width, h.height = 120, 50
	h.sessions = []*pb.Session{
		{Id: "review", Title: "Review feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Database migration"}},
		{Id: "progress", Title: "Building feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}},
		{Id: "closed", Title: "Closed PR feature", PrNumber: proto.Int32(6395), Attention: done, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED, DisplayLabel: "closed", DisplayIntent: muted},
		{Id: "merged", Title: "Merged PR feature", PrNumber: proto.Int32(6400), Attention: done, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED, DisplayLabel: "✓ merged", DisplayIntent: muted},
		{Id: "archived", Title: "Archived feature", PrNumber: proto.Int32(6401), Attention: done, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED, DisplayLabel: "✓ merged", ArchivedAt: timestamppb.Now()},
	}
	h.buildTableRows()

	screen := ansi.Strip(h.table.View())
	if strings.Contains(screen, "Archived feature") {
		t.Fatalf("archived session rendered:\n%s", screen)
	}
	progressAt := strings.Index(screen, "IN PROGRESS")
	if progressAt < 0 || !strings.Contains(screen, "NEEDS YOU") {
		t.Fatalf("want NEEDS YOU then IN PROGRESS:\n%s", screen)
	}
	rowFor := func(title string) string {
		for _, r := range h.table.Rows() {
			if strings.Contains(ansi.Strip(r[2]), title) {
				return strings.Join(r, " ")
			}
		}
		return ""
	}
	for title, status := range map[string]string{"Closed PR feature": "closed", "Merged PR feature": "✓ merged"} {
		if at := strings.Index(screen, title); at < progressAt {
			t.Errorf("%q rendered at %d, want under IN PROGRESS at %d:\n%s", title, at, progressAt, screen)
		}
		row := rowFor(title)
		if !strings.Contains(row, ";9m") {
			t.Errorf("%q row is not struck through: %q", title, row)
		}
		if !strings.Contains(ansi.Strip(row), status) {
			t.Errorf("%q row = %q, want status %q", title, ansi.Strip(row), status)
		}
	}
	if row := rowFor("Building feature"); strings.Contains(row, ";9m") {
		t.Errorf("live row is struck through: %q", row)
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
			screen := ansi.Strip(h.boardView()) + "\n" + actionBarText(left, nav, quit) + "\n"
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
				if r[0] == "" && r[1] == "" && r[3] == "" && r[4] == "" {
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
	want := []string{"attention-2", "attention-0", "attention-3", "attention-4", "attention-5", "attention-6", "attention-7", "attention-1", "attention-8", "attention-9"}
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
		if strings.Contains(screen, "NEEDS YOU") || strings.Contains(screen, "IN PROGRESS") {
			t.Fatalf("a one-group board must not title its group: %s", screen)
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
	if block := (ChatPickerModel{session: s}).legacyWarningBlock(); block != "" {
		t.Fatalf("details view rendered legacy hints beside attention: %q", ansi.Strip(block))
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
	if want := []string{"[n]ew session", "[enter] select", "[/] filter"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("fix footer=%v", left)
	}
	row, _ := h.tableCursorForSessionID("attention-0")
	h.table.SetCursor(row)
	h.buildTableRows()
	left, _, _ = h.sessionTableFooterActions()
	if want := []string{"[n]ew session", "[enter] select", "[v] video", "[m] merge", "[/] filter"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("review footer=%v", left)
	}

	h.table.SetCursor(h.tableCursorForSessionIndex(0))
	for _, want := range []string{"attention-0", "attention-3", "attention-4", "attention-5", "attention-6", "attention-7", "attention-1", "attention-8", "attention-9"} {
		next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		h = next.(HomeModel)
		if got := h.selectedSessionID(); got != want {
			t.Fatalf("down selected %s, want %s", got, want)
		}
	}
	next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	h = next.(HomeModel)
	if got := h.selectedSessionID(); got != "attention-8" {
		t.Fatalf("up selected %s, want attention-8", got)
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
			if got := len(newlyNeedsYouSessions([]*pb.Session{tc.before}, []*pb.Session{tc.after}, nil)); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		kind pb.AttentionKind
		want string
	}{
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, "#42 needs you · Please approve this change"},
		{pb.AttentionKind_ATTENTION_KIND_FIX, "#42 needs a fix · Please approve this change"},
		{pb.AttentionKind_ATTENTION_KIND_QUESTION, "Please approve this change"},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, "#42 is ready to merge"},
	} {
		if got := attentionNotificationBody(makeSession(tc.kind, "")); got != tc.want {
			t.Errorf("body=%q want %q", got, tc.want)
		}
	}
}

func TestHomeAttentionNotifiesAParkOnlyIfTheBoardHasNotShownIt(t *testing.T) {
	session := func(att *pb.SessionAttention) []*pb.Session {
		n := int32(42)
		return []*pb.Session{{Id: "one", PrNumber: &n, Attention: att}}
	}
	park := func(codes ...string) []*pb.Session {
		return session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Review left must-fix findings open · send it back to fix them", ParkCodes: codes})
	}
	signOff := session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Watch the evidence video, then approve", ParkCodes: []string{"human-review:ui"}})
	repairFailed := session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Automatic repair failed", AlertKey: "repair:failed"})
	progress := session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS})
	question := session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION, Sentence: "Which port?"})

	type poll struct {
		sessions   []*pb.Session
		wantNotify bool
	}
	for _, tc := range []struct {
		name  string
		polls []poll
	}{
		{"dismissing a repair failure on a parked PR", []poll{{park("ledger-open"), true}, {repairFailed, true}, {park("ledger-open"), false}}},
		{"a question cleared over a parked PR", []poll{{park("ledger-open"), true}, {question, false}, {repairFailed, true}, {park("ledger-open"), false}}},
		{"a park that arose under the alert", []poll{{progress, false}, {repairFailed, true}, {park("ledger-open"), true}}},
		{"a sign-off that arose under the alert", []poll{{progress, false}, {repairFailed, true}, {signOff, true}}},
		{"the park cleared and returned under the alert", []poll{{park("ledger-open"), true}, {progress, false}, {repairFailed, true}, {park("ledger-open"), true}}},
		{"a different park under the alert", []poll{{park("ledger-open"), true}, {repairFailed, true}, {park("ledger-open", "no-receipt"), true}}},
		{"the park changing while it stays shown", []poll{{park("ledger-open"), true}, {park("ledger-open", "no-receipt"), false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			for i, p := range tc.polls {
				notified := h.notifyNewNeedsYou(p.sessions) != nil
				h.sessions = p.sessions
				if notified != p.wantNotify {
					t.Fatalf("poll %d (%s): notified=%v, want %v", i, p.sessions[0].GetAttention().GetKind(), notified, p.wantNotify)
				}
			}
		})
	}
}

func TestHomeAttentionStatus(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	for _, tc := range []struct {
		kind       pb.AttentionKind
		unverified bool
		want       string
	}{
		{pb.AttentionKind_ATTENTION_KIND_QUESTION, false, "? question"},
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, false, "review"},
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, true, "review"},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, false, "✓ merge"},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, true, "✓ ready"},
		{pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, false, ansi.Strip(h.spinner.View()) + "building"},
	} {
		s := &pb.Session{DisplayLabel: "passing", DisplaySpinner: true, Attention: &pb.SessionAttention{Kind: tc.kind, ProgressLabel: "building", IsUnverified: tc.unverified}}
		if got := ansi.Strip(h.renderSessionStatus(s)); got != tc.want {
			t.Errorf("kind %v unverified %v: got %q want %q", tc.kind, tc.unverified, got, tc.want)
		}
	}
}

// TestHomeMergeStatusSpinsWhileAChatIsWorking pins the merge cell's glyph. An
// unverified "ready" shows the spinner, in the success colour, while the
// session's served display label says a chat is working, and the check mark
// otherwise. A verified "merge" always keeps its check mark.
func TestHomeMergeStatusSpinsWhileAChatIsWorking(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	spin := ansi.Strip(h.spinner.View())
	for _, tc := range []struct {
		name         string
		unverified   bool
		displayLabel string
		want         string
	}{
		{"unverified with a working chat", true, "working", spin + "ready"},
		{"verified with a working chat", false, "working", "✓ merge"},
		{"unverified with no working chat", true, "✓ passing", "✓ ready"},
		{"verified with no working chat", false, "✓ passing", "✓ merge"},
		{"unverified with an idle chat", true, "idle", "✓ ready"},
		{"verified with a waiting chat", false, "waiting", "✓ merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &pb.Session{DisplayLabel: tc.displayLabel, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, IsUnverified: tc.unverified}}
			got := h.renderSessionStatus(s)
			if ansi.Strip(got) != tc.want {
				t.Fatalf("status = %q, want %q", ansi.Strip(got), tc.want)
			}
			if want := styleStatusSuccess.Render(ansi.Strip(got)); got != want {
				t.Fatalf("status = %q, want the success colour %q", got, want)
			}
		})
	}
}

// TestHomeOutcomeStatusSpinsInPlaceOfItsGlyphWhileAChatIsWorking pins the one
// rule behind every attention cell that names an outcome: while a chat works,
// the spinner stands where the glyph was (or opens a bare word), in the cell's
// own colour. A question keeps its "?" because the agent waits on the operator.
func TestHomeOutcomeStatusSpinsInPlaceOfItsGlyphWhileAChatIsWorking(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	spin := ansi.Strip(h.spinner.View())
	for _, tc := range []struct {
		kind    pb.AttentionKind
		working string
		resting string
		style   func(...string) string
	}{
		{pb.AttentionKind_ATTENTION_KIND_FIX, spin + "fix", "fix", styleStatusDanger.Render},
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, spin + "review", "review", styleStatusWarning.Render},
		{pb.AttentionKind_ATTENTION_KIND_QUESTION, "? question", "? question", styleStatusWarning.Render},
	} {
		for _, working := range []bool{true, false} {
			want, label := tc.resting, "passing"
			if working {
				want, label = tc.working, "working"
			}
			s := &pb.Session{DisplayLabel: label, Attention: &pb.SessionAttention{Kind: tc.kind}}
			got := h.renderSessionStatus(s)
			if ansi.Strip(got) != want || got != tc.style(want) {
				t.Errorf("%v working=%v: got %q, want %q in its own colour", tc.kind, working, got, want)
			}
		}
	}
}

// TestHomeWorkingMergeRowStaysInNeedsYouAndKeepsItsFilterText keeps the row in
// NEEDS YOU and matches the filter by the resting "ready" text, not a frame of
// the spinner.
func TestHomeWorkingMergeRowStaysInNeedsYouAndKeepsItsFilterText(t *testing.T) {
	working := &pb.Session{Id: "w", Title: "Still working", DisplayLabel: "working", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, IsUnverified: true}}
	other := &pb.Session{Id: "o", Title: "Building", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}}
	h := NewHomeModel(nil, context.Background(), nil)
	h.width, h.height = 120, 30
	h.sessions = []*pb.Session{other, working}
	h.buildTableRows()

	screen := ansi.Strip(h.boardView())
	needs, progress := strings.Index(screen, "NEEDS YOU"), strings.Index(screen, "IN PROGRESS")
	row := strings.Index(screen, "Still working")
	if needs < 0 || progress < 0 || row < needs || row > progress {
		t.Fatalf("working merge row is not under NEEDS YOU:\n%s", screen)
	}
	if !strings.Contains(screen, ansi.Strip(h.spinner.View())+"ready") || strings.Contains(screen, "✓ ready") {
		t.Fatalf("working merge row does not show the spinner in place of the check:\n%s", screen)
	}
	if hay := homeFilterHaystack(working); !strings.Contains(hay, "✓ ready") {
		t.Fatalf("filter haystack %q lost the resting merge text", hay)
	}
}

// TestHomeArchivingAndMergingOverrideTheAttentionStatus pins the served
// "archiving" and "merging" labels over every attention cell: the archive or
// merge in flight ends whatever the board says the row needs. Each shows its
// spinner in the colour bossd serves, and the row stays in its group.
func TestHomeArchivingAndMergingOverrideTheAttentionStatus(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	spin := ansi.Strip(h.spinner.View())
	served := []struct {
		label  string
		intent pb.DisplayIntent
		style  func(...string) string
	}{
		{"archiving", pb.DisplayIntent_DISPLAY_INTENT_WARNING, styleStatusWarning.Render},
		{"merging", pb.DisplayIntent_DISPLAY_INTENT_INFO, styleStatusInfo.Render},
	}
	attentions := map[string]*pb.SessionAttention{
		"ready":       {Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, IsUnverified: true},
		"merge":       {Kind: pb.AttentionKind_ATTENTION_KIND_MERGE},
		"review":      {Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW},
		"fix":         {Kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		"question":    {Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION},
		"in progress": {Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "idle"},
	}
	for _, sv := range served {
		for name, a := range attentions {
			s := &pb.Session{DisplayLabel: sv.label, DisplayIntent: sv.intent, DisplaySpinner: true, Attention: a}
			want := spin + sv.label
			if got := h.renderSessionStatus(s); got != sv.style(want) {
				t.Errorf("%s row while %s: status = %q, want %q", name, sv.label, ansi.Strip(got), want)
			}
			if hay := homeFilterHaystack(s); !strings.Contains(hay, sv.label) {
				t.Errorf("%s row while %s: filter haystack %q lacks the shown label", name, sv.label, hay)
			}
		}
	}

	archiving := &pb.Session{Id: "a", Title: "Being archived", DisplayLabel: "archiving", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, DisplaySpinner: true, Attention: attentions["ready"]}
	other := &pb.Session{Id: "o", Title: "Building", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}}
	h.width, h.height = 120, 30
	h.sessions = []*pb.Session{other, archiving}
	h.buildTableRows()
	screen := ansi.Strip(h.boardView())
	needs, progress, row := strings.Index(screen, "NEEDS YOU"), strings.Index(screen, "IN PROGRESS"), strings.Index(screen, "Being archived")
	if needs < 0 || progress < 0 || row < needs || row > progress {
		t.Fatalf("archiving ready row left NEEDS YOU:\n%s", screen)
	}
	if !strings.Contains(screen, spin+"archiving") || strings.Contains(screen, "ready") {
		t.Fatalf("archiving ready row does not read archiving:\n%s", screen)
	}
}

// TestHomeReadyRowReadsMergingThenArchivingThenLeaves walks a ready row through
// a merge bossd runs (started from this TUI or any other client) and the
// archive that follows it, as the session polls serve it.
func TestHomeReadyRowReadsMergingThenArchivingThenLeaves(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width, h.height = 120, 30
	spin := ansi.Strip(h.spinner.View())
	ready := &pb.Session{Id: "r", Title: "Sign releases", DisplayLabel: "✓ ready", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, IsUnverified: true}}
	other := &pb.Session{Id: "o", Title: "Building", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}}
	poll := func(mutate func(*pb.Session)) string {
		s := proto.Clone(ready).(*pb.Session)
		mutate(s)
		h.sessions = []*pb.Session{other, s}
		h.buildTableRows()
		return ansi.Strip(h.boardView())
	}
	for _, step := range []struct {
		name   string
		mutate func(*pb.Session)
		want   string
	}{
		{"ready", func(*pb.Session) {}, "✓ ready"},
		{"merging", func(s *pb.Session) {
			s.DisplayMerging, s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = true, "merging", pb.DisplayIntent_DISPLAY_INTENT_INFO, true
		}, spin + "merging"},
		{"archiving", func(s *pb.Session) {
			s.ArchivePending, s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = true, "archiving", pb.DisplayIntent_DISPLAY_INTENT_WARNING, true
		}, spin + "archiving"},
	} {
		screen := poll(step.mutate)
		line := ""
		for _, l := range strings.Split(screen, "\n") {
			if strings.Contains(l, "Sign releases") {
				line = l
			}
		}
		if !strings.Contains(line, step.want) || (step.name != "ready" && strings.Contains(line, "ready")) {
			t.Fatalf("%s: row %q, want it to read %q:\n%s", step.name, line, step.want, screen)
		}
		if needs, row := strings.Index(screen, "NEEDS YOU"), strings.Index(screen, "Sign releases"); needs < 0 || row < needs || row > strings.Index(screen, "IN PROGRESS") {
			t.Fatalf("%s: row left NEEDS YOU:\n%s", step.name, screen)
		}
	}
	if screen := poll(func(s *pb.Session) { s.ArchivedAt = timestamppb.Now() }); strings.Contains(screen, "Sign releases") {
		t.Fatalf("archived row still on the board:\n%s", screen)
	}
}

// TestHomeIdleAttentionRowsKeepTheirWords guards the other side of the
// override: a row whose served label is anything but archiving or merging,
// including the other live labels, keeps the board's words.
func TestHomeIdleAttentionRowsKeepTheirWords(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	for _, label := range []string{"✓ passing", "idle", "initializing", "repairing", "waiting", "usage-limited", "? question"} {
		for want, a := range map[string]*pb.SessionAttention{
			"✓ ready": {Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, IsUnverified: true},
			"review":  {Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW},
			"fix":     {Kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		} {
			s := &pb.Session{DisplayLabel: label, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Attention: a}
			if got := ansi.Strip(h.renderSessionStatus(s)); got != want {
				t.Errorf("%s row with served label %q: status = %q, want %q", want, label, got, want)
			}
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
	for _, want := range []string{"NEEDS YOU", "IN PROGRESS", "working", "legacy setup warning"} {
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

// TestHomeReadyRowInARepoWithNoVerifyStage renders an unverified MERGE (a repo
// with no running verify stage) as a plain "✓ ready" row: no "merge" label, no
// "Not verified" warning, and no subline restating it.
func TestHomeReadyRowInARepoWithNoVerifyStage(t *testing.T) {
	ready := &pb.Session{Id: "ready", Title: "Fresh claim", Attention: &pb.SessionAttention{
		Kind:         pb.AttentionKind_ATTENTION_KIND_MERGE,
		Sentence:     "Ready to merge",
		Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
		IsUnverified: true,
	}}
	h := NewHomeModel(nil, context.Background(), nil)
	h.width, h.height = 120, 30
	h.sessions = []*pb.Session{ready}
	h.buildTableRows()

	screen := ansi.Strip(h.table.View())
	if !strings.Contains(screen, "✓ ready") {
		t.Fatalf("ready status missing:\n%s", screen)
	}
	for _, unwanted := range []string{"✓ merge", "Not verified", "Ready to merge"} {
		if strings.Contains(screen, unwanted) {
			t.Fatalf("ready row shows %q:\n%s", unwanted, screen)
		}
	}
	if got := len(h.table.Rows()); got != 1 {
		t.Fatalf("rows = %d, want the one primary row and no subline", got)
	}
}

func TestHomeMergeRowDropsSentenceThatRestatesItsStatus(t *testing.T) {
	merge := &pb.Session{Id: "merge", Title: "Merge feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, Sentence: "Ready to merge"}}
	review := &pb.Session{Id: "review", Title: "Review feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Database migration"}}
	h := NewHomeModel(nil, context.Background(), nil)
	h.width, h.height = 120, 30
	h.sessions = []*pb.Session{merge, review}
	h.buildTableRows()

	screen := ansi.Strip(h.table.View())
	if strings.Contains(screen, "Ready to merge") {
		t.Fatalf("merge row repeats its status as a subline:\n%s", screen)
	}
	if !strings.Contains(screen, "Database migration") || !strings.Contains(screen, "✓ merge") {
		t.Fatalf("review sentence or merge status missing:\n%s", screen)
	}
	if got := len(h.table.Rows()); got != 3 {
		t.Fatalf("rows = %d, want 3: two primary rows and the review subline", got)
	}
	if h.tableDataRowCount() != len(h.table.Rows()) {
		t.Fatalf("height accounting %d differs from rendered rows %d", h.tableDataRowCount(), len(h.table.Rows()))
	}
	if got := h.selectedSessionID(); got != "merge" {
		t.Fatalf("selected %q, want the merge row first", got)
	}
	next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	h = next.(HomeModel)
	if got := h.selectedSessionID(); got != "review" || h.table.Cursor() != 1 {
		t.Fatalf("down from the one-row merge entry selected %q at row %d, want review at row 1", got, h.table.Cursor())
	}
	details := ChatPickerModel{session: merge, timeline: []*pb.TimelineStep{{Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}}
	if block := ansi.Strip(details.timelineBlock()); !strings.Contains(block, "Ready to merge") {
		t.Fatalf("details view lost the merge sentence: %q", block)
	}
}

func TestHomeFixRowShowsTheStateThatNeedsFixing(t *testing.T) {
	const (
		danger  = pb.DisplayIntent_DISPLAY_INTENT_DANGER
		blocked = pb.SessionState_SESSION_STATE_BLOCKED
	)
	fix := styleStatusDanger.Render("fix")
	for _, tc := range []struct {
		name   string
		label  string
		intent pb.DisplayIntent
		state  pb.SessionState
		pr     pb.DisplayStatus
		codes  []string
		want   string
	}{
		{"failing checks", "⨯ failing", danger, 0, pb.DisplayStatus_DISPLAY_STATUS_FAILING, nil, styleStatusDanger.Render("⨯ failing")},
		{"blocked verify park", displaystatus.BlockedLabel, danger, 0, pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, nil, styleStatusDanger.Render("blocked")},
		{"merge conflict", "⨯ conflict", danger, 0, pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, nil, styleStatusDanger.Render("⨯ conflict")},
		{"changes requested", "⨯ rejected", danger, 0, pb.DisplayStatus_DISPLAY_STATUS_REJECTED, nil, styleStatusDanger.Render("⨯ rejected")},
		{"failing checks on a blocked run", "⨯ failing", danger, blocked, pb.DisplayStatus_DISPLAY_STATUS_FAILING, nil, styleStatusDanger.Render("⨯ failing")},
		{"daemon without a display label", "", pb.DisplayIntent_DISPLAY_INTENT_UNSPECIFIED, 0, 0, nil, fix},
		{"idle", "idle", pb.DisplayIntent_DISPLAY_INTENT_WARNING, 0, 0, nil, fix},
		{"waiting", displaystatus.WaitingLabel, pb.DisplayIntent_DISPLAY_INTENT_INFO, 0, 0, nil, fix},
		{"repairing", "repairing", pb.DisplayIntent_DISPLAY_INTENT_WARNING, 0, 0, nil, fix},
		// A merge in flight ends the fix, so the row reads it.
		{"merging", "merging", pb.DisplayIntent_DISPLAY_INTENT_INFO, 0, 0, nil, styleStatusInfo.Render("merging")},
		// A rotation-incident FIX on a green PR: the label is not the problem.
		{"passing checks", "✓ passing", pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, 0, pb.DisplayStatus_DISPLAY_STATUS_PASSING, nil, fix},
		{"muted draft", "draft", pb.DisplayIntent_DISPLAY_INTENT_MUTED, 0, pb.DisplayStatus_DISPLAY_STATUS_DRAFT, nil, fix},
		// The errored recolor serves a blocked run's every state as DANGER.
		{"idle blocked run", "idle", danger, blocked, 0, nil, fix},
		{"passing checks on a blocked run", "✓ passing", danger, blocked, pb.DisplayStatus_DISPLAY_STATUS_PASSING, nil, fix},
		{"waiting orphaned run", displaystatus.WaitingLabel, danger, pb.SessionState_SESSION_STATE_ORPHANED, 0, nil, fix},
		// A park that only asks for a sign-off is not the failure on a blocked
		// run; a park with a failure code is.
		{"sign-off verify park on a blocked run", displaystatus.NeedsSignOffLabel, danger, blocked, pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, []string{"human-review:ui"}, fix},
		{"blocked verify park on a blocked run", displaystatus.BlockedLabel, danger, blocked, pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, []string{"ledger-open"}, styleStatusDanger.Render("blocked")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.width, h.height = 120, 30
			s := &pb.Session{Id: "fix", Title: "Repair feature", DisplayLabel: tc.label, DisplayIntent: tc.intent, State: tc.state, DisplayStatus: tc.pr, VerifyParkCodes: tc.codes, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Run stopped"}}
			if got := h.renderSessionStatus(s); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
			h.sessions = []*pb.Session{s}
			h.buildTableRows()
			if screen := ansi.Strip(h.table.View()); !strings.Contains(screen, "Run stopped") {
				t.Fatalf("fix row lost the sentence that says what to do:\n%s", screen)
			}
		})
	}
}

func TestHomeSignOffParkReadsReview(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	s := &pb.Session{DisplayLabel: displaystatus.NeedsSignOffLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	if got, want := h.renderSessionStatus(s), styleStatusWarning.Render("review"); got != want {
		t.Fatalf("sign-off park = %q, want the review cell %q", got, want)
	}
	review := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW}}
	if got, want := h.renderSessionStatus(s), h.renderSessionStatus(review); got != want {
		t.Fatalf("sign-off park %q differs from the attention review cell %q", got, want)
	}
}

func TestHomeProgressStatusKeepsDisplayStyleAndSpinner(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	for _, tc := range []struct {
		label  string
		legacy *pb.Session
	}{
		{"working", &pb.Session{DisplayLabel: "working", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, DisplaySpinner: true}},
		{"building", &pb.Session{DisplayLabel: "building", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, DisplaySpinner: true}},
		{"checking 13/15", &pb.Session{DisplayLabel: "checking 13/15", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, DisplaySpinner: true}},
		{"waiting", &pb.Session{DisplayLabel: "waiting", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO, DisplaySpinner: true}},
		{"idle", &pb.Session{DisplayLabel: "idle", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}},
	} {
		s := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: tc.label}}
		got := h.renderSessionStatus(s)
		if want := renderDisplayStatus(tc.legacy, h.spinner); got != want {
			t.Errorf("%s renders %q, want the display status styling %q", tc.label, got, want)
		}
		if strings.Contains(got, "\x1b[2m") {
			t.Errorf("%s renders faint: %q", tc.label, got)
		}
	}

	// The style is chosen from the label text, so build the labels where the
	// daemon builds them: a reword in attention.progressLabel must fail here,
	// not silently recolour the row.
	for _, tc := range []struct {
		name   string
		in     attention.Input
		intent pb.DisplayIntent
		spin   bool
	}{
		{"running checks", attention.Input{HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 7, ChecksDone: 3}, pb.DisplayIntent_DISPLAY_INTENT_WARNING, true},
		{"waiting on main CI", attention.Input{ParkCodes: []string{"base-ci-red"}}, pb.DisplayIntent_DISPLAY_INTENT_INFO, true},
		{"waiting on the test stack", attention.Input{ParkCodes: []string{"waiting: stack-busy"}}, pb.DisplayIntent_DISPLAY_INTENT_INFO, true},
		{"stopped", attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED}, pb.DisplayIntent_DISPLAY_INTENT_MUTED, false},
		{"idle", attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}, pb.DisplayIntent_DISPLAY_INTENT_WARNING, false},
		{"working", attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING}, pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, true},
	} {
		att := attention.Compute(tc.in)
		if att.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
			t.Fatalf("%s: Compute answered %v, want IN_PROGRESS", tc.name, att.GetKind())
		}
		got := h.renderSessionStatus(&pb.Session{Attention: att})
		label := att.GetProgressLabel()
		if tc.spin {
			label = h.spinner.View() + label
		}
		if want := styleForIntent(tc.intent).Render(ansi.Truncate(label, homeStatusWidth, "…")); got != want {
			t.Errorf("%s: Compute's label %q renders %q, want %v styling %q", tc.name, att.GetProgressLabel(), got, tc.intent, want)
		}
	}

	h.width, h.height = 120, 30
	labels := []string{"working", "checking 13/15"}
	h.sessions = nil
	for _, label := range labels {
		h.sessions = append(h.sessions, &pb.Session{Id: label, Title: label, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: label}})
	}
	h.buildTableRows()
	cells := func() []string {
		var out []string
		for _, row := range h.primarySessionRows() {
			out = append(out, ansi.Strip(h.table.Rows()[row][4]))
		}
		return out
	}
	before := cells()
	next, _ := h.Update(h.spinner.Tick())
	h = next.(HomeModel)
	after := cells()
	for i, label := range labels {
		if before[i] == after[i] || !strings.HasSuffix(before[i], label) || !strings.HasSuffix(after[i], label) {
			t.Errorf("%s spinner did not advance on tick: before %q, after %q", label, before[i], after[i])
		}
	}
}

func TestHomeSectionTitlesOnlySeparateTwoGroups(t *testing.T) {
	needs := func(id string) *pb.Session {
		return &pb.Session{Id: id, Title: id, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Check it"}}
	}
	progress := func(id string) *pb.Session {
		return &pb.Session{Id: id, Title: id, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "idle"}}
	}
	for _, tc := range []struct {
		name     string
		sessions []*pb.Session
		titled   bool
	}{
		{"none", nil, false},
		{"only needs you", []*pb.Session{needs("n1"), needs("n2")}, false},
		{"only in progress", []*pb.Session{progress("p1"), progress("p2")}, false},
		{"both", []*pb.Session{progress("p1"), needs("n1"), progress("p2"), needs("n2")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.width, h.height = 120, 40
			h.sessions = tc.sessions
			h.buildTableRows()

			rows := h.table.Rows()
			if h.tableDataRowCount() != len(rows) {
				t.Fatalf("height accounting %d differs from rendered rows %d", h.tableDataRowCount(), len(rows))
			}
			titles := 0
			for i, r := range rows {
				title := ansi.Strip(r[2])
				if title != "NEEDS YOU" && title != "IN PROGRESS" {
					continue
				}
				titles++
				if r[2] != styleEmphasis.Render(title) {
					t.Errorf("title %q is not in the emphasis foreground: %q", title, r[2])
				}
				if i == 0 || strings.Join(rows[i-1], "") != "" {
					t.Errorf("title %q has no blank row above it", title)
				}
			}
			if want := map[bool]int{true: 2, false: 0}[tc.titled]; titles != want {
				t.Fatalf("titles = %d, want %d:\n%s", titles, want, ansi.Strip(h.table.View()))
			}
			if tc.titled {
				board := strings.Split(ansi.Strip(h.boardView()), "\n")
				repoAt := strings.Index(board[0], "REPO")
				rules := 0
				for _, line := range board {
					for _, title := range []string{"NEEDS YOU", "IN PROGRESS"} {
						if !strings.Contains(line, " "+title+" ") {
							continue
						}
						rules++
						left, right, ok := strings.Cut(strings.TrimLeft(line, " "), " "+title+" ")
						if !ok || strings.Trim(left, "─") != "" || strings.Trim(right, "─") != "" {
							t.Errorf("%q is not a rule around its title: %q", title, line)
						}
						if at := len(line) - len(strings.TrimLeft(line, " ")); at != repoAt {
							t.Errorf("%q rule starts at column %d, want the REPO column %d:\n%s", title, at, repoAt, strings.Join(board, "\n"))
						}
						if got, want := ansi.StringWidth(line), ansi.StringWidth(board[0]); got != want {
							t.Errorf("%q rule is %d wide, want the table's %d", title, got, want)
						}
						if l, r := ansi.StringWidth(left), ansi.StringWidth(right); r-l < 0 || r-l > 1 {
							t.Errorf("%q is not centred: %d cells of rule left, %d right", title, l, r)
						}
					}
				}
				if rules != 2 {
					t.Fatalf("rules = %d, want 2:\n%s", rules, strings.Join(board, "\n"))
				}
			}

			var visited []string
			for range h.sessions {
				id := h.selectedSessionID()
				if cursor, ok := h.tableCursorForSessionID(id); !ok || cursor != h.table.Cursor() {
					t.Fatalf("cursor %d is not on a session's primary row (selected %q)", h.table.Cursor(), id)
				}
				visited = append(visited, id)
				next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				h = next.(HomeModel)
			}
			var want []string
			for _, s := range h.sessions {
				want = append(want, s.Id)
			}
			if !reflect.DeepEqual(visited, want) {
				t.Fatalf("down visited %v, want every session once in board order %v", visited, want)
			}
		})
	}
}

// TestHomeFooterActionsNeverShareAKey renders the action bar for every
// attention shape the daemon computes, each linked to a tracker, under every
// auth state, and fails when two actions claim one key.
func TestHomeFooterActionsNeverShareAKey(t *testing.T) {
	mergeable := true
	inputs := map[string]*attention.Input{
		"fix":         {Fix: &attention.FixFact{AlertKey: "alert", Sentence: "broken"}},
		"question":    {QuestionText: "which one?"},
		"review":      {ParkCodes: []string{"human-review:ui"}, Evidence: &attention.Evidence{URL: "https://example.com/proof.mp4"}},
		"blocked":     {ParkCodes: []string{"ledger-open"}},
		"merge":       {HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable},
		"in progress": {ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
		"legacy":      nil,
	}
	auths := []struct {
		name     string
		mgr      *auth.Manager
		loggedIn bool
	}{{"no auth", nil, false}, {"logged out", &auth.Manager{}, false}, {"logged in", &auth.Manager{}, true}}
	offered := map[pb.AttentionAction]bool{}
	keyOf := regexp.MustCompile(`\[([^\]]+)\]`)
	for kind, in := range inputs {
		var att *pb.SessionAttention
		if in != nil {
			in.HasTrackerLink = true
			att = attention.Compute(*in)
		}
		for _, a := range att.GetActions() {
			offered[a] = true
		}
		for _, a := range auths {
			h := NewHomeModel(nil, context.Background(), nil)
			h.width, h.height = 120, 30
			h.authMgr, h.loggedIn = a.mgr, a.loggedIn
			h.sessions = []*pb.Session{{Id: "s", Title: "session", Attention: att}}
			h.buildTableRows()
			left, nav, quit := h.sessionTableFooterActions()
			claimed := map[string]string{}
			for _, label := range slices.Concat(left, nav, quit) {
				m := keyOf.FindStringSubmatch(label)
				if m == nil {
					t.Fatalf("%s/%s: action %q names no key", kind, a.name, label)
				}
				if prev, dup := claimed[m[1]]; dup {
					t.Errorf("%s/%s: %q and %q both claim [%s]", kind, a.name, prev, label, m[1])
				}
				claimed[m[1]] = label
			}
		}

		trackerURL := "https://linear.app/team/issue/BOS-1"
		m := seedChatPicker(&chatPickerStub{}, statusWorking)
		m.newTabSupported = true
		m.session = &pb.Session{Id: "session-1", PrNumber: proto.Int32(42), TrackerUrl: &trackerURL, Attention: att}
		left, middle, back := m.chatListActionGroups()
		claimed := map[string]string{}
		for _, label := range slices.Concat(left, middle, back) {
			key := keyOf.FindStringSubmatch(label)[1]
			if prev, dup := claimed[key]; dup {
				t.Errorf("%s/details: %q and %q both claim [%s]", kind, prev, label, key)
			}
			claimed[key] = label
		}
	}
	for _, b := range attentionActionBindings {
		if !offered[b.action] {
			t.Errorf("no state offers %v, so the matrix no longer covers every footer action", b.action)
		}
	}
}

// TestHomeSectionRuleFitsItsWidth: a group rule never overflows the width it
// is given, centres its title in light rule when there is room, and shows the
// title alone, cut to fit, when there is not.
func TestHomeSectionRuleFitsItsWidth(t *testing.T) {
	for _, tt := range []struct {
		width int
		want  string
	}{
		{0, ""},
		{5, "NEED…"},
		{9, "NEEDS YOU"},
		{12, "NEEDS YOU"},
		{13, "─ NEEDS YOU ─"},
		{14, "─ NEEDS YOU ──"},
		{30, "───────── NEEDS YOU ──────────"},
	} {
		got := sectionRule("NEEDS YOU", tt.width)
		if ansi.Strip(got) != tt.want {
			t.Errorf("width %d: %q, want %q", tt.width, ansi.Strip(got), tt.want)
		}
		if ansi.StringWidth(got) > tt.width {
			t.Errorf("width %d: rule is %d wide", tt.width, ansi.StringWidth(got))
		}
	}
	if got := sectionRule("NEEDS YOU", 30); !strings.Contains(got, styleEmphasis.Render("NEEDS YOU")) || !strings.Contains(got, styleStatusMuted.Render("──────────")) {
		t.Errorf("rule styling: %q", got)
	}
}
