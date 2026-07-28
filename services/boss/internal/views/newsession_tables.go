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
	"strings"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (m *NewSessionModel) buildRepoTable() {
	names := make([]string, len(m.repos))
	paths := make([]string, len(m.repos))
	for i, r := range m.repos {
		names[i] = r.DisplayName
		paths[i] = r.LocalPath
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep},
		{Title: "PATH", Width: maxColWidth("PATH", paths, 60) + tableColumnSep},
	}

	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i], paths[i]}
	}

	m.repoTable = newBossTable(cols, rows, m.repoTableHeight())
	m.repoTable.SetWidth(columnsWidth(cols))
}

// repoTableHeight returns the height for the repo selection table.
func (m NewSessionModel) repoTableHeight() int {
	return clampedTableHeight(len(m.repos), m.height, bannerOverhead+6) // header + gaps + action bar
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

	cols := []table.Column{
		cursorColumn,
		{Title: "AGENT", Width: maxColWidth("AGENT", names, 20) + tableColumnSep},
	}

	rows := make([]table.Row, len(m.agents))
	for i := range m.agents {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i]}
	}

	m.agentTable = newBossTable(cols, rows, len(m.agents)+1)
	m.agentTable.SetCursor(cursor)
	m.agentTable.SetWidth(columnsWidth(cols))
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
	cols := []table.Column{
		cursorColumn,
		{Title: "", Width: 24 + tableColumnSep},
		{Title: "", Width: 46 + tableColumnSep},
	}
	opts := m.buildSessionTypeOptions()
	rows := make([]table.Row, len(opts))
	for i, opt := range opts {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.typeTable = newBossTable(cols, rows, len(opts)+1)
	m.typeTable.SetWidth(columnsWidth(cols))
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

	cols := []table.Column{
		cursorColumn,
		{Title: "PR", Width: maxColWidth("PR", numbers, 10) + tableColumnSep},
		{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep},
		{Title: "BRANCH", Width: maxColWidth("BRANCH", branches, 30) + tableColumnSep},
	}

	rows := make([]table.Row, n)
	for j := range m.prsFiltered {
		indicator := ""
		if j == 0 {
			indicator = cursorChevron
		}
		rows[j] = table.Row{indicator, numbers[j], titles[j], styleSubtle.Render(branches[j])}
	}

	m.prTable = newBossTable(cols, rows, m.prTableHeight())
	m.prTable.SetWidth(columnsWidth(cols))
}

// prTableHeight returns the height for the PR selection table.
func (m NewSessionModel) prTableHeight() int {
	return clampedTableHeight(len(m.prsFiltered), m.height, bannerOverhead+6+m.prFilter.Height())
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

	cols := []table.Column{
		cursorColumn,
		{Title: "ID", Width: maxColWidth("ID", ids, 10) + tableColumnSep},
		{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep},
		{Title: "STATE", Width: maxColWidth("STATE", states, 15) + tableColumnSep},
	}

	rows := make([]table.Row, n)
	for j := range m.issuesFiltered {
		indicator := ""
		if j == 0 {
			indicator = cursorChevron
		}
		rows[j] = table.Row{indicator, ids[j], titles[j], styleSubtle.Render(states[j])}
	}

	m.issueTable = newBossTable(cols, rows, m.issueTableHeight())
	m.issueTable.SetWidth(columnsWidth(cols))
}

// issueTableHeight returns the height for the issue selection table.
func (m NewSessionModel) issueTableHeight() int {
	return clampedTableHeight(len(m.issuesFiltered), m.height, bannerOverhead+6+m.issueFilter.Height())
}
