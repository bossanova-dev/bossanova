package views

// The new-session wizard's phase advancement, huh form and session-creation
// concern — the tracker-source resolvers, resolveInitialAccount, the
// selectedRepo lookup, the advance* phase transitions, buildForm plus the
// updateForm delegation Update falls through to, handleFormCompleted, and
// startCreating, which assembles the CreateSessionRequest and opens the create
// stream. Split out of newsession.go (BOS-528); the declarations are unchanged.

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/trackerprompt"
)

// trackerSource maps the selected session type to the task source name the
// daemon routes on. Defaults to "linear" so any non-Sentry issue flow keeps
// its existing behaviour.
func (m *NewSessionModel) trackerSource() string {
	if m.selectedType == sessionTypeSentryIssue {
		return "sentry"
	}
	return "linear"
}

// trackerSourceLabel returns a human-readable name for the current tracker
// source, used in loading and empty-state copy.
func (m *NewSessionModel) trackerSourceLabel() string {
	if m.selectedType == sessionTypeSentryIssue {
		return "Sentry"
	}
	return "Linear"
}

func (m *NewSessionModel) resolveInitialAccount() {
	if m.initialAccountSet {
		m.selectedAccount = m.initialAccount
		if m.initialAccount == "" {
			m.accountPicked = true
		}
		return
	}
	m.selectedAccount = ""
	m.accountPicked = false
}

func (m *NewSessionModel) selectedRepo() *pb.Repo {
	for _, r := range m.repos {
		if r.Id == m.selectedRepoID {
			return r
		}
	}
	return nil
}

// advanceFromRepo moves on after a repo has been selected. With more than
// one agent loaded AND no CLI override, route to the agent picker first;
// otherwise jump straight to the session-type chooser. Returns the updated
// model so callers can return it directly to bubbletea.
func (m *NewSessionModel) advanceFromRepo() NewSessionModel {
	if len(m.agents) > 1 && m.initialAgent == "" {
		m.phase = newSessionPhaseAgentSelect
		m.buildAgentTable()
		return *m
	}
	if len(m.agents) == 1 && m.initialAgent == "" {
		m.initialAgent = m.agents[0].Name
		m.preferredAgent = m.initialAgent
	}
	return m.advanceToType()
}

// advanceToType routes straight to the session-type chooser, resolving only the
// explicit `--account` override. Without the flag, account_id stays unset so the
// daemon applies its default-account policy.
func (m *NewSessionModel) advanceToType() NewSessionModel {
	m.resolveInitialAccount()
	m.phase = newSessionPhaseTypeSelect
	m.buildTypeTable()
	return *m
}

func (m *NewSessionModel) advanceFromTypeSelect() (tea.Model, tea.Cmd) {
	switch m.selectedType {
	case sessionTypeQuickChat:
		m.phase = newSessionPhaseForm
		m.buildForm()
		return *m, m.form.Init()
	case sessionTypeExistingPR:
		// Fetch PRs, then show PR selector table.
		m.phase = newSessionPhaseLoading
		return *m, fetchPRs(m.client, m.ctx, m.selectedRepoID)
	case sessionTypeLinearTicket, sessionTypeSentryIssue:
		// Fetch tracker issues (Linear or Sentry), then show the issue selector.
		m.phase = newSessionPhaseLoading
		m.issueSearchQuery = ""
		return *m, fetchIssues(m.client, m.ctx, m.selectedRepoID, "", m.trackerSource(), m.issueSearchSeq)
	case sessionTypeNewPR:
		m.phase = newSessionPhaseForm
		m.buildForm()
		return *m, m.form.Init()
	default:
		m.cancel = true
		return *m, nil
	}
}

func (m *NewSessionModel) buildForm() {
	if m.fd == nil {
		m.fd = &formData{}
	}

	switch m.selectedType {
	case sessionTypeNewPR:
		title := huh.NewInput().
			Title("Session name").
			Placeholder("What are you working on?").
			Value(&m.fd.title).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("title is required")
				}
				return nil
			})
		m.form = newBossForm(title)
		m.formFields = newFormFields(title)

	case sessionTypeQuickChat:
		title := huh.NewInput().
			Title("Session name").
			Placeholder("Quick Chat").
			Value(&m.fd.title)
		m.form = newBossForm(title)
		m.formFields = newFormFields(title)

	case sessionTypeExistingPR, sessionTypeExecutePlan, sessionTypeLinearTicket, sessionTypeSentryIssue:
		// No form needed for these types.
	}
}

func (m NewSessionModel) updateForm(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	if m.form == nil || m.phase != newSessionPhaseForm || m.confirmingOverwrite {
		return m, nil, false
	}

	// Click-to-focus, ahead of huh: huh v2 has no mouse support, so every
	// mouse-shaped message is consumed here rather than forwarded (BOS-512).
	if cmd, handled := m.formFields.handleMouse(msg, m.form, linesBefore(m.formPrefix())); handled {
		return m, cmd, true
	}

	_, cmd := m.form.Update(msg)

	if m.form.State == huh.StateAborted {
		m.phase = newSessionPhaseTypeSelect
		m.form = nil
		m.err = nil
		m.fd = nil
		m.forceBranch = false
		return m, nil, true
	}

	if m.form.State == huh.StateCompleted {
		model, cmd := m.handleFormCompleted()
		return model, cmd, true
	}

	return m, cmd, true
}

func (m *NewSessionModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	// Title input or plan input completed — proceed to create.
	cmd := m.startCreating()
	return *m, cmd
}

// startCreating builds a CreateSessionRequest and fires the RPC.
func (m *NewSessionModel) startCreating() tea.Cmd {
	if m.phase == newSessionPhaseCreating {
		return nil
	}
	m.phase = newSessionPhaseCreating
	m.setupLines = nil // Clear setup output from any previous attempt
	// ...and the accepted session with it (BOS-720). This is not just a render
	// value: handleSetupScriptLine threads acceptedSess back into
	// readNextStreamMsg as the accepted-vs-settled discriminator, and a clean
	// EOF while holding one is reported as a successful create. A retry after
	// an attempt that got past its accepted frame would otherwise show — and
	// could attach to — a session id cleanupFailedCreateSession has deleted.
	m.acceptedSess = nil
	repo := m.selectedRepo()
	if repo == nil {
		m.err = fmt.Errorf("no repository selected")
		return nil
	}

	req := &pb.CreateSessionRequest{
		RepoId:      repo.Id,
		BaseBranch:  repo.DefaultBaseBranch,
		ForceBranch: m.forceBranch,
	}
	if m.initialAgent != "" {
		// Copy to a local so req.AgentName points at request-owned memory,
		// not the model field (which may be mutated later).
		agent := m.initialAgent
		req.AgentName = &agent
	}
	if m.selectedAccount != "" || m.accountPicked {
		// Copy to a local so req.AccountId points at request-owned memory.
		//   - selectedAccount != "": an explicit --account value.
		//   - selectedAccount == "" && accountPicked: explicit --account= sends
		//     an empty account_id so the daemon binds account 0 and skips the
		//     default-account policy.
		// When neither holds, account_id stays unset and the daemon applies its
		// default-account policy.
		account := m.selectedAccount
		req.AccountId = &account
	}

	switch m.selectedType {
	case sessionTypeQuickChat:
		var title string
		if m.fd != nil {
			title = strings.TrimSpace(m.fd.title)
		}
		if title == "" {
			title = "Quick Chat " + time.Now().Format("2006-01-02 15:04")
		}
		req.Title = title
		req.IsQuickChat = true
	case sessionTypeNewPR:
		req.Title = m.fd.title
	case sessionTypeExistingPR:
		idx := m.prTable.Cursor()
		if idx >= 0 && idx < len(m.prsFiltered) {
			pr := m.prs[m.prsFiltered[idx]]
			req.Title = pr.Title
			req.PrNumber = &pr.Number
		}
	case sessionTypeLinearTicket, sessionTypeSentryIssue:
		if m.selectedIssue != nil {
			issue := m.selectedIssue
			req.Title = fmt.Sprintf("[%s] %s", issue.ExternalId, issue.Title)
			req.Plan = trackerprompt.Format(issue, m.trackerSource())
			req.TrackerId = &issue.ExternalId
			if issue.Url != "" {
				req.TrackerUrl = &issue.Url
			}
			if issue.PrNumber > 0 {
				// Existing PR - attach to it
				prNum := issue.PrNumber
				req.PrNumber = &prNum
			} else if issue.BranchName != "" {
				// New branch using the tracker's suggested name
				req.BranchName = &issue.BranchName
			}
		}
	default:
		req.Title = "New session"
	}

	return openCreateStream(m.client, m.ctx, req)
}
