package views

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestToast_ShowRendersText verifies Show makes the text visible in View and
// that an empty toast renders nothing.
func TestToast_ShowRendersText(t *testing.T) {
	tm := newToastModel()
	if got := tm.View(80); got != "" {
		t.Errorf("empty toast View = %q, want empty", got)
	}
	tm, cmd := tm.Show("session-x: acct-a switched to acct-b")
	if cmd == nil {
		t.Error("Show returned a nil expire command")
	}
	got := tm.View(80)
	if !strings.Contains(got, "acct-a switched to acct-b") {
		t.Errorf("toast View = %q, want it to contain the shown text", got)
	}
}

// renderedColumnOf returns the terminal column (0-based) at which needle first
// appears in a rendered block, measured in display cells so a wide rune or a
// leading style escape cannot skew the arithmetic.
func renderedColumnOf(t *testing.T, rendered, needle string) int {
	t.Helper()
	for _, line := range strings.Split(stripANSI(rendered), "\n") {
		if idx := strings.Index(line, needle); idx >= 0 {
			return lipgloss.Width(line[:idx])
		}
	}
	t.Fatalf("needle %q not found in rendered block:\n%s", needle, rendered)
	return -1
}

// firstNonSpaceColumn returns the column (0-based) of the first non-space
// character in the first line of rendered that has one.
func firstNonSpaceColumn(t *testing.T, rendered string) int {
	t.Helper()
	for _, line := range strings.Split(stripANSI(rendered), "\n") {
		if idx := strings.IndexFunc(line, func(r rune) bool { return r != ' ' }); idx >= 0 {
			return lipgloss.Width(line[:idx])
		}
	}
	t.Fatalf("rendered block has no non-space content:\n%s", rendered)
	return -1
}

// TestToast_ViewHasNoEmojiDecoration pins the removal of the "🔄 " prefix: the
// rotation notice is plain text in the info color, with nothing pictographic
// prepended (BOS-506). A decoration creeping back in would also shift the
// notice out of alignment with the table caret.
func TestToast_ViewHasNoEmojiDecoration(t *testing.T) {
	const text = "session-x: acct-a switched to acct-b"
	tm, _ := newToastModel().Show(text)
	got := stripANSI(tm.View(80))

	if strings.Contains(got, "🔄") {
		t.Errorf("toast View = %q, want the 🔄 decoration removed", got)
	}
	for _, r := range got {
		if r == '\n' || r == ' ' {
			continue
		}
		decorative := unicode.Is(unicode.So, r) || (r > unicode.MaxASCII && !strings.ContainsRune(text, r))
		if decorative {
			t.Errorf("toast View = %q contains non-ASCII decoration %q; want plain text only", got, r)
		}
	}
	// The notice itself must start the line: nothing precedes it but padding.
	firstLine := strings.Split(got, "\n")[0]
	if !strings.HasPrefix(strings.TrimLeft(firstLine, " "), text) {
		t.Errorf("toast first line = %q, want it to start with the notice text %q", firstLine, text)
	}
}

// TestToast_ViewEndsWithBlankSeparatorLine verifies the toast owns the blank
// line that separates it from the view below (BOS-506): View emits the padded
// notice followed by a trailing newline, so its rendered block is two lines and
// its final line is empty.
func TestToast_ViewEndsWithBlankSeparatorLine(t *testing.T) {
	const text = "session-x: acct-a switched to acct-b"
	tm, _ := newToastModel().Show(text)
	got := tm.View(80)

	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("toast View = %q, want it to end with a trailing newline", got)
	}
	if h := lipgloss.Height(got); h != 2 {
		t.Errorf("lipgloss.Height(View) = %d, want 2 (notice + blank separator)", h)
	}
	lines := strings.Split(stripANSI(got), "\n")
	if last := lines[len(lines)-1]; strings.TrimSpace(last) != "" {
		t.Errorf("last line = %q, want a blank separator line", last)
	}
	// The line before the separator is the notice, padded out to the width.
	if notice := lines[len(lines)-2]; !strings.Contains(notice, text) {
		t.Errorf("line before separator = %q, want it to contain the notice text", notice)
	}
}

// TestToast_AlignsWithTableCursorChevron is the point of BOS-506: the rotation
// notice must start at the same column as the table's selection caret, not butt
// against the left edge of the terminal. Both columns are measured from real
// renders at the same width, so the test states the intent rather than
// re-asserting a hard-coded indent on both sides.
func TestToast_AlignsWithTableCursorChevron(t *testing.T) {
	const width = 80

	// Build a table exactly the way the list views do: cursor column first,
	// caret written by updateCursorColumn, whole table wrapped in the shared
	// Padding(0, 1) frame (see HomeModel.View / RepoListModel.View).
	cols := []table.Column{
		cursorColumn,
		{Title: "NAME", Width: maxColWidth("NAME", []string{"session-x"}, 30) + tableColumnSep},
	}
	rows := []table.Row{{"", "session-x"}, {"", "session-y"}}
	tbl := newBossTable(cols, rows, len(rows)+1)
	tbl.SetWidth(columnsWidth(cols))
	updateCursorColumn(&tbl)
	renderedTable := lipgloss.NewStyle().Padding(0, 1).Render(tbl.View())

	tm, _ := newToastModel().Show("session-x: acct-a switched to acct-b")

	caretCol := renderedColumnOf(t, renderedTable, cursorChevron)
	toastCol := firstNonSpaceColumn(t, tm.View(width))
	if toastCol != caretCol {
		t.Errorf("toast starts at column %d, table caret at column %d; want them aligned\ntable:\n%s\ntoast:\n%s",
			toastCol, caretCol, stripANSI(renderedTable), stripANSI(tm.View(width)))
	}
}

// TestToast_HeightMatchesRenderedLines verifies Height reports exactly what
// View occupies, including the blank separator and any wrapped continuation
// lines, so App.View can reserve those rows out of the active view's height.
func TestToast_HeightMatchesRenderedLines(t *testing.T) {
	if got := newToastModel().Height(80); got != 0 {
		t.Errorf("hidden toast Height = %d, want 0", got)
	}

	short, _ := newToastModel().Show("session-x: acct-a switched to acct-b")
	if got := short.Height(80); got != 2 {
		t.Errorf("short toast Height(80) = %d, want 2 (notice + blank separator)", got)
	}

	long, _ := newToastModel().Show(strings.Repeat("a long rotation notice ", 6))
	got := long.Height(20)
	if got <= 2 {
		t.Errorf("wrapped toast Height(20) = %d, want > 2 (wrapped continuation lines)", got)
	}
	if want := lipgloss.Height(long.View(20)); got != want {
		t.Errorf("wrapped toast Height(20) = %d, want %d (the height View actually occupies)", got, want)
	}
}

// A long notice wraps at the terminal width, and EVERY wrapped continuation
// line keeps the 2-column indent — not just the first. lipgloss pads each line
// of the block, but that is the property the acceptance criterion names, so pin
// it rather than trusting the library (BOS-506).
func TestToast_WrappedLinesKeepTheIndent(t *testing.T) {
	const width = 24

	tm, _ := newToastModel().Show(strings.Repeat("a long rotation notice ", 6))
	rendered := tm.View(width)
	if h := lipgloss.Height(rendered); h < 4 {
		t.Fatalf("toast Height(%d) = %d, want >= 4 so there is at least one continuation line to check", width, h)
	}

	// The absolute indent is pinned by TestToast_AlignsWithTableCursorChevron;
	// what this test adds is that every continuation line matches the first.
	indent := firstNonSpaceColumn(t, rendered)
	for i, line := range strings.Split(stripANSI(rendered), "\n") {
		idx := strings.IndexFunc(line, func(r rune) bool { return r != ' ' })
		if idx < 0 {
			continue // the trailing blank separator carries no text to indent
		}
		// Measure in display cells, the same unit firstNonSpaceColumn returns.
		if got := lipgloss.Width(line[:idx]); got != indent {
			t.Errorf("wrapped line %d starts at column %d, want %d\n%q", i, got, indent, line)
		}
	}
}

func TestToastDurationIsSixSeconds(t *testing.T) {
	if got, want := toastDuration, 6*time.Second; got != want {
		t.Fatalf("toastDuration = %s, want %s", got, want)
	}
}

// TestToast_ExpireClearsByID verifies a matching-id expire clears the toast,
// while a stale id (from a superseded toast) is ignored.
func TestToast_ExpireClearsByID(t *testing.T) {
	tm := newToastModel()
	tm, _ = tm.Show("first")

	// A stale expire (id from before the current toast) must not clear it.
	tm, consumed := tm.Update(toastExpireMsg{id: tm.id - 1})
	if consumed {
		t.Error("expire msg reported consumed; want false")
	}
	if tm.View(80) == "" {
		t.Error("stale expire cleared the toast; want it to remain visible")
	}

	// The matching expire clears it.
	tm, _ = tm.Update(toastExpireMsg{id: tm.id})
	if got := tm.View(80); got != "" {
		t.Errorf("toast View = %q after matching expire, want empty", got)
	}
}

// TestToast_EscDismisses verifies Esc clears a visible toast and reports the
// key consumed, while Esc on an empty toast is not consumed.
func TestToast_EscDismisses(t *testing.T) {
	tm := newToastModel()
	if _, consumed := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); consumed {
		t.Error("Esc on empty toast reported consumed; want false")
	}

	tm, _ = tm.Show("visible")
	tm, consumed := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !consumed {
		t.Error("Esc on visible toast reported not consumed; want true")
	}
	if got := tm.View(80); got != "" {
		t.Errorf("toast View = %q after Esc, want empty", got)
	}
}

// TestDetectNewRotationEvents_SeedThenDiff verifies the first observation seeds
// silently, an unchanged newest event produces no toast, and a changed newest
// event produces exactly one toast naming the session.
func TestDetectNewRotationEvents_SeedThenDiff(t *testing.T) {
	sess := &pb.Session{
		Id:    "sess-1",
		Title: "Fix the bug",
		RotationEvents: []*pb.RotationEvent{{
			Id:          "ev-1",
			Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
			FromAccount: "acct-a",
			ToAccount:   "acct-b",
		}},
	}

	// Seed pass (prev == nil): no toasts, map recorded.
	seen, toasts := detectNewRotationEvents(nil, []*pb.Session{sess})
	if len(toasts) != 0 {
		t.Errorf("seed pass toasts = %v, want none", toasts)
	}
	if seen["sess-1"] != "ev-1" {
		t.Errorf("seed map[sess-1] = %q, want ev-1", seen["sess-1"])
	}

	// Unchanged newest event: no toast.
	seen, toasts = detectNewRotationEvents(seen, []*pb.Session{sess})
	if len(toasts) != 0 {
		t.Errorf("unchanged toasts = %v, want none", toasts)
	}

	// New rotation event lands: exactly one toast naming the session.
	sess.RotationEvents = []*pb.RotationEvent{{
		Id:          "ev-2",
		Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		FromAccount: "acct-b",
		ToAccount:   "acct-c",
	}}
	seen, toasts = detectNewRotationEvents(seen, []*pb.Session{sess})
	if len(toasts) != 1 {
		t.Fatalf("changed toasts = %v, want exactly one", toasts)
	}
	if !strings.Contains(toasts[0], "Fix the bug") || !strings.Contains(toasts[0], "acct-b switched to acct-c") {
		t.Errorf("toast = %q, want session title + rotation label", toasts[0])
	}
	if seen["sess-1"] != "ev-2" {
		t.Errorf("post-diff map[sess-1] = %q, want ev-2", seen["sess-1"])
	}
}

// TestDetectNewRotationEvents_UnspecifiedFallsBackToDetail ensures a rotation
// event with no meaningful outcome label (UNSPECIFIED, e.g. a BOS-409
// stale-port notice) still produces an informative toast — the detail string,
// not a bare "<title>: " with the label dropped.
func TestDetectNewRotationEvents_UnspecifiedFallsBackToDetail(t *testing.T) {
	sess := &pb.Session{
		Id:    "sess-1",
		Title: "Fix the bug",
		RotationEvents: []*pb.RotationEvent{{
			Id:      "ev-1",
			Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		}},
	}
	seen, _ := detectNewRotationEvents(nil, []*pb.Session{sess}) // seed pass

	sess.RotationEvents = []*pb.RotationEvent{{
		Id:      "ev-2",
		Outcome: pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
		Detail:  "stale failover-proxy port (BOS-409)",
	}}
	_, toasts := detectNewRotationEvents(seen, []*pb.Session{sess})
	if len(toasts) != 1 {
		t.Fatalf("toasts = %v, want exactly one", toasts)
	}
	if !strings.Contains(toasts[0], "stale failover-proxy port (BOS-409)") {
		t.Errorf("toast = %q, want the detail string as fallback label", toasts[0])
	}
	if strings.HasSuffix(toasts[0], ": ") {
		t.Errorf("toast = %q, should not end with a bare title separator", toasts[0])
	}
}
