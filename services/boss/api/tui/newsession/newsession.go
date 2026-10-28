package newsession

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"

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

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/telemetry"
)

// Model is the multi-step wizard for creating a new coding session.
type Model struct {
	client    Client
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
	issueTable      ui.Table
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
	// pendingIssueActivate defers an Enter pressed during live search until
	// the matching result set lands, so stale rows are never activated.
	pendingIssueActivate bool

	// Filters — indices into m.prs / m.trackerIssues that match the current query.
	prFilter       ui.ListFilter
	issueFilter    ui.ListFilter
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
	createStream  rpc.CreateSessionStream
	watchStream   rpc.WatchSessionSetupStream
	streamID      *streamIdentity
	streamCancel  context.CancelFunc
	watchingSetup bool
	setupLines    []string
	// acceptedSess is the session the daemon's accepted SessionCreated frame
	// carried (BOS-720), or nil before it arrives. Non-nil means the session is
	// addressable even though its bootstrap is still running, and that the next
	// SessionCreated is the terminal settled one.
	acceptedSess *pb.Session

	// Agents (loaded once at wizard startup; drives agent-select phase).
	agents                 []rpc.AgentInfo
	enabledAgentNames      map[string]bool
	filterAgentsBySettings bool
	agentTable             ui.Table

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
	repoTable            ui.Table
	typeTable            ui.Table
	typeTableOptionTypes []sessionType
	prTable              ui.Table

	// Form
	form *huh.Form
	// formFields is the ordered field slice handed to huh.NewGroup, retained
	// because huh.Form exposes no enumeration; it backs click-to-focus (BOS-512).
	formFields ui.FormFields

	// Spinner for the creating phase.
	spinner spinner.Model

	// Layout
	width  int
	height int
}

// SetTelemetry installs a telemetry client for successful session creation.
func (m *Model) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// NewModel creates a Model wired to the daemon client.
func NewModel(c Client, ctx context.Context) Model {
	return Model{
		client:      c,
		ctx:         ctx,
		phase:       newSessionPhaseLoading,
		prFilter:    ui.NewListFilter(),
		issueFilter: ui.NewListFilter(),
		spinner:     ui.NewStatusSpinner(),
	}
}

// SetInitialAgent overrides the agent for the session created by this wizard.
// Empty means "use Settings.DefaultAgent" — the daemon falls back automatically
// when the request omits agent_name.
func (m *Model) SetInitialAgent(name string) {
	m.initialAgent = name
	m.preferredAgent = name
}

// SetPreferredAgent sets the default cursor in the agent picker without
// bypassing the picker.
func (m *Model) SetPreferredAgent(name string) {
	m.preferredAgent = name
}

// SetAgentSettings applies user settings to the loaded daemon agents. The
// daemon may still report runners that were disabled in settings until it is
// restarted, so the wizard filters locally before deciding whether to show the
// agent picker.
func (m *Model) SetAgentSettings(settings config.Settings) {
	enabled := make(map[string]bool)
	for _, plugin := range settings.Plugins {
		// Effective state, not the persisted flag: for an experimental plugin
		// experimental_plugins is authoritative and the daemon never persists its
		// override, so plugins[].enabled stays false for a runner it has loaded.
		if config.PluginEnabledForSettings(settings, plugin.Name) {
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
func (m *Model) SetAgentSelectionHandler(fn func(string) error) {
	m.onAgentSelected = fn
}

// SetInitialAgentAccount overrides the account for the session created by this
// wizard. Empty is meaningful when the flag was present: it sends a
// present-empty account_id so the daemon binds account 0.
func (m *Model) SetInitialAgentAccount(account string) {
	m.initialAccount = account
	m.initialAccountSet = true
}

// NewSetupModel attaches the creating screen to an existing session's bootstrap.
func NewSetupModel(c Client, ctx context.Context, session *pb.Session) Model {
	m := NewModel(c, ctx)
	m.phase = newSessionPhaseCreating
	m.acceptedSess = session
	m.watchingSetup = true
	m.streamID = &streamIdentity{}
	watchCtx, cancel := context.WithCancel(ctx)
	m.ctx = watchCtx
	m.streamCancel = cancel
	return m
}

func (m Model) Init() tea.Cmd {
	if m.watchingSetup {
		return tea.Batch(openSetupWatch(m.client, m.ctx, m.acceptedSess.GetId(), m.streamID), m.spinner.Tick)
	}
	return tea.Batch(fetchRepos(m.client, m.ctx), fetchAgents(m.client, m.ctx), m.spinner.Tick)
}

// Update routes each message to a named handler. The arm order below is the
// order the inline type switch used and is behaviour: the first matching arm
// wins and every arm returns, so the trailing form delegation is reached only
// by message types no arm claims.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.watchingSetup {
		// A wizard abandoned before setup re-entry can still deliver its RPC
		// results and search ticks. They do not belong to this watcher.
		switch msg.(type) {
		case ReposMsg, agentsMsg, PrsMsg, IssuesMsg, searchIssuesTickMsg:
			return m, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)
	case ReposMsg:
		return m.handleRepos(msg)
	case agentsMsg:
		return m.handleAgents(msg)
	case PrsMsg:
		return m.handlePRs(msg)
	case IssuesMsg:
		return m.handleIssues(msg)
	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)
	case searchIssuesTickMsg:
		return m.handleSearchIssuesTick(msg)
	case setupWatchStreamMsg:
		return m.handleSetupWatchStream(msg)
	case setupSettledMsg:
		return m.handleSetupSettled(msg)
	case createSessionStreamMsg:
		return m.handleCreateStream(msg)
	case setupScriptLineMsg:
		return m.handleSetupScriptLine(msg)
	case streamSessionAcceptedMsg:
		return m.handleStreamAccepted(msg)
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

// textEntryActive reports whether a focused text input owns keystrokes in the
// wizard's current phase — either of the two "/" list filters, or the huh form
// phase. App consults it before aliasing ctrl+x onto Esc (BOS-660).
//
// Each filter is scoped to its own picker phase, so the pairs here are exactly
// the ones handleKey delegates to keyPRFilter / keyIssueFilter on (and that
// handlePaste routes on): the question is whether the filter really consumes
// this keystroke, not whether one was left focused. Enter in keyIssueFilter
// selects the highlighted row and starts creating without blurring the filter,
// so an unscoped Active() kept claiming a text input on the creating screen and
// on the overwrite-confirm prompt and create-error notice reached from it —
// screens whose only key is Esc.
//
// The filters are checked with Active() (focused), not Engaged(): a committed
// but blurred filter is a non-text state whose Esc clears the query, and that
// is exactly the behaviour ctrl+x should inherit.
//
// The form phase is checked with formOnScreen, not the phase alone: the wizard
// stays on newSessionPhaseForm while the overwrite-confirm prompt and the
// create-error notice render INSTEAD of the completed form (see View's guard
// order), and both of those are pure y/n-and-esc screens. The phase test still
// leads because a form left non-nil by an earlier phase is not on screen either.
func (m Model) TextEntryActive() bool {
	if m.err != nil {
		return false
	}
	if m.phase == newSessionPhasePRSelect && m.prFilter.Active() {
		return true
	}
	if m.phase == newSessionPhaseIssueSelect && m.issueFilter.Active() {
		return true
	}
	return m.phase == newSessionPhaseForm && ui.FormOnScreen(m.form)
}

// Cancelled returns true if the user cancelled the wizard.
func (m Model) Cancelled() bool { return m.cancel }

// Done returns true if session creation succeeded.
func (m Model) Done() bool { return m.done }

// CreatedSession returns the session created by the wizard, or nil.
func (m Model) CreatedSession() *pb.Session { return m.createdSess }

// View selects the renderer for the current wizard state. The guard order
// below is the order of the original if-chain and is behaviour.
func (m Model) View() tea.View {
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

// Form returns the wizard's active form.
func (m Model) Form() *huh.Form { return m.form }

// FormFields returns the wizard's form fields for pointer navigation.
func (m Model) FormFields() ui.FormFields { return m.formFields }

// FormPrefix returns the content rendered above the active form.
func (m Model) FormPrefix() string { return m.formPrefix() }

// SetSize sets terminal dimensions without changing the wizard state.
func (m *Model) SetSize(width, height int) { m.width, m.height = width, height }

// SetHeight sets the height available after router decorations.
func (m *Model) SetHeight(height int) { m.height = height }

// Height returns the available terminal height.
func (m Model) Height() int { return m.height }

// Width returns the available terminal width.
func (m Model) Width() int { return m.width }

// RepoTable returns the repository picker table for router height reservation.
func (m *Model) RepoTable() *ui.Table { return &m.repoTable }

// PRTable returns the pull-request picker table for router height reservation.
func (m *Model) PRTable() *ui.Table { return &m.prTable }

// IssueTable returns the issue picker table for router height reservation.
func (m *Model) IssueTable() *ui.Table { return &m.issueTable }

// RepoTableHeight returns the repository picker's available row count.
func (m Model) RepoTableHeight() int { return m.repoTableHeight() }

// PRTableHeight returns the pull-request picker's available row count.
func (m Model) PRTableHeight() int { return m.prTableHeight() }

// IssueTableHeight returns the issue picker's available row count.
func (m Model) IssueTableHeight() int { return m.issueTableHeight() }
