package views

// Per-phase renderers for the new-session wizard (BOS-528). View stays in
// newsession.go as the guard chain that picks between them; each branch it used
// to inline lives here as a named renderer returning the body string it
// contributed. prSelectActionBar and headerView ride along as the chrome
// helpers those renderers read — those two are moves. The render* methods are
// new declarations introduced by this PR's View decomposition; the bodies they
// carry are the original if-chain's branches verbatim.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderErr renders the wizard error screen, including any captured setup
// script output.
func (m NewSessionModel) renderErr() string {
	var b strings.Builder
	b.WriteString(renderError(rpcErrorMessage(m.err), m.width))
	if len(m.setupLines) > 0 {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Foreground(colorWarning).Render("Setup script output:"))
		b.WriteString("\n")
		for _, line := range m.setupLines {
			b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Foreground(lipgloss.Color("8")).Render(line))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(actionBarWidth(m.width, []string{"[esc] back"}))
	return b.String()
}

// renderLoading renders the initial repo-fetch placeholder.
func (m NewSessionModel) renderLoading() string {
	return lipgloss.NewStyle().Padding(0, 2).Render("Loading...")
}

// renderRepoSelect renders the repo picker.
func (m NewSessionModel) renderRepoSelect() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Select a repository"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.repoTable.View()))
	b.WriteString("\n")
	b.WriteString(actionBarWidth(m.width, []string{"[enter] select"}, []string{"[esc] back"}))
	return b.String()
}

// renderAgentSelect renders the agent picker.
func (m NewSessionModel) renderAgentSelect() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(
		"Multiple agents are installed — pick one for this session."))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.agentTable.View()))
	b.WriteString("\n")
	b.WriteString(actionBarWidth(m.width, []string{"[enter] select"}, []string{"[esc] back"}))
	return b.String()
}

// renderTypeSelect renders the session-type picker.
func (m NewSessionModel) renderTypeSelect() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.typeTable.View()))
	b.WriteString("\n")
	b.WriteString(actionBarWidth(m.width, []string{"[enter] select"}, []string{"[esc] back"}))
	return b.String()
}

// renderPRSelect renders the PR picker and its filter line.
func (m NewSessionModel) renderPRSelect() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	if m.prFilter.Engaged() && len(m.prsFiltered) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render("no matches"))
		b.WriteString("\n")
	} else {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.prTable.View()))
		b.WriteString("\n")
	}
	if m.prFilter.Engaged() {
		b.WriteString(m.prFilter.View())
		b.WriteString("\n")
	}
	b.WriteString(prSelectActionBar(m.width, m.prFilter, len(m.prs) > 0))
	return b.String()
}

// renderIssueSelect renders the tracker-issue picker and its filter line.
func (m NewSessionModel) renderIssueSelect() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	if !m.issueTableReady {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("Loading %s issues...", m.trackerSourceLabel())))
	} else {
		if m.issueFilter.Engaged() && len(m.issuesFiltered) == 0 {
			placeholder := "no matches"
			if m.issuesFetching {
				placeholder = "searching…"
			}
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(placeholder))
			b.WriteString("\n")
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.issueTable.View()))
			b.WriteString("\n")
		}
		if m.issueFilter.Engaged() {
			b.WriteString(m.issueFilter.View())
			if m.issuesFetching && len(m.issuesFiltered) > 0 {
				b.WriteString(lipgloss.NewStyle().Foreground(colorMuted).Render("  searching…"))
			}
			b.WriteString("\n")
		}
		b.WriteString(prSelectActionBar(m.width, m.issueFilter, len(m.trackerIssues) > 0))
	}
	return b.String()
}

// renderCreating renders the spinner and setup-script tail while the
// CreateSession stream is open.
func (m NewSessionModel) renderCreating() string {
	var b strings.Builder
	// Render "initializing" indicator using INFO/blue style with an animated
	// Dot spinner (same style as the session-list spinner).
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(styleStatusInfo.Render(m.spinner.View() + "initializing")))
	// Blank line after "initializing" so the creating phase has more breathing
	// room before the following status line (BOS-397). The gap precedes whichever
	// message comes next ("Running setup script..." or "Creating a new session...").
	b.WriteString("\n\n")
	if len(m.setupLines) > 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Running setup script..."))
		b.WriteString("\n")
		// Show last 10 lines of setup output.
		start := 0
		if len(m.setupLines) > 10 {
			start = len(m.setupLines) - 10
		}
		for _, line := range m.setupLines[start:] {
			b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Foreground(lipgloss.Color("8")).Render(line))
			b.WriteString("\n")
		}
	} else {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Creating a new session..."))
	}
	return b.String()
}

// renderDone renders the terminal success screen. The caller must have checked
// m.createdSess != nil — inside the pre-split View that guard sat immediately
// above this body, and as a standalone method it dereferences unconditionally.
func (m NewSessionModel) renderDone() string {
	return lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render("Session created!") + "\n" +
		lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("  ID:     %s\n  Title:  %s\n  Branch: %s",
				m.createdSess.Id, m.createdSess.Title, m.createdSess.BranchName))
}

// renderConfirmOverwrite renders the branch-overwrite prompt.
func (m NewSessionModel) renderConfirmOverwrite() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
		"A branch with this name already exists."))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		"Remove the old branch and create a new session?"))
	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	return b.String()
}

// renderForm renders the huh form. The caller must have checked m.form != nil;
// like renderDone this body relied on its enclosing View guard before the
// split. The single trailing newline before the action bar is the BOS-509
// spacing contract — styleActionBar's own padding supplies the blank line, so
// do not add another here.
func (m NewSessionModel) renderForm() string {
	var b strings.Builder
	b.WriteString(m.formPrefix())
	b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()))
	// styleActionBar's Padding(actionBarPadY, 2) supplies the blank line
	// above the bar, so the form ends with a single newline and no more.
	b.WriteString("\n")
	// Both form variants (new-PR and quick-chat) submit straight into
	// CreateSession, so one verb is truthful for either phase. Kept short so
	// the verb line stays within 80 columns — the nav hints now occupy their
	// own line above it (BOS-512), so it no longer shares one with them.
	b.WriteString(formActionBarWidth(m.width, []string{"[enter] create"}, []string{"[esc] back"}))
	return b.String()
}

// formPrefix is everything renderForm writes above the huh form: the wizard
// header (empty when no repo is selected) and the blank line under it. The
// click hit test measures this same string, so a line added here moves both
// the render and the hit test together (BOS-512).
func (m NewSessionModel) formPrefix() string {
	return m.headerView() + "\n"
}

// prSelectActionBar renders the action bar for the filterable select phases
// (PR select and issue select). The bar adapts to the filter state:
//   - filtering (input focused): replaces the bar with the filter help.
//   - applied (query committed): offers to edit or clear the filter.
//   - idle: normal "[enter] select" plus discoverability hint for "/".
func prSelectActionBar(width int, f listFilter, hasItems bool) string {
	if f.Active() {
		return actionBarWidth(width, f.ActionBar())
	}
	if f.Applied() {
		return actionBarWidth(width,
			[]string{"[enter] select"},
			[]string{"[/] edit filter", "[esc] clear"},
		)
	}
	primary := []string{"[enter] select"}
	if hasItems {
		primary = append(primary, "[/] filter")
	}
	return actionBarWidth(width, primary, []string{"[esc] back"})
}

func prSelectActionBarLineCount(width int, f listFilter, hasItems bool) int {
	if f.Active() {
		return actionBarLineCount(width, f.ActionBar())
	}
	if f.Applied() {
		return actionBarLineCount(width,
			[]string{"[enter] select"},
			[]string{"[/] edit filter", "[esc] clear"},
		)
	}
	primary := []string{"[enter] select"}
	if hasItems {
		primary = append(primary, "[/] filter")
	}
	return actionBarLineCount(width, primary, []string{"[esc] back"})
}

func (m NewSessionModel) headerView() string {
	repo := m.selectedRepo()
	if repo == nil {
		return ""
	}
	bold := lipgloss.NewStyle().Bold(true)
	return lipgloss.NewStyle().Padding(0, 2).Render(
		"Starting a "+bold.Render("new session")+" for "+bold.Render(repo.DisplayName)) + "\n"
}
