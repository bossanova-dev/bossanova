package views

// Table construction and layout arithmetic for the new-session wizard — a row
// builder per selection phase, the height reservations the repo, PR and issue
// tables claim, and the filters deciding which rows reach a table
// (filterEnabledAgents, applyPRFilter, applyIssueFilter). accountRowLabel is
// here because it lived in newsession.go, but it is a package-wide account-row
// formatter that accounts_list.go, account_actions.go and chatpicker_switch.go
// read too. Split out of newsession.go (BOS-528); the declarations are
// unchanged.

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const newSessionTableBlockPadding = 1

func tableCursor(t table.Model, rows int) int {
	if rows == 0 {
		return 0
	}
	return min(t.Cursor(), rows-1)
}

// rebuildTable updates a wizard table without discarding its viewport. A table
// constructor starts at offset zero, and SetCursor does not reveal a restored
// cursor below that first page after a terminal resize.
func (m NewSessionModel) rebuildTable(t *table.Model, cols []responsiveColumn, rows []table.Row, height, cursor int) {
	fitted := fitColumnsIndexed(cols, fitAvailWidth(m.width, newSessionTableBlockPadding))
	projected := make([]table.Row, len(rows))
	for i, row := range rows {
		projected[i] = projectRow(fitted, row)
	}
	fittedCols := fittedColumns(fitted)
	if len(t.Columns()) == 0 {
		*t = newBossTable(fittedCols, projected, height)
		t.SetWidth(columnsWidth(fittedCols))
	} else {
		setTableContent(t, fittedCols, projected)
		t.SetWidth(columnsWidth(fittedCols))
		t.SetHeight(height)
	}
	t.SetCursor(cursor)
	if len(t.Rows()) > 0 {
		t.MoveDown(0)
	}
}

func (m *NewSessionModel) buildRepoTable() {
	names := make([]string, len(m.repos))
	paths := make([]string, len(m.repos))
	for i, r := range m.repos {
		names[i] = r.DisplayName
		paths[i] = r.LocalPath
	}

	cols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep}, priority: 0, minWidth: 1},
		{col: table.Column{Title: "PATH", Width: maxColWidth("PATH", paths, 60) + tableColumnSep}, priority: 1, minWidth: 1},
	}

	cursor := tableCursor(m.repoTable, len(m.repos))
	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i], paths[i]}
	}

	m.rebuildTable(&m.repoTable, cols, rows, m.repoTableHeight(), cursor)
}

// repoTableHeight returns the height for the repo selection table.
func (m NewSessionModel) repoTableHeight() int {
	footerLines := actionBarLineCount(m.width, []string{"[enter] select"}, []string{"[esc] back"})
	return clampedTableHeight(len(m.repos), m.height, bannerOverhead+5+footerLines) // header + gaps + action bar
}

// buildAgentTable populates m.agentTable from m.agents with a single AGENT
// column. The cursor column matches the other phase tables.
func (m *NewSessionModel) buildAgentTable() {
	names := make([]string, len(m.agents))
	for i, a := range m.agents {
		names[i] = a.Name
	}
	cursor := agentIndex(m.agents, m.preferredAgent)
	if cursor < 0 {
		cursor = 0
	}
	if len(m.agentTable.Rows()) > 0 {
		cursor = tableCursor(m.agentTable, len(m.agents))
	}

	cols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "AGENT", Width: maxColWidth("AGENT", names, 20) + tableColumnSep}, priority: 0, minWidth: 1},
	}

	rows := make([]table.Row, len(m.agents))
	for i := range m.agents {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i]}
	}

	m.rebuildTable(&m.agentTable, cols, rows, len(m.agents)+1, cursor)
}

func (m NewSessionModel) filterEnabledAgents(agents []client.AgentInfo) []client.AgentInfo {
	if !m.filterAgentsBySettings {
		return agents
	}
	out := make([]client.AgentInfo, 0, len(agents))
	for _, agent := range agents {
		if m.enabledAgentNames[agent.Name] {
			out = append(out, agent)
		}
	}
	return out
}

// accountRowLabel renders an account's picker label, falling back to the id
// when it has no human-facing label.
func accountRowLabel(a *pb.Account) string {
	if a.GetLabel() != "" {
		return a.GetLabel()
	}
	return a.GetId()
}

func (m *NewSessionModel) buildTypeTable() {
	cols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "", Width: 24 + tableColumnSep}, priority: 0, minWidth: 1},
		{col: table.Column{Title: "", Width: 46 + tableColumnSep}, priority: 1, minWidth: 1},
	}
	opts := m.buildSessionTypeOptions()
	optionTypes := make([]sessionType, len(opts))
	for i, opt := range opts {
		optionTypes[i] = opt.typ
	}
	cursor := 0
	if slices.Equal(m.typeTableOptionTypes, optionTypes) {
		cursor = tableCursor(m.typeTable, len(opts))
	}
	rows := make([]table.Row, len(opts))
	for i, opt := range opts {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.rebuildTable(&m.typeTable, cols, rows, len(opts)+1, cursor)
	m.typeTableOptionTypes = optionTypes
}

// buildSessionTypeOptions returns available session types based on repo configuration.
func (m *NewSessionModel) buildSessionTypeOptions() []sessionTypeOption {
	opts := []sessionTypeOption{
		{"Create a new PR", "Start a fresh branch and pull request", sessionTypeNewPR},
		{"Work on an existing PR", "Attach to an open pull request", sessionTypeExistingPR},
		{"Quick Chat", "Work directly in the repo's base folder", sessionTypeQuickChat},
	}

	// Add tracker-issue options if the repo has the relevant credentials. Each
	// is inserted before "Quick Chat", in order, so Sentry lands directly after
	// Linear. The slices are rebuilt explicitly to avoid aliasing surprises from
	// append-in-place when both options are present.
	repo := m.selectedRepo()
	var inserts []sessionTypeOption
	if repo != nil && repo.LinearApiKey != "" {
		inserts = append(inserts, sessionTypeOption{"Work on a Linear issue", "Pick an issue from your Linear board", sessionTypeLinearTicket})
	}
	if repo != nil && repo.SentryApiKey != "" && repo.SentryOrg != "" {
		inserts = append(inserts, sessionTypeOption{"Fix a Sentry issue", "Pick an issue from your Sentry organization", sessionTypeSentryIssue})
	}
	if len(inserts) > 0 {
		merged := make([]sessionTypeOption, 0, len(opts)+len(inserts))
		merged = append(merged, opts[:2]...)
		merged = append(merged, inserts...)
		merged = append(merged, opts[2:]...)
		opts = merged
	}

	return opts
}

// applyPRFilter rebuilds m.prsFiltered based on the current prFilter query.
func (m *NewSessionModel) applyPRFilter() {
	m.prsFiltered = m.prsFiltered[:0]
	for i, pr := range m.prs {
		hay := fmt.Sprintf("#%d %s %s", pr.Number, pr.Title, pr.HeadBranch)
		if m.prFilter.Matches(hay) {
			m.prsFiltered = append(m.prsFiltered, i)
		}
	}
	m.prFilter.SetCounts(len(m.prsFiltered), len(m.prs))
}

func (m *NewSessionModel) buildPRTable() {
	// Always re-apply the filter so m.prsFiltered reflects current m.prs.
	// Without this, stale indices from a previous fetch can point past the end
	// of a shorter new m.prs and panic in the row loop below.
	m.applyPRFilter()
	n := len(m.prsFiltered)
	numbers := make([]string, n)
	titles := make([]string, n)
	branches := make([]string, n)
	for j, i := range m.prsFiltered {
		pr := m.prs[i]
		numbers[j] = fmt.Sprintf("#%d", pr.Number)
		titles[j] = pr.Title
		branches[j] = pr.HeadBranch
	}

	cols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "PR", Width: maxColWidth("PR", numbers, 10) + tableColumnSep}, priority: 1, minWidth: 1},
		{col: table.Column{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep}, priority: 0, minWidth: 1},
		{col: table.Column{Title: "BRANCH", Width: maxColWidth("BRANCH", branches, 30) + tableColumnSep}, priority: 2, minWidth: 1},
	}

	cursor := tableCursor(m.prTable, n)
	rows := make([]table.Row, n)
	for j := range m.prsFiltered {
		indicator := ""
		if j == cursor {
			indicator = cursorChevron
		}
		rows[j] = table.Row{indicator, numbers[j], titles[j], styleSubtle.Render(branches[j])}
	}

	m.rebuildTable(&m.prTable, cols, rows, m.prTableHeight(), cursor)
}

// prTableHeight returns the height for the PR selection table.
func (m NewSessionModel) prTableHeight() int {
	footerLines := prSelectActionBarLineCount(m.width, m.prFilter, len(m.prsFiltered) > 0)
	return clampedTableHeight(len(m.prsFiltered), m.height, bannerOverhead+5+footerLines+m.prFilter.Height())
}

// applyIssueFilter rebuilds m.issuesFiltered based on the current issueFilter query.
func (m *NewSessionModel) applyIssueFilter() {
	m.issuesFiltered = m.issuesFiltered[:0]
	query := m.issueFilter.Query()
	// When the displayed issues were already fetched from the tracker with this
	// exact query, the server has applied it. Structured/tag queries (e.g.
	// "environment:production" for Sentry) match server-side but won't appear
	// verbatim in the rendered columns, so re-running the local substring filter
	// would hide valid server matches and show "no matches". Trust the server's
	// result set in that case.
	serverFiltered := strings.TrimSpace(query) != "" && query == m.issueSearchQuery
	for i, issue := range m.trackerIssues {
		// Include the rendered description (culprit, tags, stacktrace) in the
		// haystack so local narrowing — used as you type, before the debounced
		// server fetch returns — can match tag/metadata content, not just the
		// ID/title/state columns.
		hay := issue.ExternalId + " " + issue.Title + " " + issue.State + " " + issue.Description
		if serverFiltered || m.issueFilter.Matches(hay) {
			m.issuesFiltered = append(m.issuesFiltered, i)
		}
	}
	m.issueFilter.SetCounts(len(m.issuesFiltered), len(m.trackerIssues))
}

func (m *NewSessionModel) buildIssueTable() {
	// Always re-apply the filter so m.issuesFiltered reflects current
	// m.trackerIssues. Without this, stale indices from a previous fetch can
	// point past the end of a shorter new m.trackerIssues and panic below.
	m.applyIssueFilter()
	n := len(m.issuesFiltered)
	ids := make([]string, n)
	titles := make([]string, n)
	states := make([]string, n)
	for j, i := range m.issuesFiltered {
		issue := m.trackerIssues[i]
		ids[j] = issue.ExternalId
		titles[j] = issue.Title
		states[j] = issue.State
	}

	cols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "ID", Width: maxColWidth("ID", ids, 10) + tableColumnSep}, priority: 1, minWidth: 1},
		{col: table.Column{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep}, priority: 0, minWidth: 1},
		{col: table.Column{Title: "STATE", Width: maxColWidth("STATE", states, 15) + tableColumnSep}, priority: 2, minWidth: 1},
	}

	cursor := tableCursor(m.issueTable, n)
	rows := make([]table.Row, n)
	for j := range m.issuesFiltered {
		indicator := ""
		if j == cursor {
			indicator = cursorChevron
		}
		rows[j] = table.Row{indicator, ids[j], titles[j], styleSubtle.Render(states[j])}
	}

	m.rebuildTable(&m.issueTable, cols, rows, m.issueTableHeight(), cursor)
}

// issueTableHeight returns the height for the issue selection table.
func (m NewSessionModel) issueTableHeight() int {
	footerLines := prSelectActionBarLineCount(m.width, m.issueFilter, len(m.issuesFiltered) > 0)
	return clampedTableHeight(len(m.issuesFiltered), m.height, bannerOverhead+5+footerLines+m.issueFilter.Height())
}
