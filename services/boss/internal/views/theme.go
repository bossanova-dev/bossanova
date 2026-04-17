package views

import (
	"image/color"
	"os"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
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
	styleStatusInfo    = lipgloss.NewStyle().Foreground(colorInfo)
	styleStatusMuted   = lipgloss.NewStyle().Foreground(colorMuted)
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
		Header:   lipgloss.NewStyle().Bold(true).Faint(true).Padding(0, 0, 0, 1),
		Cell:     lipgloss.NewStyle().Padding(0, 0, 0, 1),
		Selected: lipgloss.NewStyle().Bold(true).Foreground(colorSelected),
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

// maxColWidth returns the maximum width needed for a column, given its header
// and a set of values, capped at cap.
func maxColWidth(header string, values []string, cap int) int {
	w := lipgloss.Width(header)
	for _, v := range values {
		if vw := lipgloss.Width(v); vw > w {
			w = vw
		}
	}
	if w > cap {
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

// MaxColWidth is the exported version of maxColWidth for use by cmd/ package.
func MaxColWidth(header string, values []string, cap int) int {
	return maxColWidth(header, values, cap)
}

// --- Action Bar Helper ---

const actionBarSeparator = " · "

// actionBar builds a grouped action bar string. Each group's items are joined
// with double-space, then groups are joined with " · ". The result is wrapped
// in styleActionBar.Render().
func actionBar(groups ...[]string) string {
	var parts []string
	for _, g := range groups {
		if len(g) > 0 {
			parts = append(parts, strings.Join(g, "  "))
		}
	}
	return styleActionBar.Render(strings.Join(parts, actionBarSeparator))
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

	// Screen-specific overrides (used when session/repo are nil).
	line1 string
	line2 string
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
		// Session title with clickable tracker ID and PR number.
		title := renderTrackerLink(opts.session, opts.session.Title)
		if prLink := renderPRLink(opts.session); prLink != "" {
			title = prLink + " " + title
		}
		if prStatus := renderSessionPRStatus(opts.session, opts.spinner); prStatus != "" {
			title += " (" + prStatus + ")"
		}
		line1 = title

		// Worktree root path.
		wt := opts.session.GetWorktreePath()
		if home, err := os.UserHomeDir(); err == nil {
			wt = strings.Replace(wt, home, "~", 1)
		}
		line2 = styleSubtle.Render(wt)

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
		line2 = styleSubtle.Render("v" + buildinfo.Version)
	}

	banner := colorize(row1) + "  " + line1 + "\n" +
		colorize(row2) + "  " + line2

	return lipgloss.NewStyle().Padding(1, 1, 1, 1).Render(banner)
}
