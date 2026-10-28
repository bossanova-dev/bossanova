package ui

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/domain/display"
)

func TestActionBar(t *testing.T) {
	tests := []struct {
		name   string
		groups [][]string
		want   string // substring that must appear in rendered output
	}{
		{
			name:   "single group",
			groups: [][]string{{"[q]uit"}},
			want:   "[q]uit",
		},
		{
			name:   "two groups separated by dot",
			groups: [][]string{{"[enter] select", "[a]rchive"}, {"[q]uit"}},
			want:   "[enter] select  [a]rchive · [q]uit",
		},
		{
			name:   "three groups",
			groups: [][]string{{"[enter] select"}, {"[n]ew", "[r]epos"}, {"[q]uit"}},
			want:   "[enter] select · [n]ew  [r]epos · [q]uit",
		},
		{
			name:   "empty groups are skipped",
			groups: [][]string{{}, {"[a]dd"}, {}, {"[esc] back"}},
			want:   "[a]dd · [esc] back",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := actionBar(tt.groups...)
			if !strings.Contains(got, tt.want) {
				t.Errorf("actionBar() = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestCompactKeyHint(t *testing.T) {
	for _, tc := range []struct{ hint, want string }{
		{"[m] merge", "[m]erge"},
		{"[g] github", "[g]ithub"},
		{"[v] video", "[v]ideo"},
		{"[r] resume", "[r]esume"},
		{"[t] tracker", "[t]racker"},
		{"[y] approve", "[y] approve"},
		{"[b] send back", "[b] send back"},
		{"[x] dismiss", "[x] dismiss"},
		{"[a]rchive", "[a]rchive"},
		{"[R]eauth", "[R]eauth"},
		{"[R] reauth", "[R] reauth"},
		{"[r] Resume", "[r] Resume"},
		{"[M] Merge", "[M]erge"},
		{"[enter] enter", "[enter] enter"},
		{"[ctrl+d] delete", "[ctrl+d] delete"},
		{"[a] delete all", "[a] delete all"},
		{"[y] merge anyway  [m] merge", "[y] merge anyway  [m]erge"},
		{"[m] merge  [g] github", "[m]erge  [g]ithub"},
		{"no hint here", "no hint here"},
	} {
		if got := compactKeyHint(tc.hint); got != tc.want {
			t.Errorf("compactKeyHint(%q) = %q, want %q", tc.hint, got, tc.want)
		}
	}
}

// TestActionBarCompactsKeyHints renders through every action-bar entry point,
// so a caller that builds "[key] label" never has to compact it itself.
func TestActionBarCompactsKeyHints(t *testing.T) {
	groups := [][]string{{"[n]ew session", "[m] merge", "[y] approve"}, {"[g] github"}, {"[q]uit"}}
	want := "[n]ew session  [m]erge  [y] approve · [g]ithub · [q]uit"
	if got := ansi.Strip(actionBar(groups...)); !strings.Contains(got, want) {
		t.Errorf("actionBar = %q, want %q", got, want)
	}
	if got := ansi.Strip(ActionBarWidth(120, groups...)); !strings.Contains(got, want) {
		t.Errorf("actionBarWidth = %q, want %q", got, want)
	}
	if got := ansi.Strip(FormActionBarWidth(120, []string{"[s] save"}, []string{"[esc] back"})); !strings.Contains(got, "[s]ave · [esc] back") {
		t.Errorf("formActionBarWidth = %q, want [s]ave", got)
	}
	folded := []string{"[m] merge", "[v] video", "[r] resume", "[x] dismiss"}
	if n := ActionBarLineCount(24, folded); n < 2 {
		t.Fatalf("the oversized group renders on %d line(s); this case needs it folded", n)
	}
	got := ansi.Strip(ActionBarWidth(24, folded))
	for _, hint := range []string{"[m]erge", "[v]ideo", "[r]esume", "[x] dismiss"} {
		if !strings.Contains(got, hint) {
			t.Errorf("folded group = %q, want %q", got, hint)
		}
	}
}

func TestActionBarWidth(t *testing.T) {
	groups := [][]string{
		{"[n]ew session", "[enter] select"},
		{"[s]ettings", "[l]ogout", "[c]loud"},
		{"[q]uit"},
	}

	for _, width := range []int{40, 60, 72, 80, 100, 140} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			bar := ActionBarWidth(width, groups...)
			for _, line := range strings.Split(bar, "\n") {
				if got := ansi.StringWidth(line); got > width {
					t.Errorf("actionBarWidth(%d) rendered %d-column line, want <= %d: %q", width, got, width, line)
				}
			}
			if got, want := ActionBarLineCount(width, groups...), lipgloss.Height(bar)-ActionBarPadY*2; got != want {
				t.Errorf("actionBarLineCount(%d) = %d, want rendered text-line count %d", width, got, want)
			}
		})
	}
}

func TestActionBarWidthDoesNotSplitGroups(t *testing.T) {
	groups := [][]string{
		{"[n]ew session", "[enter] select"},
		{"[s]ettings", "[l]ogout"},
		{"[q]uit"},
	}
	bar := ActionBarWidth(40, groups...)
	for _, group := range groups {
		joined := strings.Join(group, "  ")
		for _, line := range strings.Split(bar, "\n") {
			if strings.Contains(line, joined) {
				continue
			}
			for _, item := range group {
				if strings.Contains(line, item) {
					t.Fatalf("action group %q split across lines in %q", joined, bar)
				}
			}
		}
	}
}

func TestActionBarWidthFoldsOversizedLegacyGroupsBetweenActions(t *testing.T) {
	group := []string{"[enter] open", "[g]eneral", "[r]epos", "[c]ron", "[a]ccounts", "[t]rash"}
	bar := ActionBarWidth(40, group, []string{"[esc] back"})
	for _, line := range strings.Split(bar, "\n") {
		if got := ansi.StringWidth(line); got > 40 {
			t.Errorf("actionBarWidth(40) rendered %d-column line: %q", got, line)
		}
	}
	for _, action := range group {
		if strings.Count(bar, action) != 1 {
			t.Errorf("action %q is missing or split in %q", action, bar)
		}
	}
}

func TestActionBarWidthUnknownPreservesExistingOutput(t *testing.T) {
	groups := [][]string{{"[enter] select", "[a]rchive"}, {"[q]uit"}}
	if got, want := ActionBarWidth(0, groups...), actionBar(groups...); got != want {
		t.Errorf("actionBarWidth(0) changed output:\n got %q\nwant %q", got, want)
	}
}

// TestFormNavHints pins the only key pair huh binds identically across every
// field type. enter is overloaded (it advances an Input but submits a Select),
// so it must never be advertised as the "next field" key.
func TestFormNavHints(t *testing.T) {
	got := FormNavHints()
	want := []string{"[tab] next field", "[shift+tab] previous field", "[click] select field"}
	if len(got) != len(want) {
		t.Fatalf("formNavHints() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("formNavHints()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, hint := range got {
		if strings.Contains(hint, "[enter]") {
			t.Errorf("formNavHints() advertises enter as field movement: %q", hint)
		}
	}
}

func TestFormActionBar(t *testing.T) {
	got := formActionBar([]string{"[enter] save"}, []string{"[esc] cancel"})

	// The shared nav hints own the first text line, on their own.
	if want := "[tab] next field  [shift+tab] previous field  [click] select field"; !strings.Contains(got, want) {
		t.Errorf("formActionBar() = %q, want substring %q", got, want)
	}
	// The caller's actions own the second, with the right-hand group separated
	// by the action-bar separator.
	if want := "[enter] save" + actionBarSeparator + "[esc] cancel"; !strings.Contains(got, want) {
		t.Errorf("formActionBar() = %q, want substring %q", got, want)
	}
	// Nav hints and caller actions must not share a line: folded together the
	// add-repo details bar is 101 columns and wraps on an 80-column terminal.
	if want := "[click] select field  [enter] save"; strings.Contains(got, want) {
		t.Errorf("formActionBar() folded the nav hints and actions onto one line: %q", got)
	}
	assertFormActionBarLineBudget(t, got)
}

func TestFormActionBarWithoutRightGroup(t *testing.T) {
	got := formActionBar([]string{"[enter] submit"}, nil)
	if want := "[tab] next field  [shift+tab] previous field  [click] select field"; !strings.Contains(got, want) {
		t.Errorf("formActionBar() = %q, want substring %q", got, want)
	}
	if !strings.Contains(got, "[enter] submit") {
		t.Errorf("formActionBar() = %q, want the submit action", got)
	}
	if strings.Contains(got, actionBarSeparator) {
		t.Errorf("formActionBar() = %q, want no group separator when right group is empty", got)
	}
	assertFormActionBarLineBudget(t, got)
}

func TestFormActionBarWidthMatchesItsReportedLineCount(t *testing.T) {
	for _, width := range []int{40, 60, 72, 80, 100, 140} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			bar := FormActionBarWidth(width, []string{"[enter] save"}, []string{"[esc] cancel"})
			for _, line := range strings.Split(bar, "\n") {
				if got := ansi.StringWidth(line); got > width {
					t.Errorf("formActionBarWidth(%d) rendered %d-column line, want <= %d: %q", width, got, width, line)
				}
			}
			if got, want := FormActionBarLineCount(width, []string{"[enter] save"}, []string{"[esc] cancel"}), lipgloss.Height(bar)-ActionBarPadY*2; got != want {
				t.Errorf("formActionBarLineCount(%d) = %d, want rendered text-line count %d", width, got, want)
			}
		})
	}
}

// assertFormActionBarLineBudget pins the rendered bar to the vertical budget
// repoAddChrome and cronFormChrome reserve for it: styleActionBar's top and
// bottom padding plus formActionBarLines of text. A third text line would push
// the bar off a short terminal without any other test noticing.
func assertFormActionBarLineBudget(t *testing.T, bar string) {
	t.Helper()
	want := ActionBarPadY*2 + FormActionBarLines
	if got := lipgloss.Height(bar); got != want {
		t.Errorf("formActionBar() renders %d lines, want %d (the chrome constants budget that)", got, want)
	}
}

func TestCLIColumnsWidth(t *testing.T) {
	// Each column contributes its width plus 2 chars of cell padding (1 per side).
	tests := []struct {
		name string
		cols []table.Column
		want int
	}{
		{name: "empty", cols: nil, want: 0},
		{name: "single", cols: []table.Column{{Width: 3}}, want: 5},
		{name: "multiple", cols: []table.Column{{Width: 3}, {Width: 5}, {Width: 10}}, want: 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := display.CLIColumnsWidth(tt.cols); got != tt.want {
				t.Errorf("CLIColumnsWidth(%v) = %d, want %d", tt.cols, got, tt.want)
			}
		})
	}
}

func TestMaxColWidth_Uncapped(t *testing.T) {
	values := []string{"8078890d6b0affc7"}
	if got, want := display.MaxColWidth("ID", values, 0), len(values[0]); got != want {
		t.Errorf("MaxColWidth uncapped = %d, want %d", got, want)
	}
}

func TestRenderCLITable(t *testing.T) {
	cols := []table.Column{
		{Title: "ID", Width: 16},
		{Title: "LABEL", Width: 13},
		{Title: "EMAIL", Width: 19},
	}
	rows := []table.Row{
		{"8078890d6b0affc7", "agent.yuki", "agent.yuki@kamik.ai"},
		{"6aaff35db711eee5", "dave@kamik.ai", "dave@kamik.ai"},
	}
	want := strings.Join([]string{
		"ID                LABEL          EMAIL              ",
		"8078890d6b0affc7  agent.yuki     agent.yuki@kamik.ai",
		"6aaff35db711eee5  dave@kamik.ai  dave@kamik.ai      ",
	}, "\n")
	if got := display.RenderCLITable(cols, rows); got != want {
		t.Errorf("RenderCLITable() =\n%q\nwant\n%q", got, want)
	}
}

// --- Responsive column fitting (BOS-572) ---

// responsiveTestWidths are the terminal widths every fitColumns case is
// exercised at: two narrow-tier widths, the classic 72/80 boundary pair, and
// two full-tier widths. 140 is the proof harness default, so it doubles as the
// "must behave exactly like the pre-BOS-572 board" width.
var responsiveTestWidths = []int{40, 60, 72, 80, 100, 140}

// testResponsiveColumns is the shared descriptor fixture: one never-dropped
// column plus three droppable ones with distinct priorities, declared out of
// priority order on purpose so a naive right-to-left drop would fail.
//
// Declared width (incl. tableColumnGap per column) is 41+15+9+7 = 72, i.e. it
// fits at 72 and above and is over budget at 60 and 40.
func testResponsiveColumns() []ResponsiveColumn {
	return []ResponsiveColumn{
		{Col: table.Column{Title: "NAME", Width: 40}, Priority: 0, MinWidth: 10},
		{Col: table.Column{Title: "STATUS", Width: 14}, Priority: 1, MinWidth: 6},
		{Col: table.Column{Title: "PR", Width: 8}, Priority: 3, MinWidth: 4},
		{Col: table.Column{Title: "AGE", Width: 6}, Priority: 2, MinWidth: 3},
	}
}

// declaredColumns extracts the table.Columns from a descriptor slice, i.e. the
// exact value fitColumns must return when nothing needs fitting.
func declaredColumns(cols []ResponsiveColumn) []table.Column {
	out := make([]table.Column, len(cols))
	for i, c := range cols {
		out[i] = c.Col
	}
	return out
}

func columnTitles(cols []table.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Title
	}
	return out
}

func TestFitColumnsReturnsInputWhenItAlreadyFits(t *testing.T) {
	cols := testResponsiveColumns()
	want := declaredColumns(cols)
	wantWidth := ColumnsWidth(want)

	// 72 is the exact declared width; everything at or above it must be a no-op.
	// This is the regression guard for the committed 140-column board.
	for _, avail := range []int{72, 80, 100, 140, 1000} {
		got := FitColumns(cols, avail)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("fitColumns(avail=%d) = %v, want untouched %v", avail, got, want)
		}
		if gw := ColumnsWidth(got); gw != wantWidth {
			t.Fatalf("fitColumns(avail=%d) width = %d, want unchanged %d", avail, gw, wantWidth)
		}
	}
}

func TestFitColumnsUnknownWidthReturnsInput(t *testing.T) {
	cols := testResponsiveColumns()
	want := declaredColumns(cols)

	// avail <= 0 means no tea.WindowSizeMsg has arrived yet.
	for _, avail := range []int{0, -1, -140} {
		got := FitColumns(cols, avail)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("fitColumns(avail=%d) = %v, want untouched %v", avail, got, want)
		}
	}
}

func TestFitColumnsEmptyInput(t *testing.T) {
	if got := FitColumns(nil, 80); len(got) != 0 {
		t.Fatalf("fitColumns(nil, 80) = %v, want empty", got)
	}
	if got := FitColumnsIndexed([]ResponsiveColumn{}, 80); len(got) != 0 {
		t.Fatalf("fitColumnsIndexed(empty, 80) = %v, want empty", got)
	}
}

func TestFitColumnsDropsInPriorityOrder(t *testing.T) {
	tests := []struct {
		name  string
		avail int
		want  []string
	}{
		// Over by 12: drop PR (priority 3, width 8+gap=9) -> 63, still over;
		// drop AGE (priority 2, 6+gap=7) -> 56, fits. STATUS (priority 1) stays,
		// proving only as many columns as needed are dropped.
		{name: "drops highest priority first", avail: 60, want: []string{"NAME", "STATUS"}},
		// Over by 32: all three droppable columns go, priority-0 NAME survives.
		{name: "drops all droppable when needed", avail: 40, want: []string{"NAME"}},
		// One column over budget: only the highest priority column goes.
		{name: "drops exactly one", avail: 65, want: []string{"NAME", "STATUS", "AGE"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := columnTitles(FitColumns(testResponsiveColumns(), tt.avail))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("fitColumns(avail=%d) titles = %v, want %v", tt.avail, got, tt.want)
			}
		})
	}
}

func TestFitColumnsTieBreaksLeftmost(t *testing.T) {
	// Two columns share priority 2; the leftmost must be the one dropped so the
	// function is deterministic.
	cols := []ResponsiveColumn{
		{Col: table.Column{Title: "LEFT", Width: 10}, Priority: 2, MinWidth: 3},
		{Col: table.Column{Title: "RIGHT", Width: 10}, Priority: 2, MinWidth: 3},
		{Col: table.Column{Title: "KEEP", Width: 10}, Priority: 0, MinWidth: 3},
	}
	got := columnTitles(FitColumns(cols, 25)) // declared width 33, over by 8
	want := []string{"RIGHT", "KEEP"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fitColumns tie-break titles = %v, want %v", got, want)
	}
}

func TestFitColumnsNeverDropsPriorityZero(t *testing.T) {
	cols := testResponsiveColumns()
	widths := append([]int{1, 5, 10, 20}, responsiveTestWidths...)
	for _, avail := range widths {
		got := FitColumns(cols, avail)
		if len(got) == 0 || got[0].Title != "NAME" {
			t.Fatalf("fitColumns(avail=%d) = %v, want priority-0 NAME retained", avail, columnTitles(got))
		}
	}
}

func TestFitColumnsNeverEmitsNonPositiveWidth(t *testing.T) {
	cols := testResponsiveColumns()
	// bubbles omits a Width <= 0 column outright (renderRow and headersView both
	// `continue` on it) while columnsWidth still bills a gap for it, and lipgloss
	// reads Width(0) as unconstrained. Either way overflow is strictly safer, so
	// no width fitColumns COMPUTES may be non-positive.
	widths := append([]int{1, 5, 10, 20, -3, 0}, responsiveTestWidths...)
	for _, avail := range widths {
		for _, c := range FitColumns(cols, avail) {
			if c.Width <= 0 {
				t.Fatalf("fitColumns(avail=%d) produced column %q with width %d", avail, c.Title, c.Width)
			}
		}
	}
}

// TestFitColumnsPassesThroughADeclaredNonPositiveWidth pins the other half of
// rule 6, so the guarantee above is not misread as "the result never contains a
// non-positive width". fitColumns narrows columns; it does not validate them. A
// caller that declares Width: 0 gets it back on EVERY path — the early returns,
// the drop loop (which only removes columns) and the squeeze loop (which skips
// any column already at or below its floor, which a non-positive width always
// is). Declaring one is a caller bug; this test says so out loud rather than
// leaving a future reader to discover it from a vanished column.
func TestFitColumnsPassesThroughADeclaredNonPositiveWidth(t *testing.T) {
	cols := []ResponsiveColumn{
		{Col: table.Column{Title: "ZERO", Width: 0}, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "NAME", Width: 40}, Priority: 0, MinWidth: 8},
		{Col: table.Column{Title: "DROP", Width: 20}, Priority: 1, MinWidth: 4},
	}
	// 200 exits at rule 3 (already fits), 0 at rule 2 (unknown width), 30 runs
	// the drop loop, and 10 runs the squeeze loop to exhaustion.
	for _, avail := range []int{200, 0, 30, 10} {
		got := FitColumns(cols, avail)
		idx := slices.IndexFunc(got, func(c table.Column) bool { return c.Title == "ZERO" })
		if idx == -1 {
			t.Fatalf("fitColumns(avail=%d) dropped the priority-0 ZERO column: %v", avail, columnTitles(got))
		}
		if got[idx].Width != 0 {
			t.Errorf("fitColumns(avail=%d) rewrote the declared ZERO width to %d, want it passed through as 0", avail, got[idx].Width)
		}
	}
}

// TestFitColumnsNeverDropsANegativePriority pins the <= 0 sentinel. Testing
// == 0 would make a negative priority the LAST column dropped rather than a
// pinned one — the exact inversion of what a caller writing -1 means, and
// invisible in production since Home only ever declares 0..3.
//
// The fixture needs THREE columns, and specifically a priority-0 one. With only
// {-1, 1} the test cannot discriminate: under the == 0 bug the positive column
// still goes first, and rule 4a (never drop the last column standing) then
// protects the negative one anyway, so the assertions pass either way. Adding
// ZERO exposes the real inversion — under the bug, PINNED is dropped BEFORE the
// priority-0 column and ZERO is what survives.
func TestFitColumnsNeverDropsANegativePriority(t *testing.T) {
	cols := []ResponsiveColumn{
		{Col: table.Column{Title: "PINNED", Width: 20}, Priority: -1, MinWidth: 4},
		{Col: table.Column{Title: "ZERO", Width: 20}, Priority: 0, MinWidth: 4},
		{Col: table.Column{Title: "DROPPABLE", Width: 20}, Priority: 1, MinWidth: 4},
	}
	for _, avail := range []int{4, 10, 20, 30} {
		titles := columnTitles(FitColumns(cols, avail))
		if !slices.Contains(titles, "PINNED") {
			t.Errorf("fitColumns(avail=%d) dropped the priority -1 column: %v", avail, titles)
		}
		// Note what discriminates what. Under a `p == 0` regression it is the
		// PRESENCE of ZERO in the fixture that exposes the bug (it frees rule
		// 4a from having to protect PINNED); the assertion just below cannot
		// fail there, because `p == 0` still pins a priority-0 column. This
		// assertion guards the other side: a `p < 0` sentinel would pin PINNED
		// and drop ZERO, and only this line notices.
		if !slices.Contains(titles, "ZERO") {
			t.Errorf("fitColumns(avail=%d) dropped the priority 0 column: %v", avail, titles)
		}
	}
	if titles := columnTitles(FitColumns(cols, 10)); !slices.Equal(titles, []string{"PINNED", "ZERO"}) {
		t.Errorf("fitColumns(avail=10) = %v, want [PINNED ZERO] — only DROPPABLE may go", titles)
	}
}

// TestProjectRowPanicsWithADiagnosticMessage pins that a short row fails loudly
// rather than as a bare index-out-of-range, which is close to undiagnosable
// once a raw-mode alt-screen TUI has restored the terminal.
func TestProjectRowPanicsWithADiagnosticMessage(t *testing.T) {
	fitted := []fittedColumn{
		{index: 0, col: table.Column{Title: "A", Width: 4}},
		{index: 3, col: table.Column{Title: "D", Width: 4}},
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("projectRow accepted a row shorter than the declared column set, want a panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "projectRow:") || !strings.Contains(msg, "DECLARED") {
			t.Fatalf("panic value = %v, want a projectRow diagnostic naming the declared-column rule", r)
		}
	}()
	ProjectRow(fitted, table.Row{"a", "b"}) // only 2 cells; index 3 is out of range
}

func TestFitColumnsSqueezesWidestTowardMinWidth(t *testing.T) {
	// Only priority-0 columns, so the drop loop cannot help and every case
	// exercises the squeeze loop.
	makeCols := func() []ResponsiveColumn {
		return []ResponsiveColumn{
			{Col: table.Column{Title: "SMALL", Width: 10}, Priority: 0, MinWidth: 4},
			{Col: table.Column{Title: "WIDE", Width: 30}, Priority: 0, MinWidth: 5},
		}
	}
	tests := []struct {
		name  string
		avail int
		want  []int
	}{
		// Declared width 42. Over by 2: the widest column absorbs all of it.
		{name: "widest absorbs the overflow", avail: 40, want: []int{10, 28}},
		// Over by 22, still above WIDE's floor of 5.
		{name: "squeezes toward but not past the floor", avail: 20, want: []int{10, 8}},
		// Over by 27: WIDE floors at 5, then SMALL takes the residual 2.
		{name: "spills onto the next widest at the floor", avail: 15, want: []int{8, 5}},
		// Both columns floored (4+1 + 5+1 = 11) and still over: accept overflow
		// rather than emit a sub-floor (or zero) width.
		{name: "accepts overflow at the floor", avail: 6, want: []int{4, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FitColumns(makeCols(), tt.avail)
			gotWidths := make([]int, len(got))
			for i, c := range got {
				gotWidths[i] = c.Width
			}
			if !reflect.DeepEqual(gotWidths, tt.want) {
				t.Fatalf("fitColumns(avail=%d) widths = %v, want %v", tt.avail, gotWidths, tt.want)
			}
		})
	}
}

func TestFitColumnsIsPureAndDeterministic(t *testing.T) {
	cols := testResponsiveColumns()
	before := declaredColumns(cols)

	for _, avail := range append([]int{1, 5, 10}, responsiveTestWidths...) {
		first := FitColumns(cols, avail)
		second := FitColumns(cols, avail)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("fitColumns(avail=%d) not deterministic: %v vs %v", avail, first, second)
		}
		// Purity guard: the descriptor slice (and the table.Columns inside it)
		// must be byte-identical after every call.
		if after := declaredColumns(cols); !reflect.DeepEqual(after, before) {
			t.Fatalf("fitColumns(avail=%d) mutated its input: %v, want %v", avail, after, before)
		}
	}
}

func TestFitColumnsIndexedPreservesOrderAndIndexes(t *testing.T) {
	cols := testResponsiveColumns()
	for _, avail := range append([]int{1, 5, 10}, responsiveTestWidths...) {
		fitted := FitColumnsIndexed(cols, avail)
		prev := -1
		for _, f := range fitted {
			if f.index < 0 || f.index >= len(cols) {
				t.Fatalf("fitColumnsIndexed(avail=%d) index %d out of range [0,%d)", avail, f.index, len(cols))
			}
			if f.index <= prev {
				t.Fatalf("fitColumnsIndexed(avail=%d) indexes not strictly increasing: %d after %d", avail, f.index, prev)
			}
			prev = f.index
			if f.col.Title != cols[f.index].Col.Title {
				t.Fatalf("fitColumnsIndexed(avail=%d) index %d titled %q, want %q",
					avail, f.index, f.col.Title, cols[f.index].Col.Title)
			}
		}
		// fitColumns is exactly the .col projection of fitColumnsIndexed.
		want := make([]table.Column, len(fitted))
		for i, f := range fitted {
			want[i] = f.col
		}
		if got := FitColumns(cols, avail); !reflect.DeepEqual(got, want) {
			t.Fatalf("fitColumns(avail=%d) = %v, want projection %v", avail, got, want)
		}
	}
}

func TestWidthTierConstantsOrdering(t *testing.T) {
	// The narrow tier is reserved for genuinely small windows, so it must end
	// below the classic 80-column terminal.
	if NarrowWidthMax >= 80 {
		t.Fatalf("narrowWidthMax = %d, want < 80 so an 80-column terminal is not narrow", NarrowWidthMax)
	}
	if NarrowWidthMax >= CompactWidthMax {
		t.Fatalf("narrowWidthMax = %d, compactWidthMax = %d, want narrow < compact", NarrowWidthMax, CompactWidthMax)
	}
	// The proof harness renders at 140 columns; that width must stay in the
	// full tier so every committed capture keeps the pre-BOS-572 board.
	if CompactWidthMax >= 140 {
		t.Fatalf("compactWidthMax = %d, want < 140 so the proof default stays full-tier", CompactWidthMax)
	}
}

func TestFitColumnsNeverReturnsAnEmptySet(t *testing.T) {
	// Every column droppable — Home's cursor column is priority 0, but the
	// primitive must not assume its callers declare one. An empty result would
	// render nothing while the rows still exist.
	cols := []ResponsiveColumn{
		{Col: table.Column{Title: "A", Width: 20}, Priority: 1, MinWidth: 4},
		{Col: table.Column{Title: "B", Width: 20}, Priority: 2, MinWidth: 4},
		{Col: table.Column{Title: "C", Width: 20}, Priority: 3, MinWidth: 4},
	}
	for _, avail := range append([]int{1, 2, 5, 10}, responsiveTestWidths...) {
		got := FitColumns(cols, avail)
		if len(got) == 0 {
			t.Fatalf("fitColumns(all-droppable, avail=%d) returned an empty set", avail)
		}
		for _, c := range got {
			if c.Width <= 0 {
				t.Fatalf("fitColumns(all-droppable, avail=%d) column %q width %d, want > 0", avail, c.Title, c.Width)
			}
		}
	}
	// The survivor is the LEAST expendable of the set: the highest priorities
	// are dropped first, so "A" is what is left standing.
	if got := columnTitles(FitColumns(cols, 5)); !reflect.DeepEqual(got, []string{"A"}) {
		t.Fatalf("fitColumns(all-droppable, avail=5) = %v, want the least expendable column [A]", got)
	}
}

func TestProjectRow(t *testing.T) {
	full := table.Row{"cursor", "attn", "repo", "name", "pr", "status"}

	t.Run("drops exactly the dropped columns cells", func(t *testing.T) {
		fitted := []fittedColumn{
			{index: 0, col: table.Column{Title: " "}},
			{index: 1, col: table.Column{Title: " "}},
			{index: 3, col: table.Column{Title: "NAME"}},
			{index: 5, col: table.Column{Title: "STATUS"}},
		}
		want := table.Row{"cursor", "attn", "name", "status"}
		if got := ProjectRow(fitted, full); !reflect.DeepEqual(got, want) {
			t.Fatalf("projectRow = %v, want %v", got, want)
		}
	})

	t.Run("identity when nothing was dropped", func(t *testing.T) {
		fitted := make([]fittedColumn, len(full))
		for i := range full {
			fitted[i] = fittedColumn{index: i}
		}
		if got := ProjectRow(fitted, full); !reflect.DeepEqual(got, full) {
			t.Fatalf("projectRow = %v, want the row unchanged %v", got, full)
		}
	})

	t.Run("empty fitted set yields an empty row", func(t *testing.T) {
		if got := ProjectRow(nil, full); len(got) != 0 {
			// Go call notation naming the call under test, not copy: ellipsis: literal-dots ok
			t.Fatalf("projectRow(nil, ...) = %v, want empty", got)
		}
	})

	t.Run("does not alias the source row", func(t *testing.T) {
		src := table.Row{"a", "b"}
		out := ProjectRow([]fittedColumn{{index: 0}, {index: 1}}, src)
		out[0] = "mutated"
		if src[0] != "a" {
			t.Fatalf("projectRow aliased its input: src = %v", src)
		}
	})
}

func TestFitAvailWidth(t *testing.T) {
	tests := []struct {
		name    string
		width   int
		padding int
		want    int
	}{
		{name: "unknown width", width: 0, padding: 1, want: 0},
		{name: "negative width", width: -5, padding: 1, want: 0},
		{name: "padding swamps the width", width: 2, padding: 1, want: 0},
		{name: "padding swamps and would go negative", width: 1, padding: 1, want: 0},
		{name: "one column left over", width: 3, padding: 1, want: 1},
		{name: "no padding is the width itself", width: 80, padding: 0, want: 80},
		{name: "home's padding", width: 72, padding: 1, want: 70},
		{name: "wider padding", width: 140, padding: 2, want: 136},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FitAvailWidth(tt.width, tt.padding); got != tt.want {
				t.Fatalf("fitAvailWidth(%d, %d) = %d, want %d", tt.width, tt.padding, got, tt.want)
			}
		})
	}
}

func TestSetTableContentSurvivesEitherDirection(t *testing.T) {
	// bubbles renders on every setter, so one of the two assignments always
	// lands against the other's stale half; a row longer than the column set
	// panics in renderRow. Both directions must be safe, and neither may lose
	// the viewport's scroll offset.
	wide := []table.Column{{Title: "A", Width: 6}, {Title: "B", Width: 6}, {Title: "C", Width: 6}}
	narrow := []table.Column{{Title: "A", Width: 6}}
	rowsFor := func(cols []table.Column, n int) []table.Row {
		out := make([]table.Row, n)
		for i := range out {
			row := make(table.Row, len(cols))
			for j := range row {
				row[j] = fmt.Sprintf("r%dc%d", i, j)
			}
			out[i] = row
		}
		return out
	}

	tbl := NewBossTable(wide, rowsFor(wide, 40), 10)
	tbl.SetWidth(ColumnsWidth(wide))
	tbl.SetHeight(10)
	tbl.GotoBottom()
	if !strings.Contains(stripANSI(tbl.View()), "r39c0") {
		t.Fatalf("precondition: GotoBottom did not bring the last row on screen:\n%s", stripANSI(tbl.View()))
	}
	before := tbl.View()

	for _, step := range []struct {
		name string
		cols []table.Column
	}{
		{name: "narrowing", cols: narrow},
		{name: "widening", cols: wide},
		{name: "unchanged", cols: wide},
	} {
		t.Run(step.name, func(t *testing.T) {
			SetTableContent(&tbl, step.cols, rowsFor(step.cols, 40))
			tbl.SetWidth(ColumnsWidth(step.cols))
			if got := len(tbl.Columns()); got != len(step.cols) {
				t.Fatalf("columns = %d, want %d", got, len(step.cols))
			}
			for i, row := range tbl.Rows() {
				if len(row) != len(step.cols) {
					t.Fatalf("row %d has %d cells, want %d", i, len(row), len(step.cols))
				}
			}
			// Rendering is where a mismatch would have panicked.
			if tbl.View() == "" {
				t.Fatal("table rendered nothing")
			}
			// The selection must not have scrolled away.
			if !strings.Contains(stripANSI(tbl.View()), "r39c0") {
				t.Errorf("the selected row scrolled out of the viewport:\n%s", stripANSI(tbl.View()))
			}
		})
	}
	if before == "" {
		t.Fatal("precondition: the table rendered nothing before the swaps")
	}
}
