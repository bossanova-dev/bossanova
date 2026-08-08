package views

// One named handler per non-key arm of NewSessionModel.Update (BOS-528).
// Update keeps the dispatch and the trailing updateForm delegation; each
// handler here holds its arm's body verbatim and the arm's (tea.Model,
// tea.Cmd) shape. tea.KeyMsg routing lives in newsession_keys.go.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

func (m NewSessionModel) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	m.buildRepoTable()
	m.buildAgentTable()
	m.buildTypeTable()
	m.buildPRTable()
	m.buildIssueTable()
	return m, nil
}

func (m NewSessionModel) handleRepos(msg reposMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.repos = msg.repos
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

func (m NewSessionModel) handleAgents(msg agentsMsg) (tea.Model, tea.Cmd) {
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

func (m NewSessionModel) handlePRs(msg prsMsg) (tea.Model, tea.Cmd) {
	m.prs = msg.prs
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	if len(m.prs) == 0 {
		m.err = fmt.Errorf("no open PRs without an existing session")
		return m, nil
	}
	m.phase = newSessionPhasePRSelect
	// A prsMsg belongs to a newly selected repository. Start its list at the
	// first PR; resize-only rebuilds retain the cursor through buildPRTable.
	m.prTable.SetCursor(0)
	m.buildPRTable()
	return m, nil
}

func (m NewSessionModel) handleIssues(msg issuesMsg) (tea.Model, tea.Cmd) {
	// Drop stale responses: a newer search may have been issued, or the
	// user may have navigated away from the issue flow, since this fetch
	// was started. Keying off seq (rather than query) closes the window
	// where m.issueSearchQuery has not yet caught up with the latest
	// keystroke — the debounce tick only updates it when it fires.
	if msg.seq != m.issueSearchSeq {
		return m, nil
	}
	m.issuesFetching = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.trackerIssues = msg.issues
	// An empty result is fatal only on the very first (unfiltered) load —
	// after that we may legitimately be showing "no matches for <query>",
	// which the table renders fine on its own.
	if len(m.trackerIssues) == 0 && !m.issueTableReady && msg.query == "" {
		m.err = fmt.Errorf("no %s issues without an existing session", m.trackerSourceLabel())
		return m, nil
	}
	m.phase = newSessionPhaseIssueSelect
	m.issueTable.SetCursor(0)
	m.buildIssueTable()
	if len(m.issueTable.Rows()) > 0 {
		m.issueTable.GotoTop()
	}
	updateCursorColumn(&m.issueTable)
	m.issueTableReady = true
	return m, nil
}

func (m NewSessionModel) handleSpinnerTick(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m NewSessionModel) handleSearchIssuesTick(msg searchIssuesTickMsg) (tea.Model, tea.Cmd) {
	// Ignore stale ticks — a newer keystroke has superseded this one.
	if msg.seq != m.issueSearchSeq {
		return m, nil
	}
	m.issueSearchQuery = msg.query
	return m, fetchIssues(m.client, m.ctx, m.selectedRepoID, msg.query, m.trackerSource(), msg.seq)
}

func (m NewSessionModel) handleCreateStream(msg createSessionStreamMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		var connectErr *connect.Error
		if errors.As(msg.err, &connectErr) && connectErr.Code() == connect.CodeAlreadyExists {
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
	return m, readNextStreamMsg(m.createStream, nil)
}

func (m NewSessionModel) handleSetupScriptLine(msg setupScriptLineMsg) (tea.Model, tea.Cmd) {
	m.setupLines = append(m.setupLines, msg.text)
	return m, readNextStreamMsg(m.createStream, m.acceptedSess)
}

// handleStreamAccepted records the accepted session (BOS-720) and keeps
// reading. The view stays in its creating phase — showing the "initializing"
// indicator and the setup output as it arrives — until the settled frame
// navigates onward.
func (m NewSessionModel) handleStreamAccepted(msg streamSessionAcceptedMsg) (tea.Model, tea.Cmd) {
	m.acceptedSess = msg.session
	return m, readNextStreamMsg(m.createStream, msg.session)
}

func (m NewSessionModel) handleStreamCreated(msg streamSessionCreatedMsg) (tea.Model, tea.Cmd) {
	// readNextStreamMsg closes the stream on terminal events.
	m.createdSess = msg.session
	m.done = true
	captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventSessionCreated, map[string]any{
		"source": "tui",
	})
	return m, nil
}

func (m NewSessionModel) handleStreamError(msg streamErrorMsg) (tea.Model, tea.Cmd) {
	// readNextStreamMsg closes the stream on terminal events.
	var connectErr *connect.Error
	if errors.As(msg.err, &connectErr) && connectErr.Code() == connect.CodeAlreadyExists {
		m.confirmingOverwrite = true
		m.phase = newSessionPhaseForm
		m.err = nil
		return m, nil
	}
	m.err = msg.err
	m.phase = newSessionPhaseForm
	return m, nil
}

func (m NewSessionModel) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
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
