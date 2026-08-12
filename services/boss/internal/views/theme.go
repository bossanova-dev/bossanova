package views

import (
	"fmt"
	"image/color"
	"os"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Colors (semantic names, decoupled from actual color values) ---

var (
	colorSelected = lipgloss.Color("#4CA7F8") // selected row + chevron
	colorSuccess  = lipgloss.Color("#04B575") // pass, working, completion
	colorWarning  = lipgloss.Color("#DBBD70") // pending, idle
	colorDanger   = lipgloss.Color("#FF6347") // fail, error, destructive confirms
	colorInfo     = lipgloss.Color("#4CA7F8") // transitional/progress states
	colorMuted    = lipgloss.Color("#626262") // stopped, unknown, default
)

// --- Characters ---

const cursorChevron = "❯"

// --- Layout ---

const (
	shortIDLen         = 7
	actionBarPadY      = 1
	defaultTableHeight = 20
	bannerOverhead     = bannerHeight + 1 // banner lines + trailing newline added by App.View
	// statusLinePadding is the horizontal padding on every Home status/warning
	// line. It is part of the block's total width (lipgloss Width includes
	// padding), so a terminal narrower than twice this has no columns left to
	// wrap into.
	statusLinePadding = 2
	// minStatusWrapWidth is the narrowest width a Home status/warning line is
	// allowed to wrap at. Below roughly 60 columns a long error (the
	// cloud-access failure is ~250 columns) wraps into an unreadably tall
	// ribbon, so a very narrow table — or an empty state, which draws no table
	// at all — floors here rather than tracking the table exactly.
	//
	// Keep this at the readability minimum, NOT above it. The floor overrides
	// the table-derived width, so every column added here is a column the
	// status block overhangs the table by — the exact "status text runs past
	// the content block" defect BOS-507 exists to fix. A terminal narrower than
	// this still wins: the clamp to the terminal width is applied after the
	// floor (BOS-507).
	//
	// The pre-BOS-572 range was ~38..116, reaching the low end only through
	// short content, so a 60-column floor left the table-derived width in
	// charge for all but the narrowest boards. Since BOS-572 the table is fitted
	// to the terminal, so the low end is reachable by NARROWING ALONE — Home's
	// columnsWidth is 21 at a 22-column terminal — and the fitted table only
	// overtakes the floor at about 62 columns. So on any terminal at or below
	// ~61 it is now this floor, not the table, that decides the wrap width, and
	// the terminal clamp is what keeps that from overhanging. That is BOS-507's
	// intended override rather than a regression, but it is the common case on a
	// narrow board now, not the exception it used to be.
	//
	// KNOWN TRADE-OFF, deliberate: 60 is narrower than most of Home's fixed
	// status copy (the longest, statusCloudNeedsSubscriptionUpgrade, is 112
	// columns), so in the empty state — the one place the floor is the whole
	// rule, because there is no table to track — that copy renders on two rows
	// instead of one. Nothing is lost, unlike the clipping this ticket fixes,
	// and a floor wide enough to keep all of it on one row (>= 116, the copy
	// plus statusLinePadding either side) would overhang a typical ~70-column
	// board by 40+ columns. The two goals genuinely conflict and alignment
	// wins. TestHomeFixedStatusCopyStaysReadableAtFloor bounds the cost at two
	// rows against those same consts, so copy edited past it fails the build.
	minStatusWrapWidth = 60

	// narrowWidthMax and compactWidthMax bracket the three width tiers a
	// responsive board renders at:
	//
	//	width <= narrowWidthMax                          narrow  ("phone")
	//	narrowWidthMax < width <= compactWidthMax        compact ("tablet")
	//	width > compactWidthMax                          full
	//
	// The values are chosen so that today's typical 80–120 column terminals
	// land in the compact and full tiers, and the narrow tier is reserved for
	// genuinely small windows — a split pane, a phone-sized SSH client, a
	// deliberately shrunk terminal. An 80-column terminal is emphatically NOT
	// "narrow": it is the classic default, so it sits one column above the
	// narrow ceiling on purpose.
	//
	// The full tier (> compactWidthMax) must stay behaviourally identical to
	// the pre-BOS-572 board. The proof harness renders at 140 columns, and
	// ~30 committed proof scenarios plus home_test.go's committed golden
	// boards all assert that layout, so any change that alters what the full
	// tier draws is a regression, not a feature. fitColumns' "already fits"
	// rule is what mechanically guarantees this.
	//
	// KNOWN TRADE-OFF, deliberate: these two numbers are a judgement call, not
	// a measurement. Nobody has yet captured real boards at 60/72/79/80/100
	// columns to confirm the boundaries land where a reader would want them,
	// and the honest expectation is that they get tuned once those captures
	// exist. That is exactly why they are named constants declared beside the
	// other layout invariants rather than literals sprinkled through the view
	// code: retuning a tier must be a one-line edit here, with the tests that
	// pin their ordering (TestWidthTierConstantsOrdering) failing loudly if a
	// retune ever pushes 80 into the narrow tier or 140 out of the full one.
	narrowWidthMax  = 79
	compactWidthMax = 119
)

// --- TUI Styles ---

var (
	styleTitle     = lipgloss.NewStyle().Bold(true).Padding(0, 2)
	styleSelected  = lipgloss.NewStyle().Bold(true)
	styleActionBar = lipgloss.NewStyle().Faint(true).Padding(actionBarPadY, 2)
	styleError     = lipgloss.NewStyle().Foreground(colorDanger).Padding(0, 2)
	styleSubtle    = lipgloss.NewStyle().Faint(true)
)

// --- Status Styles ---

var (
	styleStatusSuccess = lipgloss.NewStyle().Foreground(colorSuccess)
	styleStatusWarning = lipgloss.NewStyle().Foreground(colorWarning)
	styleStatusDanger  = lipgloss.NewStyle().Foreground(colorDanger)
	// styleStatusDangerFaded is a dimmed danger style for warnings that no
	// longer need to alarm — e.g. a residual "finalize failed" hint on a row
	// whose PR has already merged/closed (BOS-246). Still readable as a
	// warning, but visually recessive.
	styleStatusDangerFaded = styleStatusDanger.Faint(true)
	// styleStatusWarningFaded is the warning-tier twin of styleStatusDangerFaded,
	// used for the attention "!" on a row whose every warning hint has gone
	// recessive (BOS-855). Without it the indicator would stay at full intensity
	// above a block of faded hints — the same self-contradiction the recessive
	// treatment exists to remove.
	styleStatusWarningFaded = styleStatusWarning.Faint(true)
	styleStatusInfo         = lipgloss.NewStyle().Foreground(colorInfo)
	styleStatusMuted        = lipgloss.NewStyle().Foreground(colorMuted)
	// styleToast is the rotation-notice style: the info color, indented to column 2
	// so the notice lines up with the table caret and with the rest of the TUI
	// chrome (styleTitle / styleActionBar / styleError are all Padding(0, 2)) rather
	// than butting against the left edge of the terminal (BOS-506).
	styleToast = styleStatusInfo.Padding(0, 2)
)

// --- TUI Table ---

const (
	tableColumnGap = 1 // left padding applied to every cell
	tableColumnSep = 1 // extra width added to data columns so gap between them = 2
)

var cursorColumn = table.Column{Title: " ", Width: 1}

// bossTableStyles returns table styles matching the existing TUI aesthetic:
// bold+faint header, left-padded cells, bold blue foreground for selected row.
func bossTableStyles() table.Styles {
	return table.Styles{
		Header: lipgloss.NewStyle().Bold(true).Faint(true).Padding(0, 0, 0, 1),
		Cell:   lipgloss.NewStyle().Padding(0, 0, 0, 1),
		// UnderlineColor matches the foreground so that links inside the row
		// (which emit raw SGR 4 underline ANSI) inherit the highlight color
		// instead of the terminal's default underline color.
		Selected: lipgloss.NewStyle().Bold(true).Foreground(colorSelected).UnderlineColor(colorSelected),
	}
}

// newBossTable creates a focused table with the standard boss key map and styles.
func newBossTable(cols []table.Column, rows []table.Row, height int) table.Model {
	if height <= 0 {
		height = defaultTableHeight
	}
	return table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(height),
		table.WithKeyMap(bossKeyMap()),
		table.WithStyles(bossTableStyles()),
		table.WithFocused(true),
	)
}

// updateCursorColumn sets the chevron on the selected row and clears it on all others.
func updateCursorColumn(t *table.Model) {
	rows := t.Rows()
	cursor := t.Cursor()
	for i := range rows {
		if i == cursor {
			rows[i][0] = cursorChevron
		} else {
			rows[i][0] = ""
		}
	}
	t.SetRows(rows)
}

// columnsWidth returns the total rendered width for a set of columns,
// including left-only cell padding (tableColumnGap per column).
func columnsWidth(cols []table.Column) int {
	w := 0
	for _, c := range cols {
		w += c.Width + tableColumnGap
	}
	return w
}

// responsiveColumn pairs a table.Column with the two facts a width-fitting
// policy needs: how expendable the column is, and how far it may be squeezed
// before dropping it is the better answer.
//
// Callers build these descriptors the same way they build plain columns today
// (maxColWidth over the header and the cell values) — measure with
// lipgloss.Width, never len: Home's cells carry OSC 8 hyperlinks and raw SGR
// escapes, so len over-counts a rendered cell by tens of bytes and would size
// every column wrong.
type responsiveColumn struct {
	col      table.Column
	priority int // <= 0 = never dropped; higher = dropped sooner
	minWidth int // floor when the column is kept but squeezed
}

// fittedColumn is a column that survived fitting, paired with its index in the
// descriptor slice it came from, so a caller can project its row cells onto the
// fitted set (Home's endpoint/warning sub-rows need exactly this: they build a
// cell per declared column and must drop the same ones the header did).
type fittedColumn struct {
	index int
	col   table.Column
}

// fitColumnsIndexed fits a set of column descriptors into avail rendered
// columns, dropping and then squeezing until the set fits, and reports each
// surviving column alongside its index in cols.
//
// The policy, in order:
//
//  1. Purity. cols and the table.Column values inside it are never mutated;
//     all work happens on a local copy. The same input always produces the
//     same output, so a caller may hold one descriptor slice for the lifetime
//     of a view and re-fit it on every WindowSizeMsg.
//
//  2. Unknown width. avail <= 0 means the terminal width is not known yet (no
//     tea.WindowSizeMsg has arrived). Every column is returned untouched —
//     guessing a layout from a width we do not have would make the first
//     frame flicker.
//
//  3. Already fits. If columnsWidth(cols) <= avail, every column is returned
//     untouched. This rule is LOAD-BEARING as a regression guard, not merely
//     an optimisation: 30+ committed proof scenarios and a ~121 KB
//     home_test.go assert today's 140-column board cell for cell, and this
//     early return is what keeps all of them green — at the full tier the
//     function is provably a no-op.
//
//  4. Drop loop. While the set is over budget and at least one column with
//     priority > 0 remains, drop the column with the highest priority. On
//     ties the leftmost (lowest index) column goes first, so the result is
//     deterministic rather than map-iteration-dependent. The set is
//     re-measured with columnsWidth after each drop and the loop stops the
//     moment it fits, so only as many columns are dropped as the width
//     actually requires. A non-empty input always yields a non-empty result:
//     the last column standing is never dropped, whatever its priority.
//     Note which one that is: since the loop always drops the HIGHEST
//     priority value, the survivor of an all-droppable set is the column with
//     the LOWEST priority value, wherever it sits — and among columns tied at
//     that value it is the rightmost, since ties drop leftmost-first. So the
//     floor keeps the least expendable column, not a positional one. A caller
//     must still give its identity column priority 0 rather than rely on this
//     floor: the floor exists to stop an empty set, and it silently stops
//     protecting anything the moment a second column shares the lowest
//     priority.
//
//  5. Squeeze loop. While still over budget, pick the widest remaining column
//     whose current width exceeds its floor (max(minWidth, 1)) — again
//     leftmost on ties — and reduce it by the remaining overflow, clamped at
//     that floor. The loop ends when no column can shrink any further.
//
//  6. Accept overflow rather than emit a bad width. No width this function
//     COMPUTES is ever <= 0. A width the CALLER declared non-positive is
//     passed through untouched on EVERY path, not just the early returns of
//     rules 2 and 3: the drop loop only removes columns, and the squeeze loop
//     skips any column already at or below its floor, which a non-positive
//     width always is. This function narrows columns; it does not validate
//     them, and a caller that declares a zero width gets it back.
//     Neither consumer treats a zero width as "zero columns wide": bubbles'
//     table skips such a column entirely (renderRow and headersView both
//     `continue` on Width <= 0), so the header and the cells silently vanish
//     while columnsWidth still bills a gap for them; and lipgloss reads
//     Width(0) as unconstrained — the trap both statusWrapWidth (home_view.go)
//     and renderError already document. Either way a non-positive width is
//     worse than the overflow it was meant to avoid, so when step 5 runs out
//     of slack the set is returned still over budget, on purpose.
//
//  7. Measurement is always lipgloss.Width / columnsWidth, never len. This
//     function works off declared widths so it is safe by construction, but
//     the same rule binds every caller computing those widths: cells carry
//     OSC 8 hyperlinks and raw ANSI.
//
// Empty input returns nil.
func fitColumnsIndexed(cols []responsiveColumn, avail int) []fittedColumn {
	if len(cols) == 0 {
		return nil
	}

	// Rule 1: never touch the caller's descriptors.
	kept := make([]fittedColumn, len(cols))
	for i, c := range cols {
		kept[i] = fittedColumn{index: i, col: c.col}
	}
	widths := func() []table.Column {
		out := make([]table.Column, len(kept))
		for i, f := range kept {
			out[i] = f.col
		}
		return out
	}

	// Rules 2 and 3: unknown width, or the set already fits.
	if avail <= 0 || columnsWidth(widths()) <= avail {
		return kept
	}

	// Rule 4: drop the most expendable column until the set fits.
	for columnsWidth(widths()) > avail {
		// Rule 4a: never drop the last column standing. A caller that declares
		// every column droppable would otherwise get an empty set back at a
		// small enough width, and an empty column set is not a narrow table —
		// it is a table that renders nothing while its rows still exist, and
		// any per-row cell write (updateCursorColumn's rows[i][0], say) then
		// indexes a zero-cell row and panics. One column is the floor.
		if len(kept) == 1 {
			break
		}
		drop := -1
		for i, f := range kept {
			p := cols[f.index].priority
			// <= 0, not == 0: a negative priority reads as "even less
			// droppable than 0", so it must be pinned too. Testing == 0 would
			// make it merely the LAST thing dropped — the exact inversion of
			// what a caller writing -1 intends.
			if p <= 0 {
				continue
			}
			if drop == -1 || p > cols[kept[drop].index].priority {
				// Strictly greater, so an equal priority does not displace an
				// earlier candidate: the LEFTMOST of a tied group is selected.
				drop = i
			}
		}
		if drop == -1 {
			break
		}
		kept = append(kept[:drop:drop], kept[drop+1:]...)
	}

	// Rule 5: squeeze the widest shrinkable column toward its floor.
	floor := func(f fittedColumn) int {
		return max(cols[f.index].minWidth, 1) // rule 6: never below 1
	}
	for {
		over := columnsWidth(widths()) - avail
		if over <= 0 {
			break
		}
		squeeze := -1
		for i, f := range kept {
			if f.col.Width <= floor(f) {
				continue
			}
			if squeeze == -1 || f.col.Width > kept[squeeze].col.Width {
				// Strictly greater: leftmost of the equally-widest wins.
				squeeze = i
			}
		}
		if squeeze == -1 {
			break // rule 6: overflow beats a zero/negative width
		}
		kept[squeeze].col.Width = max(kept[squeeze].col.Width-over, floor(kept[squeeze]))
	}

	return kept
}

// setTableContent installs a new column set and its matching rows on a bubbles
// table, in whichever order keeps the intermediate state renderable.
//
// bubbles has no combined setter and every setter re-renders the viewport as its
// last act, so one of the two assignments is always observed against the other's
// stale half. table.renderRow walks m.rows[r] and indexes m.cols[i] per cell, so
// the only unsafe state is a row with MORE cells than there are columns:
//
//   - fewer columns than before (a narrowing fit) — send the rows first, so the
//     shorter rows are rendered against the still-wide column set;
//   - the same or more columns — send the columns first, exactly as every
//     fixed-width table in this package always has.
//
// Getting this wrong is not a cosmetic bug: a stale six-cell row against a
// three-column set panics with an index-out-of-range and takes the whole TUI
// down. It became reachable the moment fitColumns let the column count vary —
// on a narrowing resize, and equally on an ordinary poll that widens a column
// past the budget on a terminal below the full tier (BOS-572).
//
// Emptying the rows first (SetRows(nil)) would also be safe, and is wrong for a
// different reason: viewport.SetContent CLAMPS the scroll offset, to zero for
// empty content, and bubbles exposes no way to put it back — SetCursor,
// SetWidth and SetHeight all leave the offset alone, only MoveUp/MoveDown
// maintain it. Since Home rebuilds on every keypress and every spinner tick,
// that would scroll a tall board back to the top ten times a second while the
// cursor stayed put, leaving the selected row and its ❯ caret off screen with
// Enter still acting on them. Re-ordering touches the offset not at all.
func setTableContent(t *table.Model, cols []table.Column, rows []table.Row) {
	if len(cols) < len(t.Columns()) {
		t.SetRows(rows)
		t.SetColumns(cols)
		return
	}
	t.SetColumns(cols)
	t.SetRows(rows)
}

// projectRow maps a row built in the FULL declared column order onto the set
// that survived fitting, dropping exactly the cells whose columns were dropped.
//
// Every responsive table needs this and none of them may hand-roll it. bubbles'
// table.renderRow indexes m.cols[i] for every cell in the row, so a row that
// still carries a dropped column's cell is an index-out-of-range panic, and a
// row whose cells were re-ordered by hand puts its text under the wrong header.
// Build each row — primary and sub-row alike — with one cell per declared
// column, then send it through here.
//
// full must have one cell per declared column; a shorter row is a caller bug
// and panics here rather than mis-rendering silently downstream. The panic
// carries its own message: a bare index-out-of-range from a raw-mode alt-screen
// TUI is close to undiagnosable after the terminal is restored, which is the
// same failure mode setTableContent exists to keep off the screen.
func projectRow(fitted []fittedColumn, full table.Row) table.Row {
	for _, f := range fitted {
		if f.index >= len(full) {
			panic(fmt.Sprintf(
				"projectRow: row has %d cells but the fitted set references declared column %d; build every row with one cell per DECLARED column before projecting",
				len(full), f.index))
		}
	}
	out := make(table.Row, len(fitted))
	for i, f := range fitted {
		out[i] = full[f.index]
	}
	return out
}

// fitAvailWidth returns the rendered columns a table may occupy inside a view
// that wraps it in Padding(0, padding): the terminal width less that padding on
// both sides.
//
// Returns 0 — which fitColumnsIndexed reads as "do not fit" — when the terminal
// width is unknown (no tea.WindowSizeMsg has arrived yet) or when the padding
// alone consumes it. In both cases the declared column set is drawn untouched,
// so the first frame does not flicker through a layout guessed from a width we
// do not have.
func fitAvailWidth(width, padding int) int {
	if width <= 0 {
		return 0
	}
	return max(width-padding*2, 0)
}

// fittedColumns strips the indexes off a fitted set, yielding the plain
// []table.Column bubbles' SetColumns wants. Every caller of fitColumnsIndexed
// needs this, so it lives here rather than being open-coded at each one — an
// open-coded copy is how the two forms drift.
func fittedColumns(fitted []fittedColumn) []table.Column {
	if len(fitted) == 0 {
		return nil
	}
	out := make([]table.Column, len(fitted))
	for i, f := range fitted {
		out[i] = f.col
	}
	return out
}

// fitColumns is fitColumnsIndexed for callers that do not need to project row
// cells onto the surviving set — it is exactly the .col projection of the
// indexed result. See fitColumnsIndexed for the full policy.
//
// Home does not use this form (it needs the indexes for projectRow, so it
// composes fitColumnsIndexed + fittedColumns directly), but it is the entry
// point for a table with no sub-rows, which most of the views still to be
// converted are.
func fitColumns(cols []responsiveColumn, avail int) []table.Column {
	return fittedColumns(fitColumnsIndexed(cols, avail))
}

// maxColWidth returns the maximum width needed for a column, given its header
// and a set of values, capped at cap.
func maxColWidth(header string, values []string, cap int) int {
	w := lipgloss.Width(header)
	for _, v := range values {
		if vw := lipgloss.Width(v); vw > w {
			w = vw
		}
	}
	if cap > 0 && w > cap {
		return cap
	}
	return w
}

// clampedTableHeight returns the height for a table given the number of data rows,
// total terminal height, and fixed overhead (title + gaps + action bar lines).
// Returns rows+1 (header + data) clamped to available vertical space.
func clampedTableHeight(rows, termHeight, overhead int) int {
	needed := rows + 1
	if termHeight <= 0 {
		return needed
	}
	avail := max(termHeight-overhead, 1)
	if needed < avail {
		return needed
	}
	return avail
}

// --- CLI Table (exported for cmd/ package) ---

// CLITableStyles returns table styles for non-interactive CLI output.
// Bold header, no selection highlighting needed.
func CLITableStyles() table.Styles {
	return table.Styles{
		Header:   lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle(),
	}
}

// CLIColumnsWidth returns the total rendered width for a set of columns,
// including cell padding (1 char each side per column).
func CLIColumnsWidth(cols []table.Column) int {
	w := 0
	for _, c := range cols {
		w += c.Width + 2
	}
	return w
}

// RenderCLITable renders non-interactive CLI tables with compact, deterministic
// spacing. Bubble table adds per-cell padding that can make adjacent text-heavy
// columns look separated by a variable gap.
func RenderCLITable(cols []table.Column, rows []table.Row) string {
	var b strings.Builder
	headers := make(table.Row, len(cols))
	for i, col := range cols {
		headers[i] = col.Title
	}
	writeCLITableRow(&b, cols, headers)
	for _, row := range rows {
		b.WriteByte('\n')
		writeCLITableRow(&b, cols, row)
	}
	return b.String()
}

func writeCLITableRow(b *strings.Builder, cols []table.Column, cells table.Row) {
	for i, col := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		cell := ""
		if i < len(cells) {
			cell = truncateDisplay(cells[i], col.Width)
		}
		b.WriteString(cell)
		if pad := col.Width - lipgloss.Width(cell); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
	}
}

func truncateDisplay(s string, maxWidth int) string {
	if maxWidth <= 0 || lipgloss.Width(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return strings.Repeat(".", maxWidth)
	}
	var b strings.Builder
	for _, r := range s {
		next := b.String() + string(r)
		if lipgloss.Width(next) > maxWidth-3 {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString("...")
	return b.String()
}

// MaxColWidth is the exported version of maxColWidth for use by cmd/ package.
// A non-positive cap leaves the width uncapped.
func MaxColWidth(header string, values []string, cap int) int {
	return maxColWidth(header, values, cap)
}

// --- Action Bar Helper ---

const actionBarSeparator = " · "

// actionBarText builds the unstyled text of a grouped action bar. Each group's
// items are joined with double-space, then groups are joined with " · ".
// Split out of actionBar so formActionBar can compose two such lines inside a
// single styleActionBar block.
func actionBarText(groups ...[]string) string {
	var parts []string
	for _, g := range groups {
		if len(g) > 0 {
			parts = append(parts, strings.Join(g, "  "))
		}
	}
	return strings.Join(parts, actionBarSeparator)
}

// actionBar builds a grouped action bar string. Each group's items are joined
// with double-space, then groups are joined with " · ". The result is wrapped
// in styleActionBar.Render().
func actionBar(groups ...[]string) string {
	return styleActionBar.Render(actionBarText(groups...))
}

// actionBarWidth renders a grouped action bar within width columns. It keeps a
// group intact and folds only between the visible action-bar separators, so a
// key hint never wraps in the middle of an action. Unknown widths retain the
// historical one-line rendering exactly.
func actionBarWidth(width int, groups ...[]string) string {
	if width <= 0 {
		return actionBar(groups...)
	}
	return styleActionBar.Render(strings.Join(actionBarTextLines(width, groups...), "\n"))
}

// actionBarLineCount reports the number of text lines actionBarWidth renders;
// callers that reserve vertical space for an action bar must use this rather
// than assuming a fixed line count.
func actionBarLineCount(width int, groups ...[]string) int {
	return len(actionBarTextLines(width, groups...))
}

func actionBarTextLines(width int, groups ...[]string) []string {
	if width <= 0 {
		return []string{actionBarText(groups...)}
	}
	textWidth := width - styleActionBar.GetHorizontalPadding()
	if textWidth <= 0 {
		return []string{actionBarText(groups...)}
	}

	lines := make([]string, 0, len(groups))
	line := ""
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		part := strings.Join(group, "  ")
		if ansi.StringWidth(part) > textWidth {
			// A legacy caller may place several independent actions in one
			// visual group. Preserve every complete action: when that group
			// itself cannot fit, fold between its double-space-separated items.
			if line != "" {
				lines = append(lines, line)
			}
			folded := actionBarPartsLines(width, "  ", group)
			lines = append(lines, folded[:len(folded)-1]...)
			line = folded[len(folded)-1]
			continue
		}
		if line == "" {
			line = part
			continue
		}
		if candidate := line + actionBarSeparator + part; ansi.StringWidth(candidate) <= textWidth {
			line = candidate
			continue
		}
		lines = append(lines, line)
		line = part
	}
	if line == "" {
		return []string{""}
	}
	return append(lines, line)
}

func actionBarPartsLines(width int, separator string, parts []string) []string {
	if len(parts) == 0 {
		return []string{""}
	}
	if width <= 0 {
		return []string{strings.Join(parts, separator)}
	}

	textWidth := width - styleActionBar.GetHorizontalPadding()
	if textWidth <= 0 {
		return []string{strings.Join(parts, separator)}
	}

	lines := make([]string, 0, len(parts))
	line := parts[0]
	for _, part := range parts[1:] {
		candidate := line + separator + part
		if ansi.StringWidth(candidate) <= textWidth {
			line = candidate
			continue
		}
		lines = append(lines, line)
		line = part
	}
	return append(lines, line)
}

// formNavHints are the field-movement keys every huh form shares. tab /
// shift+tab is the only pair huh binds identically across Input, Text, Select,
// MultiSelect and Confirm (see huh's keymap.go) — enter is overloaded,
// advancing an Input but submitting a Select, so it is never described as
// "next field". Every form view suppresses huh's own help footer with
// .WithShowHelp(false), so this is the only place the user learns these keys;
// keeping the wording here means it cannot drift between views.
//
// Caveat, recorded deliberately: huh disables Next on the last field and Prev
// on the first (see each field's WithPosition — keymap.Next.SetEnabled(!IsLast)),
// so on a single-field form both keys are inert. BOS-509's acceptance criteria
// require the hints on all five forms regardless, so three single-field forms
// (repo-add "open project" input, new-session, bug report) advertise keys that
// currently do nothing. Suppressing the pair below two focusable fields is the
// follow-up.
// The third hint is BOS-512's click affordance. It lives here, beside the key
// hints, so the whole "how do I move between fields" vocabulary stays in one
// place. Mouse reporting is enabled only while a form is on screen, so this is
// never advertised anywhere it would not work.
func formNavHints() []string {
	return []string{"[tab] next field", "[shift+tab] previous field", "[click] select field"}
}

// formActionBarLines is how many rendered text lines formActionBar occupies,
// excluding styleActionBar's own top/bottom padding. Views that budget vertical
// space for the bar (repoAddChrome, cronFormChrome) count against this.
const formActionBarLines = 2

// formActionBar renders the shared form action bar as two lines inside one
// styleActionBar block: the shared field-movement hints on the first, and the
// caller's submit/other actions plus its cancel/back actions on the second.
//
// Two lines is deliberate and load-bearing. The three nav hints alone are 70
// columns rendered; folded onto one line with the add-repo details form's own
// verbs the bar is 101, which hard-wraps on an 80-column terminal — and
// repoAddChrome / cronFormChrome budget a fixed number of bar lines, so a wrap
// silently clips the bar these views exist to show (the defect
// assertActionBarFitsEightyColumns was written for). Split this way both lines
// fit 80 columns by construction on every form in this package.
func formActionBar(submit []string, right []string) string {
	return formActionBarWidth(0, submit, right)
}

// formActionBarWidth renders the form action bar at the terminal width. The
// navigation hints can fold between individual hints while the submit and
// cancel groups retain the ordinary action-bar group boundary contract.
func formActionBarWidth(width int, submit []string, right []string) string {
	return styleActionBar.Render(strings.Join(formActionBarTextLines(width, submit, right), "\n"))
}

func formActionBarLineCount(width int, submit []string, right []string) int {
	return len(formActionBarTextLines(width, submit, right))
}

func formActionBarTextLines(width int, submit []string, right []string) []string {
	lines := actionBarPartsLines(width, "  ", formNavHints())
	if actions := actionBarText(submit, right); actions != "" {
		lines = append(lines, actionBarTextLines(width, submit, right)...)
	}
	return lines
}

// --- Banner ---

// bannerGradient defines a horizontal color gradient for the B icon (dawn palette).
var bannerGradient = []color.Color{
	lipgloss.Color("#00C6FF"),
	lipgloss.Color("#00AAFF"),
	lipgloss.Color("#008EFF"),
	lipgloss.Color("#0072FF"),
}

// bannerHeight is the number of lines rendered by renderBanner (including padding).
// Banner has padding(1,1,1,1) = 1 top + 2 content + 1 bottom = 4 lines.
const bannerHeight = 4

// bannerOpts carries optional per-screen overrides for the banner.
type bannerOpts struct {
	session *pb.Session
	repo    *pb.Repo
	spinner spinner.Model

	// archiving, when true, overrides the session's PR display status with the
	// animated "archiving" label. Set when the user re-enters a session whose
	// archive is still in flight (the HomeModel tracks that session's ID).
	archiving bool

	// Screen-specific overrides (used when session/repo are nil).
	line1 string
	line2 string
	width int
}

// accountBannerLabel renders the bound-account label shown beside the worktree
// line in the session banner. The Session message carries only the account id
// today (no server-provided label), so we surface that id verbatim and fall
// back to "Unmanaged" for an unbound session. Best-effort — never panics.
func accountBannerLabel(accountID string) string {
	if accountID == "" {
		return UnmanagedLocalCredentialsShortLabel
	}
	return accountID
}

func renderBanner(active View, opts bannerOpts) string {
	// Logo chars per row, matching `npx oh-my-logo "B" dawn --filled --block-font tiny`.
	row1 := []string{" ", "█", "▄", "▄"}
	row2 := []string{" ", "█", "▄", "█"}

	colorize := func(chars []string) string {
		var b strings.Builder
		for i, ch := range chars {
			b.WriteString(lipgloss.NewStyle().Foreground(bannerGradient[i]).Render(ch))
		}
		return b.String()
	}

	var line1, line2 string
	switch {
	case (active == ViewChatPicker || active == ViewSessionSettings) && opts.session != nil:
		// Session title with clickable tracker ID and PR number. Merged/closed
		// PRs get the same muted strikethrough treatment as merged rows on the
		// home screen, so the banner reflects the terminal state at a glance.
		merged := opts.session.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
			opts.session.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED
		var title string
		if merged {
			title = renderMutedTrackerLink(opts.session, opts.session.Title)
			if prLink := renderMutedPRLink(opts.session); prLink != "" {
				title = prLink + " " + title
			}
		} else {
			title = renderTrackerLink(opts.session, opts.session.Title)
			if prLink := renderPRLink(opts.session); prLink != "" {
				title = prLink + " " + title
			}
		}
		displayStatus := renderDisplayStatus(opts.session, opts.spinner)
		if opts.archiving {
			displayStatus = renderArchivingStatus(opts.spinner)
		}
		if displayStatus != "" {
			title += " (" + displayStatus + ")"
		}
		line1 = title

		// Worktree root path, followed by the bound account label (best-effort;
		// "Unmanaged" when the session is unbound).
		wt := opts.session.GetWorktreePath()
		if home, err := os.UserHomeDir(); err == nil {
			wt = strings.Replace(wt, home, "~", 1)
		}
		line2 = styleSubtle.Render(wt)
		line2 += " " + styleSubtle.Render("· "+accountBannerLabel(opts.session.GetAccountId()))

	case opts.repo != nil:
		line1 = opts.repo.DisplayName
		lp := opts.repo.LocalPath
		if home, err := os.UserHomeDir(); err == nil {
			lp = strings.Replace(lp, home, "~", 1)
		}
		line2 = styleSubtle.Render(lp)

	case opts.line1 != "":
		line1 = opts.line1
		line2 = opts.line2

	default:
		line1 = "Bossanova"
		line2 = styleSubtle.Render(buildinfo.Version)
	}

	logo1 := colorize(row1) + "  "
	logo2 := colorize(row2) + "  "
	if opts.width > 0 {
		line1 = ansi.Truncate(line1, max(opts.width-2-ansi.StringWidth(logo1), 0), "…")
		line2 = ansi.Truncate(line2, max(opts.width-2-ansi.StringWidth(logo2), 0), "…")
	}
	banner := logo1 + line1 + "\n" + logo2 + line2

	return lipgloss.NewStyle().Padding(1, 1, 1, 1).Render(banner)
}
