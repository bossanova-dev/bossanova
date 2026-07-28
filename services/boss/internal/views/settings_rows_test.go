package views

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// settingsRowCase names one hand-rolled settings screen — the row editors that
// never went through huh — together with a substring of the row the cursor
// starts on and, where the screen has more than one selectable row, a substring
// of one it does not. Session settings has a single row, so its blurredRow is
// empty and the parity half of each assertion is skipped there.
type settingsRowCase struct {
	name       string
	view       func(t *testing.T) string
	cursorRow  string
	blurredRow string
	// blurredRowLocator picks the blurred row's line when blurredRow is not
	// unique in the view. Account edit needs it: its read-only Details block
	// repeats "Status: active" above the editable rows, and only the editable
	// one carries the "enter toggles" hint.
	blurredRowLocator string
}

// blurredLocator returns the substring that identifies the blurred row's line,
// which is the row text itself unless the case had to disambiguate.
func (c settingsRowCase) blurredLocator() string {
	if c.blurredRowLocator != "" {
		return c.blurredRowLocator
	}
	return c.blurredRow
}

// settingsRowCases builds the four screens BOS-567 moved onto renderFieldRow.
// Each returns a fully rendered view rather than a model so the assertions run
// against what the user actually sees, including the padding each screen's own
// View wraps its rows in.
func settingsRowCases() []settingsRowCase {
	return []settingsRowCase{
		{
			name: "general settings",
			view: func(t *testing.T) string {
				t.Helper()
				withTempConfigHome(t)
				m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
				return m.View().Content
			},
			cursorRow:  "Worktree base directory:",
			blurredRow: "Poll interval (seconds):",
		},
		{
			name: "repository settings",
			view: func(t *testing.T) string {
				t.Helper()
				stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1", DisplayName: "Test Repo"}}}
				m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
				loaded, _ := m.Update(m.Init()())
				return loaded.(RepoSettingsModel).View().Content
			},
			cursorRow:  "Name: Test Repo",
			blurredRow: "Merge strategy:",
		},
		{
			name: "account edit",
			view: func(t *testing.T) string {
				t.Helper()
				m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
					Id:     "acct-1",
					Label:  "prod-key",
					Status: "active",
				})
				return m.View().Content
			},
			cursorRow:         "Label: prod-key",
			blurredRow:        "Status: active",
			blurredRowLocator: "(enter toggles active/disabled)",
		},
		{
			name: "session settings",
			view: func(t *testing.T) string {
				t.Helper()
				m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "sess-title"}})
				return m.View().Content
			},
			cursorRow: "Name: sess-title",
		},
	}
}

// renderedLineContaining returns the raw (still-escaped) line of view whose
// visible text contains needle. The line is returned with its ANSI intact so
// colour assertions still have something to look at; matching happens against
// the stripped text so a needle never has to know where a style boundary fell.
func renderedLineContaining(t *testing.T, view, needle string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(stripANSI(line), needle) {
			return line
		}
	}
	t.Fatalf("no rendered line contains %q; view:\n%s", needle, stripANSI(view))
	return ""
}

// textColumn returns the column needle starts at on the given rendered line.
func textColumn(t *testing.T, line, needle string) int {
	t.Helper()
	plain := stripANSI(line)
	idx := strings.Index(plain, needle)
	if idx < 0 {
		t.Fatalf("line %q does not contain %q", plain, needle)
	}
	return lipgloss.Width(plain[:idx])
}

// TestSettingsRowsCarryFocusGutter is ask #1 of BOS-567: the four hand-rolled
// settings screens must mark their cursor row the way a focused huh field is
// marked — the same coloured gutter — instead of the "❯ " chevron they each
// copy-pasted. Before the change every one of these screens fails here, because
// none of them drew a gutter at all.
//
// The colour half leans on paintedIn, which cannot tell colorSelected from
// colorInfo (they share #4CA7F8), so it is deliberately paired with the glyph
// assertion above it and the column assertions in
// TestSettingsRowsKeepTheirLeftColumn.
func TestSettingsRowsCarryFocusGutter(t *testing.T) {
	for _, tc := range settingsRowCases() {
		t.Run(tc.name, func(t *testing.T) {
			view := tc.view(t)

			cursorLine := renderedLineContaining(t, view, tc.cursorRow)
			idx := strings.IndexRune(cursorLine, focusGutterGlyph)
			if idx < 0 {
				t.Fatalf("cursor row %q draws no focus gutter %q; it must carry the same chrome as a focused huh field\nline: %q",
					tc.cursorRow, string(focusGutterGlyph), stripANSI(cursorLine))
			}
			// Everything up to and including the glyph is the gutter itself:
			// the row's own text has not started yet, so a colorSelected triple
			// found here came from the gutter and not from the row content.
			gutter := cursorLine[:idx+utf8.RuneLen(focusGutterGlyph)]
			if !paintedIn(t, gutter, colorSelected) {
				t.Errorf("cursor row %q draws an uncoloured gutter %q; an uncoloured ┃ reads as box drawing, not focus",
					tc.cursorRow, gutter)
			}

			if tc.blurredRow == "" {
				return
			}
			blurredLine := renderedLineContaining(t, view, tc.blurredLocator())
			if strings.ContainsRune(blurredLine, focusGutterGlyph) {
				t.Errorf("non-cursor row %q also draws the focus gutter; only the cursor row may\nline: %q",
					tc.blurredRow, stripANSI(blurredLine))
			}
			// The blurred row still has to reserve the gutter's columns, or the
			// row text jumps sideways as the cursor moves down the screen.
			if got, want := textColumn(t, blurredLine, tc.blurredRow), textColumn(t, cursorLine, tc.cursorRow); got != want {
				t.Errorf("non-cursor row text starts at column %d but the cursor row's starts at column %d; the blurred gutter must reserve the same width",
					got, want)
			}
		})
	}
}

// TestSettingsRowsKeepTheirLeftColumn is the guard on the gutter arithmetic.
// The chevron idiom put the glyph at column 2 and the row text at column 4
// (a Padding(0, 2) wrapper around "❯ "), and the gutter has to land on exactly
// those columns: PaddingLeft(2), then the gutter's one-column border, then its
// one-column padding. Nothing else in the package asserts these columns, so
// without this a one-column shift across four screens ships unnoticed. Modelled
// on TestCronFormView_GutterAlignsWithTitle, which pins the same columns for the
// huh side.
func TestSettingsRowsKeepTheirLeftColumn(t *testing.T) {
	const (
		wantGutterColumn = 2
		wantTextColumn   = 4
	)

	for _, tc := range settingsRowCases() {
		t.Run(tc.name, func(t *testing.T) {
			view := tc.view(t)

			cursorLine := renderedLineContaining(t, view, tc.cursorRow)
			if got := textColumn(t, cursorLine, tc.cursorRow); got != wantTextColumn {
				t.Errorf("cursor row text starts at column %d, want %d\nline: %q", got, wantTextColumn, stripANSI(cursorLine))
			}
			if got := textColumn(t, cursorLine, string(focusGutterGlyph)); got != wantGutterColumn {
				t.Errorf("focus gutter sits at column %d, want %d\nline: %q", got, wantGutterColumn, stripANSI(cursorLine))
			}

			if tc.blurredRow == "" {
				return
			}
			blurredLine := renderedLineContaining(t, view, tc.blurredLocator())
			if got := textColumn(t, blurredLine, tc.blurredRow); got != wantTextColumn {
				t.Errorf("non-cursor row text starts at column %d, want %d\nline: %q", got, wantTextColumn, stripANSI(blurredLine))
			}
		})
	}
}

// repoSettingsChildRowView renders repository settings with both integrations
// configured — so Linear and Sentry open expanded (initExpansionState) and their
// indented child rows are navigable — with the cursor parked on cursorRow.
func repoSettingsChildRowView(t *testing.T, cursorRow rowID) string {
	t.Helper()
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:           "repo-1",
		DisplayName:  "Test Repo",
		LinearApiKey: "lin_api_existing_key_1234",
		SentryApiKey: "sntrys_existing_key_5678",
		SentryOrg:    "acme",
	}}}
	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	loaded, _ := m.Update(m.Init()())
	rm := loaded.(RepoSettingsModel)
	cursorToRow(t, &rm, cursorRow)
	return rm.View().Content
}

// TestSettingsChildRowsCarryFocusGutter closes the last hole in ask #1: repository
// settings' *indented* integration child rows ("API key", "Organization slug")
// are fields the cursor can land on, so a screen that gutters its top-level rows
// and chevrons its child rows is inconsistent with itself — exactly what BOS-567
// exists to remove. The child rows must carry the same gutter while keeping the
// two columns of indent that mark them as nested under their integration header:
// the glyph moves from column 2 to column 4, the text stays at column 6.
func TestSettingsChildRowsCarryFocusGutter(t *testing.T) {
	const (
		wantChildGutterColumn = 4
		wantChildTextColumn   = 6
		wantHeaderTextColumn  = 4
	)

	view := repoSettingsChildRowView(t, repoSettingsRowSentryOrg)

	cursorLine := renderedLineContaining(t, view, "Organization slug:")
	idx := strings.IndexRune(cursorLine, focusGutterGlyph)
	if idx < 0 {
		t.Fatalf("focused child row draws no focus gutter %q; nested fields must carry the same chrome as top-level ones\nline: %q",
			string(focusGutterGlyph), stripANSI(cursorLine))
	}
	// Everything up to and including the glyph is the gutter itself, so a
	// colorSelected triple found here came from the gutter, not the row text.
	if gutter := cursorLine[:idx+utf8.RuneLen(focusGutterGlyph)]; !paintedIn(t, gutter, colorSelected) {
		t.Errorf("focused child row draws an uncoloured gutter %q; an uncoloured ┃ reads as box drawing, not focus", gutter)
	}
	if got := textColumn(t, cursorLine, string(focusGutterGlyph)); got != wantChildGutterColumn {
		t.Errorf("child row gutter sits at column %d, want %d\nline: %q", got, wantChildGutterColumn, stripANSI(cursorLine))
	}
	if got := textColumn(t, cursorLine, "Organization slug:"); got != wantChildTextColumn {
		t.Errorf("focused child row text starts at column %d, want %d — child rows must keep their indent\nline: %q",
			got, wantChildTextColumn, stripANSI(cursorLine))
	}

	// The blurred sibling reserves the same columns, or the text jumps sideways
	// as the cursor moves between child rows.
	blurredLine := renderedLineContaining(t, view, "API key:")
	if strings.ContainsRune(blurredLine, focusGutterGlyph) {
		t.Errorf("non-cursor child row also draws the focus gutter; only the cursor row may\nline: %q", stripANSI(blurredLine))
	}
	if got := textColumn(t, blurredLine, "API key:"); got != wantChildTextColumn {
		t.Errorf("blurred child row text starts at column %d, want %d\nline: %q", got, wantChildTextColumn, stripANSI(blurredLine))
	}

	// The indent is only meaningful against the header it nests under: that row
	// keeps the top-level text column, two to the left of its children.
	headerLine := renderedLineContaining(t, view, "[x] Sentry")
	if got := textColumn(t, headerLine, "[x] Sentry"); got != wantHeaderTextColumn {
		t.Errorf("integration header text starts at column %d, want %d — the child indent is measured from it\nline: %q",
			got, wantHeaderTextColumn, stripANSI(headerLine))
	}
}
