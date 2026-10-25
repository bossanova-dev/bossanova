package newsession

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"

	// One named handler per non-key arm of Model.Update (BOS-528).
	// Update keeps the dispatch and the trailing updateForm delegation; each
	// handler here holds its arm's body verbatim and the arm's (tea.Model,
	// tea.Cmd) shape. tea.KeyMsg routing lives in newsession_keys.go.

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/domain/rpcerr"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/telemetry"
)

func (m Model) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	m.buildRepoTable()
	m.buildAgentTable()
	m.buildTypeTable()
	m.buildPRTable()
	m.buildIssueTable()
	return m, nil
}

func (m Model) handleRepos(msg ReposMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		m.err = msg.Err
		return m, nil
	}
	m.repos = msg.Repos
	slices.SortFunc(m.repos, func(a, b *pb.Repo) int {
		return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
	})
	if len(m.repos) == 1 {
		m.selectedRepoID = m.repos[0].Id
		return m.advanceFromRepo(), nil
	}
	m.phase = newSessionPhaseRepoSelect
	m.buildRepoTable()
	return m, nil
}

func (m Model) handleAgents(msg agentsMsg) (tea.Model, tea.Cmd) {
	// Agent fetch errors are non-fatal — m.agents stays empty which
	// collapses the wizard to its single-agent shape. The daemon will
	// still serve the implicit default at create time.
	if msg.err == nil {
		m.agents = m.filterEnabledAgents(msg.agents)
	}
	if m.phase == newSessionPhaseTypeSelect && m.selectedRepoID != "" && m.initialAgent == "" {
		if len(m.agents) > 1 {
			m.phase = newSessionPhaseAgentSelect
			m.buildAgentTable()
			return m, nil
		}
		if len(m.agents) == 1 {
			m.initialAgent = m.agents[0].Name
			m.preferredAgent = m.initialAgent
			return m.advanceToType(), nil
		}
	}
	return m, nil
}

func (m Model) handlePRs(msg PrsMsg) (tea.Model, tea.Cmd) {
	m.prs = msg.PRs
	if msg.Err != nil {
		m.err = msg.Err
		return m, nil
	}
	if len(m.prs) == 0 {
		m.err = fmt.Errorf("no open PRs without an existing session")
		return m, nil
	}
	m.phase = newSessionPhasePRSelect
	// A PrsMsg belongs to a newly selected repository. Start its list at the
	// first PR; resize-only rebuilds retain the cursor through buildPRTable.
	m.prTable.SetCursor(0)
	m.buildPRTable()
	return m, nil
}

func (m Model) handleIssues(msg IssuesMsg) (tea.Model, tea.Cmd) {
	// Drop stale responses: a newer search may have been issued, or the
	// user may have navigated away from the issue flow, since this fetch
	// was started. Keying off seq (rather than query) closes the window
	// where m.issueSearchQuery has not yet caught up with the latest
	// keystroke — the debounce tick only updates it when it fires.
	if msg.Seq != m.issueSearchSeq {
		return m, nil
	}
	m.issuesFetching = false
	if msg.Err != nil {
		m.pendingIssueActivate = false
		m.err = msg.Err
		return m, nil
	}
	m.trackerIssues = msg.Issues
	// An empty result is fatal only on the very first (unfiltered) load —
	// after that we may legitimately be showing "no matches for <query>",
	// which the table renders fine on its own.
	if len(m.trackerIssues) == 0 && !m.issueTableReady && msg.Query == "" {
		m.err = fmt.Errorf("no %s issues without an existing session", m.trackerSourceLabel())
		return m, nil
	}
	m.err = nil
	m.phase = newSessionPhaseIssueSelect
	m.issueTable.SetCursor(0)
	m.buildIssueTable()
	if len(m.issueTable.Rows()) > 0 {
		m.issueTable.GotoTop()
	}
	ui.UpdateCursorColumn(&m.issueTable)
	m.issueTableReady = true
	if m.pendingIssueActivate {
		m.pendingIssueActivate = false
		cmd := m.activateHighlightedIssue()
		return m, cmd
	}
	return m, nil
}

func (m Model) handleSpinnerTick(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m Model) handleSearchIssuesTick(msg searchIssuesTickMsg) (tea.Model, tea.Cmd) {
	// Ignore stale ticks — a newer keystroke has superseded this one.
	if msg.seq != m.issueSearchSeq {
		return m, nil
	}
	m.issueSearchQuery = msg.query
	return m, fetchIssues(m.client, m.ctx, m.selectedRepoID, msg.query, m.trackerSource(), msg.seq)
}

func (m Model) handleCreateStream(msg createSessionStreamMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	if msg.err != nil {
		if m.streamCancel != nil {
			m.streamCancel()
		}
		if rpcerr.KindOf(msg.err) == rpcerr.AlreadyExists {
			m.confirmingOverwrite = true
			m.phase = newSessionPhaseForm
			m.err = nil
			return m, nil
		}
		m.err = msg.err
		m.phase = newSessionPhaseForm
		return m, nil
	}
	m.createStream = msg.stream
	return m, readNextStreamMsg(m.createStream, nil, m.streamID)
}

// handleSetupScriptLine folds one setup-output frame into the pane's buffer.
//
// It routes through foldSetupLine rather than appending unconditionally
// (BOS-1237): the setup child has no TTY, so a progress bar newline-terminates
// every redraw and each redraw arrives as its own frame. Appending them all
// filled the pane's ten-element window with one bar and evicted every other line
// of setup output.
func (m Model) handleSetupScriptLine(msg setupScriptLineMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	m.setupLines = foldSetupLine(m.setupLines, msg.text)
	return m, m.readNextSetupFrame()
}

// handleStreamAccepted records the accepted session (BOS-720) and keeps
// reading. The view stays in its creating phase — showing the "initializing"
// indicator and the setup output as it arrives — until the settled frame
// navigates onward.
func (m Model) handleStreamAccepted(msg streamSessionAcceptedMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	m.acceptedSess = msg.session
	return m, readNextStreamMsg(m.createStream, msg.session, m.streamID)
}

func (m Model) handleStreamCreated(msg streamSessionCreatedMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	if m.streamCancel != nil {
		m.streamCancel()
	}
	// readNextStreamMsg closes the stream on terminal events.
	m.createdSess = msg.session
	m.done = true
	ui.CaptureViewTelemetry(m.ctx, m.telemetry, telemetry.EventSessionCreated, map[string]any{
		"source": "tui",
	})
	return m, nil
}

func (m Model) handleStreamError(msg streamErrorMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	if m.streamCancel != nil {
		m.streamCancel()
	}
	// readNextStreamMsg closes the stream on terminal events.
	if rpcerr.KindOf(msg.err) == rpcerr.AlreadyExists {
		m.confirmingOverwrite = true
		m.phase = newSessionPhaseForm
		m.err = nil
		return m, nil
	}
	m.err = msg.err
	m.phase = newSessionPhaseForm
	return m, nil
}

func (m Model) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.phase == newSessionPhasePRSelect && m.prFilter.Active() {
		cmd := m.updatePRFilterInput(msg)
		return m, cmd
	}
	if m.phase == newSessionPhaseIssueSelect && m.issueFilter.Active() {
		cmd := m.updateIssueFilterInput(msg)
		return m, cmd
	}
	if model, cmd, ok := m.updateForm(msg); ok {
		return model, cmd
	}
	return m, nil
}
