package home

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/fixtures"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func confidenceBoard(t *testing.T, width int, sessions []*pb.Session) Model {
	t.Helper()
	h := NewModel(nil, context.Background(), nil)
	h.Loading = false
	h.RepoCount = 1
	h.Width, h.Height = width, 60
	h.Sessions = sessions
	h.BuildTableRows()
	return h
}

// homeRowText is the plain text of session id's primary row.
func homeRowText(t *testing.T, h Model, id string) string {
	t.Helper()
	row, ok := h.TableCursorForSessionID(id)
	if !ok {
		t.Fatalf("no row for %s", id)
	}
	return ansi.Strip(strings.Join(h.Table.Rows()[row], " "))
}

func statusColumnWidth(h Model) int {
	for _, c := range h.Table.Columns() {
		if c.Title == "STATUS" {
			return c.Width
		}
	}
	return -1
}

// TestHomeConfidenceRows_Sessions_AC208901 renders the fixture board: each
// leveled park reads its glyph, level word and asks, the repairing blocked
// park reads blocked and repairing, and a level-less review reads as before.
func TestHomeConfidenceRows_Sessions_AC208901(t *testing.T) {
	sessions := fixtures.AttentionSessions()
	for _, width := range []int{80, 120} {
		h := confidenceBoard(t, width, sessions)
		for id, want := range map[string][]string{
			"attention-0":  {"moderate", "schema", "api"},
			"attention-4":  {"low", "large", "auth"},
			"attention-10": {"blocked", "repairing"},
			"attention-5":  {"waiting for you"},
		} {
			row := homeRowText(t, h, id)
			s := sessions[slices.IndexFunc(sessions, func(s *pb.Session) bool { return s.GetId() == id })]
			if c, ok := ui.ConfidenceCell(s); ok {
				want = append(want, c.Icon)
			}
			for _, w := range want {
				if !strings.Contains(row, w) {
					t.Errorf("width %d: %s row %q lacks %q", width, id, row, w)
				}
			}
		}
	}
}

// TestHomeStatusWidthFollowsConfidence_Sessions_AC208901: STATUS keeps its
// width on a board with no level, widens to the widest confidence cell, and
// stops at the cap, cutting a longer cell with an ellipsis.
func TestHomeStatusWidthFollowsConfidence_Sessions_AC208901(t *testing.T) {
	review := func(id string, level pb.ConfidenceLevel, asks ...string) *pb.Session {
		return &pb.Session{Id: id, Title: id, RepoDisplayName: "app", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:ui"}, ConfidenceLevel: level, Asks: asks}}
	}
	plain := confidenceBoard(t, 160, []*pb.Session{review("plain", pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED, "schema")})
	base := statusColumnWidth(plain)
	if base != homeStatusWidth+ui.TableColumnSep {
		t.Fatalf("level-less STATUS = %d, want %d", base, homeStatusWidth+ui.TableColumnSep)
	}
	leveled := confidenceBoard(t, 160, []*pb.Session{review("plain", 0), review("moderate", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, "schema", "api")})
	if w := statusColumnWidth(leveled); w <= base || w > homeStatusMaxWidth+ui.TableColumnSep {
		t.Errorf("leveled STATUS = %d, want wider than %d and at most the cap", w, base)
	}
	if row := homeRowText(t, leveled, "moderate"); strings.Contains(row, "…") {
		t.Errorf("a cell under the cap was cut: %q", row)
	}
	long := confidenceBoard(t, 160, []*pb.Session{review("long", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, "database schema", "public api surface", "auth")})
	if w := statusColumnWidth(long); w != homeStatusMaxWidth+ui.TableColumnSep {
		t.Errorf("over-long STATUS = %d, want the cap %d", w, homeStatusMaxWidth+ui.TableColumnSep)
	}
	cell := ansi.Strip(long.RenderSessionStatus(long.Sessions[0]))
	if !strings.HasSuffix(cell, "…") || ansi.StringWidth(cell) > homeStatusMaxWidth {
		t.Errorf("over-long cell = %q, want it cut to %d with an ellipsis", cell, homeStatusMaxWidth)
	}
}

// TestHomeFilterMatchesConfidenceAsks_Sessions_AC208901: the search reads the
// confidence cell, so an ask finds its park.
func TestHomeFilterMatchesConfidenceAsks_Sessions_AC208901(t *testing.T) {
	h := confidenceBoard(t, 120, fixtures.AttentionSessions())
	h, _ = homeKey(h, keyString("/"))
	for _, r := range "schema" {
		h, _ = homeKey(h, keyString(string(r)))
	}
	var ids []string
	for _, s := range h.BoardSessions() {
		ids = append(ids, s.GetId())
	}
	if !slices.Equal(ids, []string{"attention-0"}) {
		t.Fatalf("filter schema kept %v, want the moderate park", ids)
	}
	h, _ = homeKey(h, keyString("esc"))
	h, _ = homeKey(h, keyString("/"))
	for _, r := range "repairing" {
		h, _ = homeKey(h, keyString(string(r)))
	}
	if ids := h.BoardSessions(); len(ids) != 1 || ids[0].GetId() != "attention-10" {
		t.Fatalf("filter repairing kept %d rows, want the blocked park", len(ids))
	}
}

// TestHomeFilterFindsAnAskPastTheCap_Sessions_AC208901: an ask the STATUS
// column cuts off is still found by the search.
func TestHomeFilterFindsAnAskPastTheCap_Sessions_AC208901(t *testing.T) {
	long := &pb.Session{Id: "long", Attention: &pb.SessionAttention{
		Kind:            pb.AttentionKind_ATTENTION_KIND_REVIEW,
		ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW,
		Asks:            []string{"database schema", "public api surface", "zebra"},
	}}
	other := &pb.Session{Id: "other", Attention: &pb.SessionAttention{
		Kind:            pb.AttentionKind_ATTENTION_KIND_REVIEW,
		ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE,
		Asks:            []string{"schema"},
	}}
	h := confidenceBoard(t, 120, []*pb.Session{long, other})
	if strings.Contains(ansi.Strip(h.RenderSessionStatus(long)), "zebra") {
		t.Fatalf("precondition: the cell should cut off the last ask")
	}
	h, _ = homeKey(h, keyString("/"))
	for _, r := range "zebra" {
		h, _ = homeKey(h, keyString(string(r)))
	}
	if got := h.BoardSessions(); len(got) != 1 || got[0].GetId() != "long" {
		t.Fatalf("filter zebra kept %d rows, want the long park", len(got))
	}
}

// TestHomeConfidenceSpinsInPlaceOfItsGlyph_Sessions_AC208901: while an agent
// chat works, the spinner stands where the level glyph was and the words stay.
func TestHomeConfidenceSpinsInPlaceOfItsGlyph_Sessions_AC208901(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	s := &pb.Session{DisplayLabel: ui.StatusWorking, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, Asks: []string{"schema"}}}
	c, _ := ui.ConfidenceCell(s)
	got := ansi.Strip(h.RenderSessionStatus(s))
	if want := ansi.Strip(h.Spinner.View()) + c.Words(); got != want {
		t.Errorf("working cell = %q, want %q", got, want)
	}
}
