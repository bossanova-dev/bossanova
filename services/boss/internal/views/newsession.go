package views

// The new-session wizard's model, its constructor and Set* configurators, Init,
// the Update dispatcher, the View phase selector and the accessors App reads.
// Everything else the wizard needs lives in a sibling newsession_*.go file
// (BOS-528):
//
//	newsession_messages.go  the enums, *Msg types, sessionTypeOption, formData
//	newsession_cmds.go      the async tea.Cmd builders
//	newsession_handlers.go  one handler per non-key Update arm
//	newsession_keys.go      key routing, per-phase key handlers, filter inputs
//	newsession_tables.go    table construction, filters and height arithmetic
//	newsession_create.go    phase advancement, the huh form, startCreating
//	newsession_view.go      the per-phase renderers

import (
	"context"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

// NewSessionModel is the multi-step wizard for creating a new coding session.
type NewSessionModel struct {
	client    client.BossClient
	telemetry telemetry.Client
	ctx       context.Context

	phase  newSessionPhase
	err    error
	done   bool
	cancel bool

	// Data
	repos []*pb.Repo
	prs   []*pb.PRSummary

	// Linear issues
	trackerIssues   []*pb.TrackerIssue
	issueTable      table.Model
	issueTableReady bool
	selectedIssue   *pb.TrackerIssue

	// Live server-side search state. issueSearchSeq is incremented on every
	// keystroke that changes the filter query; debounce ticks and in-flight
	// fetches carry the seq they were issued with so stale responses are
	// dropped. issueSearchQuery is the query that the currently displayed
	// trackerIssues was fetched with — used both for stale-response rejection
	// and to know whether a backend refetch is needed on Esc.
	issueSearchSeq   uint64
	issueSearchQuery string
	issuesFetching   bool

	// Filters — indices into m.prs / m.trackerIssues that match the current query.
	prFilter       listFilter
	issueFilter    listFilter
	prsFiltered    []int
	issuesFiltered []int

	// Form-bound values (heap-allocated for stable pointers)
	selectedRepoID string
	selectedType   sessionType
	fd             *formData

	// Async / conflict state
	createdSess         *pb.Session
	forceBranch         bool
	confirmingOverwrite bool

	// initialAgent overrides the per-session agent at create time. Empty
	// means "let the daemon fall back to Settings.DefaultAgent".
	initialAgent string

	// preferredAgent controls the default cursor in the agent picker.
	preferredAgent string

	// onAgentSelected persists picker choices for future wizards.
	onAgentSelected func(string) error

	// Streaming create session
	createStream client.CreateSessionStream
	setupLines   []string

	// Agents (loaded once at wizard startup; drives agent-select phase).
	agents                 []client.AgentInfo
	enabledAgentNames      map[string]bool
	filterAgentsBySettings bool
	agentTable             table.Model

	// selectedAccount is the explicit `--account` CLI override. Empty plus
	// accountPicked means `--account=` was present and should send a
	// present-empty account_id (account 0 opt-out); empty plus !accountPicked
	// means the flag was omitted and the daemon applies its default policy.
	selectedAccount string
	initialAccount  string
	// initialAccountSet records whether the `--account` flag was present at all
	// (even as an empty string). It distinguishes an explicit `--account=`
	// opt-out to account 0 from an omitted flag: only the former binds account 0
	// and skips the default-account policy.
	initialAccountSet bool
	// accountPicked is true only for an explicit present-empty `--account=`.
	accountPicked bool

	// Tables
	repoTable table.Model
	typeTable table.Model
	prTable   table.Model

	// Form
	form *huh.Form
	// formFields is the ordered field slice handed to huh.NewGroup, retained
	// because huh.Form exposes no enumeration; it backs click-to-focus (BOS-512).
	formFields formFields

	// Spinner for the creating phase.
	spinner spinner.Model

	// Layout
	width  int
	height int
}

// SetTelemetry installs a telemetry client for successful session creation.
func (m *NewSessionModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// NewNewSessionModel creates a NewSessionModel wired to the daemon client.
func NewNewSessionModel(c client.BossClient, ctx context.Context) NewSessionModel {
	return NewSessionModel{
		client:      c,
		ctx:         ctx,
		phase:       newSessionPhaseLoading,
		prFilter:    newListFilter(),
		issueFilter: newListFilter(),
		spinner:     newStatusSpinner(),
	}
}

// SetInitialAgent overrides the agent for the session created by this wizard.
// Empty means "use Settings.DefaultAgent" — the daemon falls back automatically
// when the request omits agent_name.
func (m *NewSessionModel) SetInitialAgent(name string) {
	m.initialAgent = name
	m.preferredAgent = name
}

// SetPreferredAgent sets the default cursor in the agent picker without
// bypassing the picker.
func (m *NewSessionModel) SetPreferredAgent(name string) {
	m.preferredAgent = name
}

// SetAgentSettings applies user settings to the loaded daemon agents. The
// daemon may still report runners that were disabled in settings until it is
// restarted, so the wizard filters locally before deciding whether to show the
// agent picker.
func (m *NewSessionModel) SetAgentSettings(settings config.Settings) {
	enabled := make(map[string]bool)
	for _, plugin := range settings.Plugins {
		if plugin.Enabled {
			enabled[plugin.Name] = true
		}
	}
	if len(enabled) == 0 {
		m.enabledAgentNames = nil
		m.filterAgentsBySettings = false
		return
	}
	m.enabledAgentNames = enabled
	m.filterAgentsBySettings = true
	if settings.DefaultAgent != "" {
		m.preferredAgent = settings.DefaultAgent
	}
}

// SetAgentSelectionHandler registers a callback for confirmed picker choices.
func (m *NewSessionModel) SetAgentSelectionHandler(fn func(string) error) {
	m.onAgentSelected = fn
}

// SetInitialAccount overrides the account for the session created by this
// wizard. Empty is meaningful when the flag was present: it sends a
// present-empty account_id so the daemon binds account 0.
func (m *NewSessionModel) SetInitialAccount(account string) {
	m.initialAccount = account
	m.initialAccountSet = true
}

func (m NewSessionModel) Init() tea.Cmd {
	return tea.Batch(fetchRepos(m.client, m.ctx), fetchAgents(m.client, m.ctx), m.spinner.Tick)
}

// Update routes each message to a named handler. The arm order below is the
// order the inline type switch used and is behaviour: the first matching arm
// wins and every arm returns, so the trailing form delegation is reached only
// by message types no arm claims.
func (m NewSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)
	case reposMsg:
		return m.handleRepos(msg)
	case agentsMsg:
		return m.handleAgents(msg)
	case prsMsg:
		return m.handlePRs(msg)
	case issuesMsg:
		return m.handleIssues(msg)
	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)
	case searchIssuesTickMsg:
		return m.handleSearchIssuesTick(msg)
	case createSessionStreamMsg:
		return m.handleCreateStream(msg)
	case setupScriptLineMsg:
		return m.handleSetupScriptLine(msg)
	case streamSessionCreatedMsg:
		return m.handleStreamCreated(msg)
	case streamErrorMsg:
		return m.handleStreamError(msg)
	case tea.PasteMsg:
		return m.handlePaste(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Delegate to form.
	if model, cmd, ok := m.updateForm(msg); ok {
		return model, cmd
	}

	return m, nil
}

// Cancelled returns true if the user cancelled the wizard.
func (m NewSessionModel) Cancelled() bool { return m.cancel }

// Done returns true if session creation succeeded.
func (m NewSessionModel) Done() bool { return m.done }

// CreatedSession returns the session created by the wizard, or nil.
func (m NewSessionModel) CreatedSession() *pb.Session { return m.createdSess }

// View selects the renderer for the current wizard state. The guard order
// below is the order of the original if-chain and is behaviour.
func (m NewSessionModel) View() tea.View {
	if m.err != nil && !m.confirmingOverwrite {
		return tea.NewView(m.renderErr())
	}

	if m.phase == newSessionPhaseLoading {
		return tea.NewView(m.renderLoading())
	}

	if m.phase == newSessionPhaseRepoSelect {
		return tea.NewView(m.renderRepoSelect())
	}

	if m.phase == newSessionPhaseAgentSelect {
		return tea.NewView(m.renderAgentSelect())
	}

	if m.phase == newSessionPhaseTypeSelect {
		return tea.NewView(m.renderTypeSelect())
	}

	if m.phase == newSessionPhasePRSelect {
		return tea.NewView(m.renderPRSelect())
	}

	if m.phase == newSessionPhaseIssueSelect {
		return tea.NewView(m.renderIssueSelect())
	}

	if m.phase == newSessionPhaseCreating {
		return tea.NewView(m.renderCreating())
	}

	if m.done && m.createdSess != nil {
		return tea.NewView(m.renderDone())
	}

	if m.confirmingOverwrite {
		return tea.NewView(m.renderConfirmOverwrite())
	}

	if m.form != nil {
		v := tea.NewView(m.renderForm())
		// Mouse reporting is scoped to form screens (BOS-512); every phase
		// above keeps the zero value MouseModeNone.
		v.MouseMode = tea.MouseModeCellMotion
		return v
	}

	return tea.NewView("")
}
