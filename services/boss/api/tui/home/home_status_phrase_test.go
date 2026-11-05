package home

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/bossalib/attention"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func phraseBoard(t *testing.T, width, height int, sessions []*pb.Session) Model {
	t.Helper()
	h := NewModel(nil, context.Background(), nil)
	h.Loading = false
	h.RepoCount = 1
	h.Width, h.Height = width, height
	h.Sessions = sessions
	h.BuildTableRows()
	return h
}

// statusCell is the plain STATUS cell of session id's primary row.
func statusCell(t *testing.T, h Model, id string) string {
	t.Helper()
	row, ok := h.TableCursorForSessionID(id)
	if !ok {
		t.Fatalf("no row for %s", id)
	}
	cells := h.Table.Rows()[row]
	return ansi.Strip(cells[len(cells)-1])
}

func statusColumnWidth(h Model) int {
	for _, c := range h.Table.Columns() {
		if c.Title == "STATUS" {
			return c.Width
		}
	}
	return -1
}

// boardLines is the rendered board as plain lines, its group titles drawn.
func boardLines(h Model) []string {
	return strings.Split(ansi.Strip(h.boardView()), "\n")
}

// sectionOf names the group title above the first line containing text.
func sectionOf(t *testing.T, lines []string, text string) string {
	t.Helper()
	section := ""
	for _, line := range lines {
		for _, title := range []string{"NEEDS YOU", "IN PROGRESS"} {
			if strings.Contains(line, title) {
				section = title
			}
		}
		if strings.Contains(line, text) {
			return section
		}
	}
	t.Fatalf("no line contains %q:\n%s", text, strings.Join(lines, "\n"))
	return ""
}

// TestHomeRowsShowTheServedPhrase_Sessions_AC14: at both widths each row's
// STATUS cell is the phrase the daemon served, in its group, with nothing
// before or after it but an IN PROGRESS spinner.
func TestHomeRowsShowTheServedPhrase_Sessions_AC14(t *testing.T) {
	for _, width := range []int{80, 120} {
		h := phraseBoard(t, width, 60, fixtures.AttentionSessions())
		lines := boardLines(h)
		spin := ansi.Strip(h.Spinner.View())
		for _, tc := range []struct{ id, cell, section string }{
			{"attention-0", attention.ReviewPhrase, "NEEDS YOU"},
			{"attention-2", "⨯ repair failed", "NEEDS YOU"},
			{"attention-3", "? question", "NEEDS YOU"},
			{"attention-6", attention.ReviewPhrase, "NEEDS YOU"},
			{"attention-7", attention.MergePhrase, "NEEDS YOU"},
			{"attention-8", spin + "checking 12/14", "IN PROGRESS"},
			{"attention-10", spin + "repairing 1 P2", "IN PROGRESS"},
		} {
			if got := strings.TrimSpace(statusCell(t, h, tc.id)); got != tc.cell {
				t.Errorf("width %d: %s STATUS = %q, want %q", width, tc.id, got, tc.cell)
			}
			if got := sectionOf(t, lines, strings.TrimPrefix(tc.cell, spin)); got != tc.section {
				t.Errorf("width %d: %q sits under %q, want %q", width, tc.cell, got, tc.section)
			}
		}
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "権限ルールの見直し") }) {
			t.Errorf("width %d: the CJK title is missing:\n%s", width, strings.Join(lines, "\n"))
		}
	}
}

// confidenceWords are the level words and glyphs the board drew before the
// served phrase replaced its confidence cell.
var confidenceWords = []string{"high", "moderate", "low", "blocked", "cannot assess", "◐", "▽", "⨯", "◌"}

// TestHomeStatusCellsCarryNoLevelAndNoCut_Sessions_AC208901 scans every STATUS
// cell of the fixture board, whose parks still serve confidence levels: none
// names a level or draws its glyph past the leading status mark ("⨯ conflict"
// marks a problem, not the Blocked level), and none is cut.
func TestHomeStatusCellsCarryNoLevelAndNoCut_Sessions_AC208901(t *testing.T) {
	for _, width := range []int{80, 120} {
		h := phraseBoard(t, width, 60, fixtures.AttentionSessions())
		for _, s := range h.BoardSessions() {
			cell := strings.TrimSpace(statusCell(t, h, s.GetId()))
			if strings.HasSuffix(cell, "…") {
				t.Errorf("width %d: %s STATUS %q is cut", width, s.GetId(), cell)
			}
			words := strings.TrimPrefix(cell, "⨯ ")
			for _, word := range confidenceWords {
				if slices.Contains(strings.Fields(words), word) || strings.Contains(words, word) && len([]rune(word)) == 1 {
					t.Errorf("width %d: %s STATUS %q names %q", width, s.GetId(), cell, word)
				}
			}
		}
	}
}

// TestHomeRowsDrawNoDetailLine_Sessions_AC233601: a row whose daemon serves a
// status detail is still one line; the detail appears nowhere on the board.
func TestHomeRowsDrawNoDetailLine_Sessions_AC233601(t *testing.T) {
	for _, width := range []int{80, 120} {
		h := phraseBoard(t, width, 60, fixtures.AttentionSessions())
		board := strings.Join(boardLines(h), "\n")
		details := 0
		for _, s := range h.BoardSessions() {
			detail := ui.SessionStatusDetail(s)
			if detail == "" {
				continue
			}
			details++
			if strings.Contains(board, detail) {
				t.Errorf("width %d: %s detail %q is drawn:\n%s", width, s.GetId(), detail, board)
			}
		}
		if details == 0 {
			t.Fatal("precondition: no fixture session serves a detail")
		}
		if got, want := len(h.Table.Rows()), len(h.BoardSessions())+2*sectionHeaderRows; got != want {
			t.Errorf("width %d: board has %d rows, want one per session plus the title rows (%d)", width, got, want)
		}
	}
}

// TestHomeCursorWalksEverySession_Sessions_AC233601: walking the board down
// and back up selects each session in turn and never an auxiliary row, and
// the row accounting the cursor and height helpers use without a layout
// agrees with the rows rendered.
func TestHomeCursorWalksEverySession_Sessions_AC233601(t *testing.T) {
	h := phraseBoard(t, 120, 60, fixtures.AttentionSessions())
	if got, want := h.TableDataRowCount(), len(h.Table.Rows()); got != want {
		t.Fatalf("height counts %d rows, the table has %d", got, want)
	}
	cold := Model{Sessions: h.Sessions}
	if got, want := cold.TableDataRowCount(), len(h.Table.Rows()); got != want {
		t.Fatalf("SessionSubRowCount accounting gives %d rows, the table has %d", got, want)
	}
	for i := range h.BoardSessions() {
		if got, want := cold.TableCursorForSessionIndex(i), h.TableCursorForSessionIndex(i); got != want {
			t.Errorf("session %d: counted row %d, rendered row %d", i, got, want)
		}
	}
	ids := make([]string, 0, len(h.BoardSessions()))
	for _, s := range h.BoardSessions() {
		ids = append(ids, s.GetId())
	}
	h.Table.SetCursor(h.TableCursorForSessionIndex(0))
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyUp}} {
		walk := ids[1:]
		if key.Code == tea.KeyUp {
			walk = slices.Clone(ids[:len(ids)-1])
			slices.Reverse(walk)
		}
		for _, want := range walk {
			next, _ := h.Update(key)
			h = next.(Model)
			if got := h.SelectedSessionID(); got != want {
				t.Fatalf("%v selected %s, want %s", key, got, want)
			}
			if h.layout.rowSession[h.Table.Cursor()] < 0 {
				t.Fatalf("%v left the cursor on auxiliary row %d", key, h.Table.Cursor())
			}
		}
	}
}

// TestHomeShortBoardScrollsToTheLastSession_Sessions_AC233601: on a board too
// short for its rows, moving to the last session scrolls it into view.
func TestHomeShortBoardScrollsToTheLastSession_Sessions_AC233601(t *testing.T) {
	h := phraseBoard(t, 120, 20, fixtures.AttentionSessions())
	if h.Table.Height() >= len(h.Table.Rows()) {
		t.Fatalf("precondition: table height %d shows all %d rows", h.Table.Height(), len(h.Table.Rows()))
	}
	for range h.BoardSessions() {
		next, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		h = next.(Model)
	}
	if got := h.SelectedSessionID(); got != "attention-10" {
		t.Fatalf("selected %s, want the last session", got)
	}
	if screen := ansi.Strip(h.Table.View()); !strings.Contains(screen, "[BOS-1510]") {
		t.Fatalf("the selected last session is off screen:\n%s", screen)
	}
}

// TestHomeOlderDaemonBoardRendersAsBefore_Sessions_AC14: a board whose
// daemon serves no phrase keeps its kind words, its 16-cell STATUS column
// and one line per session.
func TestHomeOlderDaemonBoardRendersAsBefore_Sessions_AC14(t *testing.T) {
	sessions := fixtures.AttentionSessions()
	for i, s := range sessions {
		s = proto.CloneOf(s)
		s.Attention.StatusPhrase, s.Attention.StatusDetail = "", ""
		sessions[i] = s
	}
	h := phraseBoard(t, 120, 60, sessions)
	if got, want := statusColumnWidth(h), homeStatusWidth+ui.TableColumnSep; got != want {
		t.Errorf("STATUS = %d, want %d", got, want)
	}
	for id, want := range map[string]string{"attention-3": "? question", "attention-5": "waiting for you", "attention-7": "✓ merge", "attention-2": "⨯ failing"} {
		if got := strings.TrimSpace(statusCell(t, h, id)); got != want {
			t.Errorf("%s STATUS = %q, want %q", id, got, want)
		}
	}
	sections := 2 * sectionHeaderRows
	if got, want := len(h.Table.Rows()), len(sessions)+sections; got != want {
		t.Errorf("board has %d rows, want one per session plus %d title rows (%d)", got, sections, want)
	}
}

// TestHomePhraseStatusColumnFitsEveryPhrase: a board serving phrases widens
// STATUS to the phrase budget and its spinner, so the longest phrase is whole.
func TestHomePhraseStatusColumnFitsEveryPhrase(t *testing.T) {
	long := strings.Repeat("w", 22)
	s := &pb.Session{Id: "long", Title: "long", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: long, StatusPhrase: long}}
	h := phraseBoard(t, 160, 30, []*pb.Session{s})
	if got, want := statusColumnWidth(h), homePhraseStatusWidth+ui.TableColumnSep; got != want {
		t.Fatalf("STATUS = %d, want %d", got, want)
	}
	if cell := strings.TrimSpace(statusCell(t, h, "long")); !strings.HasSuffix(cell, long) {
		t.Fatalf("STATUS %q cut the phrase", cell)
	}
}

// TestHomeFilterMatchesPhraseAndDetail_Sessions_AC233601: the search reads
// the served phrase and its undrawn detail, so an area the phrase counts as
// +N still finds its row.
func TestHomeFilterMatchesPhraseAndDetail_Sessions_AC233601(t *testing.T) {
	for query, want := range map[string]string{"compatibility": "attention-0", "repairing": "attention-10", "App.tsx": "attention-10"} {
		h := phraseBoard(t, 120, 60, fixtures.AttentionSessions())
		h, _ = homeKey(h, keyString("/"))
		for _, r := range query {
			h, _ = homeKey(h, keyString(string(r)))
		}
		if got := h.BoardSessions(); len(got) != 1 || got[0].GetId() != want {
			ids := make([]string, 0, len(got))
			for _, s := range got {
				ids = append(ids, s.GetId())
			}
			t.Errorf("filter %q kept %v, want %s", query, ids, want)
		}
	}
}

// TestHomePhraseStylesBySection: a NEEDS YOU phrase never takes a spinner,
// even while a chat works, and reads danger for a fix, success for a merge
// and warning otherwise.
func TestHomePhraseStylesBySection(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	for _, tc := range []struct {
		kind   pb.AttentionKind
		phrase string
		style  func(...string) string
	}{
		{pb.AttentionKind_ATTENTION_KIND_FIX, "⨯ repair failed", ui.StyleStatusDanger.Render},
		{pb.AttentionKind_ATTENTION_KIND_FIX, "⨯ conflict", ui.StyleStatusDanger.Render},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, attention.MergePhrase, ui.StyleStatusSuccess.Render},
		{pb.AttentionKind_ATTENTION_KIND_MERGE, attention.UnverifiedMergePhrase, ui.StyleStatusWarning.Render},
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, attention.MergePhrase, ui.StyleStatusSuccess.Render},
		{pb.AttentionKind_ATTENTION_KIND_REVIEW, attention.ReviewPhrase, ui.StyleStatusSuccess.Render},
		{pb.AttentionKind_ATTENTION_KIND_QUESTION, "? question", ui.StyleStatusWarning.Render},
	} {
		for _, label := range []string{"working", "passing"} {
			s := &pb.Session{DisplayLabel: label, Attention: &pb.SessionAttention{Kind: tc.kind, StatusPhrase: tc.phrase}}
			if got := h.RenderSessionStatus(s); got != tc.style(tc.phrase) {
				t.Errorf("%v %q with a %s chat = %q, want %q", tc.kind, tc.phrase, label, got, tc.style(tc.phrase))
			}
		}
	}
}
