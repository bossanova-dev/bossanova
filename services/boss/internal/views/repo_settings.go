package views

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

// repoSettingsLoadedMsg carries the loaded repo for the settings view.
type repoSettingsLoadedMsg struct {
	repo *pb.Repo
	err  error
}

// repoSettingsSavedMsg carries the result of saving repo settings.
type repoSettingsSavedMsg struct {
	repo *pb.Repo
	err  error
}

type repoSettingsGitHubStatusMsg struct {
	repos []*pb.GitHubAppRepoStatus
	err   error
}

type repoSettingsGitHubInstallURLMsg struct {
	url     string
	err     error
	attempt int
}

type repoSettingsGitHubOpenedMsg struct {
	url     string
	err     error
	attempt int
}

// rowID is a stable identity for each logical row in the settings view. Rows are
// referenced by identity rather than position because collapsing an integration
// hides its child rows, so on-screen positions are dynamic.
type rowID int

const (
	repoSettingsRowNone rowID = iota - 1 // sentinel: not editing / no row

	repoSettingsRowName
	repoSettingsRowSetupScript
	repoSettingsRowMergeStrategy
	repoSettingsRowCanAutoMerge
	repoSettingsRowCanAutoMergeDependabot
	repoSettingsRowCanAutoRepair
	repoSettingsRowShouldArchiveSessionsAfterMerge
	repoSettingsRowCanAutoDeleteBranches
	repoSettingsRowLinearHeader
	repoSettingsRowLinearApiKey
	repoSettingsRowSentryHeader
	repoSettingsRowSentryApiKey
	repoSettingsRowSentryOrg
)

type repoSettingsGitHubMode int

const (
	repoSettingsGitHubModeNone repoSettingsGitHubMode = iota
	repoSettingsGitHubModePrompt
	repoSettingsGitHubModeOpening
)

// mergeStrategies is the cycle order for the merge strategy setting, sourced
// from the canonical models.MergeStrategies() ordering rather than duplicating
// bare string literals.
var mergeStrategies = mergeStrategyStrings()

// mergeStrategyStrings renders the canonical models.MergeStrategy cycle order as
// wire strings for the pb.Repo.merge_strategy field the TUI reads/writes.
func mergeStrategyStrings() []string {
	ms := models.MergeStrategies()
	out := make([]string, len(ms))
	for i, s := range ms {
		out[i] = string(s)
	}
	return out
}

// mergeStrategyLabel returns a human-readable label for a merge strategy.
func mergeStrategyLabel(s string) string {
	switch s {
	case "rebase":
		return "Rebase"
	case "squash":
		return "Squash"
	default:
		return "Merge commit"
	}
}

// maskAPIKey masks an API key, showing only the last 4 characters.
func maskAPIKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 4 {
		return key
	}
	return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
}

// valueOrNotSet renders a plain-text setting value, or "(not set)" when empty.
// Used for non-secret fields (the Sentry organization slug) shown unmasked.
func valueOrNotSet(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

// RepoSettingsModel is the TUI view for editing per-repo settings.
type RepoSettingsModel struct {
	client client.BossClient
	ctx    context.Context
	repoID string
	repo   *pb.Repo
	cursor int // index into visibleRows()
	cancel bool
	done   bool
	err    error

	// Integration expansion state. Initialized from credential presence when the
	// repo loads, then driven solely by the user toggling the checkbox header.
	linearExpanded bool
	sentryExpanded bool

	githubAppClient      GitHubAppClient
	githubAppStatus      *pb.GitHubAppRepoStatus
	githubAppStatusErr   error
	githubAppMode        repoSettingsGitHubMode
	githubAppAttempt     int
	githubAppInstallURL  string
	githubAppInstallErr  error
	githubAppInstallOpen bool

	// Inline editing (repoSettingsRowNone = not editing, otherwise the row being edited)
	editingField      rowID
	nameInput         textinput.Model
	setupInput        textinput.Model
	linearApiKeyInput textinput.Model
	sentryApiKeyInput textinput.Model
	sentryOrgInput    textinput.Model

	width int
}

// NewRepoSettingsModel creates a RepoSettingsModel for the given repo ID.
func NewRepoSettingsModel(c client.BossClient, ctx context.Context, repoID string) RepoSettingsModel {
	ni := textinput.New()
	ni.Placeholder = "Repository name"
	ni.SetWidth(60)

	si := textinput.New()
	si.Placeholder = "Optional, e.g. make setup"
	si.SetWidth(60)

	aki := textinput.New()
	aki.Placeholder = "lin_api_..."
	aki.SetWidth(60)

	ski := textinput.New()
	ski.Placeholder = "sntrys_..."
	ski.SetWidth(60)

	soi := textinput.New()
	soi.Placeholder = "your-org-slug"
	soi.SetWidth(60)

	return RepoSettingsModel{
		client:            c,
		ctx:               ctx,
		repoID:            repoID,
		editingField:      repoSettingsRowNone,
		nameInput:         ni,
		setupInput:        si,
		linearApiKeyInput: aki,
		sentryApiKeyInput: ski,
		sentryOrgInput:    soi,
	}
}

func (m *RepoSettingsModel) SetGitHubAppInstall(c GitHubAppClient) {
	m.githubAppClient = c
}

// visibleRows returns the ordered list of navigable rows given the current
// expansion state. Headers and non-integration rows are always present;
// integration child rows are appended only when their parent is expanded. The
// non-navigable "Automations" and "Integrations" heading labels are not rows.
func (m RepoSettingsModel) visibleRows() []rowID {
	rows := []rowID{
		repoSettingsRowName,
		repoSettingsRowSetupScript,
		repoSettingsRowMergeStrategy,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowShouldArchiveSessionsAfterMerge,
		repoSettingsRowCanAutoDeleteBranches,
		repoSettingsRowLinearHeader,
	}
	if m.linearExpanded {
		rows = append(rows, repoSettingsRowLinearApiKey)
	}
	rows = append(rows, repoSettingsRowSentryHeader)
	if m.sentryExpanded {
		rows = append(rows,
			repoSettingsRowSentryApiKey,
			repoSettingsRowSentryOrg,
		)
	}
	return rows
}

// currentRow returns the rowID the cursor is currently on, or repoSettingsRowNone
// if the cursor is out of range.
func (m RepoSettingsModel) currentRow() rowID {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return repoSettingsRowNone
	}
	return rows[m.cursor]
}

// clampCursor keeps the cursor within the bounds of the current visible row set,
// e.g. after a collapse shrinks the list.
func (m *RepoSettingsModel) clampCursor() {
	n := len(m.visibleRows())
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// initExpansionState derives initial integration expansion from credential
// presence: an integration starts expanded if any of its credential fields is
// non-empty. Called once when the repo loads.
func (m *RepoSettingsModel) initExpansionState() {
	if m.repo == nil {
		return
	}
	m.linearExpanded = m.repo.GetLinearApiKey() != ""
	m.sentryExpanded = m.repo.GetSentryApiKey() != "" ||
		m.repo.GetSentryOrg() != ""
}

// sentryMissingFields reports which Sentry fields render red. A field is red only
// when Sentry is partially configured — exactly one of {API key, organization
// slug} is set. When both are set or both are empty, nothing is red.
func (m RepoSettingsModel) sentryMissingFields() (apiKey, org bool) {
	if m.repo == nil {
		return false, false
	}
	hasKey := m.repo.GetSentryApiKey() != ""
	hasOrg := m.repo.GetSentryOrg() != ""

	anySet := hasKey || hasOrg
	allSet := hasKey && hasOrg
	if !anySet || allSet {
		return false, false
	}
	return !hasKey, !hasOrg
}

func (m RepoSettingsModel) Init() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		if err != nil {
			return repoSettingsLoadedMsg{err: err}
		}
		for _, r := range repos {
			if r.Id == m.repoID {
				return repoSettingsLoadedMsg{repo: r}
			}
		}
		return repoSettingsLoadedMsg{err: fmt.Errorf("repo not found")}
	}
}

func (m RepoSettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// When editing a text field, forward all message types (not just KeyMsg)
	// to the textinput so that paste messages are handled correctly.
	if m.editingField != repoSettingsRowNone {
		return m.updateEditing(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case repoSettingsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repo = msg.repo
		m.nameInput.SetValue(m.repo.DisplayName)
		m.setupInput.SetValue(m.repo.GetSetupScript())
		m.initExpansionState()
		// Note: API key is NOT pre-filled (always full replace for security)
		return m, m.loadGitHubAppStatus()

	case repoSettingsSavedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repo = msg.repo
		m.err = nil
		return m, nil

	case repoSettingsGitHubStatusMsg:
		if msg.err != nil {
			m.githubAppStatusErr = msg.err
			return m, nil
		}
		m.githubAppStatusErr = nil
		m.githubAppStatus = githubAppStatusForRepo(msg.repos, m.githubRepoNWO())
		return m, nil

	case repoSettingsGitHubInstallURLMsg:
		if msg.attempt != m.githubAppAttempt || m.githubAppMode != repoSettingsGitHubModeOpening {
			return m, nil
		}
		if msg.err != nil {
			m.githubAppInstallErr = msg.err
			return m, nil
		}
		m.githubAppInstallURL = msg.url
		m.githubAppInstallOpen = true
		return m, m.openGitHubAppURL(msg.url, msg.attempt)

	case repoSettingsGitHubOpenedMsg:
		if msg.attempt != m.githubAppAttempt || m.githubAppMode != repoSettingsGitHubModeOpening {
			return m, nil
		}
		m.githubAppInstallURL = msg.url
		m.githubAppInstallErr = msg.err
		if msg.err == nil {
			m.githubAppMode = repoSettingsGitHubModeNone
		}
		return m, nil

	case tea.KeyMsg:
		if m.githubAppMode != repoSettingsGitHubModeNone {
			return m.updateGitHubMode(msg)
		}
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "g":
			return m.activateGitHubAction()
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.visibleRows())-1 {
				m.cursor++
			}
		case "enter", "space":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m RepoSettingsModel) updateGitHubMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.githubAppMode = repoSettingsGitHubModeNone
		m.githubAppInstallErr = nil
		return m, nil
	case "enter":
		if m.githubAppMode == repoSettingsGitHubModePrompt {
			if m.githubAppInstalled() {
				// Confirmed: open the installation settings page in the browser.
				m.githubAppMode = repoSettingsGitHubModeOpening
				m.githubAppAttempt++
				m.githubAppInstallErr = nil
				m.githubAppInstallOpen = true
				return m, m.openGitHubAppURL(m.githubAppInstallURL, m.githubAppAttempt)
			}
			return m.startGitHubConnect()
		}
		if m.githubAppMode == repoSettingsGitHubModeOpening && m.githubAppInstallURL != "" {
			m.githubAppInstallErr = nil
			m.githubAppInstallOpen = true
			return m, m.openGitHubAppURL(m.githubAppInstallURL, m.githubAppAttempt)
		}
	}
	return m, nil
}

func (m RepoSettingsModel) updateEditing(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			return m.commitEdit()
		case "esc":
			return m.cancelEdit(), nil
		}
	}

	var cmd tea.Cmd
	switch m.editingField {
	case repoSettingsRowName:
		m.nameInput, cmd = m.nameInput.Update(msg)
	case repoSettingsRowSetupScript:
		m.setupInput, cmd = m.setupInput.Update(msg)
	case repoSettingsRowLinearApiKey:
		m.linearApiKeyInput, cmd = m.linearApiKeyInput.Update(msg)
	case repoSettingsRowSentryApiKey:
		m.sentryApiKeyInput, cmd = m.sentryApiKeyInput.Update(msg)
	case repoSettingsRowSentryOrg:
		m.sentryOrgInput, cmd = m.sentryOrgInput.Update(msg)
	default:
		// Other rows are not editable text fields.
	}
	return m, cmd
}

func (m RepoSettingsModel) commitEdit() (tea.Model, tea.Cmd) {
	switch m.editingField {
	case repoSettingsRowName:
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			m.err = fmt.Errorf("name cannot be empty")
			return m, nil
		}
		m.editingField = repoSettingsRowNone
		m.err = nil
		m.nameInput.Blur()
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:          m.repoID,
			DisplayName: &name,
		})
	case repoSettingsRowSetupScript:
		val := strings.TrimSpace(m.setupInput.Value())
		m.editingField = repoSettingsRowNone
		m.err = nil
		m.setupInput.Blur()
		// Empty string clears the setup command.
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:          m.repoID,
			SetupScript: &val,
		})
	case repoSettingsRowLinearApiKey:
		val := strings.TrimSpace(m.linearApiKeyInput.Value())
		m.editingField = repoSettingsRowNone
		m.err = nil
		m.linearApiKeyInput.Blur()
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:           m.repoID,
			LinearApiKey: &val,
		})
	case repoSettingsRowSentryApiKey:
		val := strings.TrimSpace(m.sentryApiKeyInput.Value())
		m.editingField = repoSettingsRowNone
		m.err = nil
		m.sentryApiKeyInput.Blur()
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:           m.repoID,
			SentryApiKey: &val,
		})
	case repoSettingsRowSentryOrg:
		val := strings.TrimSpace(m.sentryOrgInput.Value())
		m.editingField = repoSettingsRowNone
		m.err = nil
		m.sentryOrgInput.Blur()
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:        m.repoID,
			SentryOrg: &val,
		})
	default:
		// Other rows have no committable edit state.
	}
	return m, nil
}

func (m RepoSettingsModel) cancelEdit() RepoSettingsModel {
	switch m.editingField {
	case repoSettingsRowName:
		m.nameInput.Blur()
		if m.repo != nil {
			m.nameInput.SetValue(m.repo.DisplayName)
		}
	case repoSettingsRowSetupScript:
		m.setupInput.Blur()
		if m.repo != nil {
			m.setupInput.SetValue(m.repo.GetSetupScript())
		}
	case repoSettingsRowLinearApiKey:
		m.linearApiKeyInput.Blur()
		m.linearApiKeyInput.SetValue("") // Always empty (full replace)
	case repoSettingsRowSentryApiKey:
		m.sentryApiKeyInput.Blur()
		m.sentryApiKeyInput.SetValue("") // Always empty (full replace)
	case repoSettingsRowSentryOrg:
		m.sentryOrgInput.Blur()
		if m.repo != nil {
			m.sentryOrgInput.SetValue(m.repo.GetSentryOrg())
		}
	default:
		// Other rows have no editable input to reset.
	}
	m.editingField = repoSettingsRowNone
	m.err = nil
	return m
}

func (m RepoSettingsModel) activateRow() (tea.Model, tea.Cmd) {
	if m.repo == nil {
		return m, nil
	}

	switch m.currentRow() {
	case repoSettingsRowName:
		m.editingField = repoSettingsRowName
		return m, m.nameInput.Focus()
	case repoSettingsRowSetupScript:
		m.editingField = repoSettingsRowSetupScript
		return m, m.setupInput.Focus()
	case repoSettingsRowMergeStrategy:
		// Cycle through merge strategies. Normalize the stored value first so an
		// unknown/empty value starts the cycle from the default (merge).
		current := string(models.ParseMergeStrategy(m.repo.MergeStrategy))
		next := mergeStrategies[0]
		for i, s := range mergeStrategies {
			if s == current {
				next = mergeStrategies[(i+1)%len(mergeStrategies)]
				break
			}
		}
		m.repo.MergeStrategy = next
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:            m.repoID,
			MergeStrategy: &next,
		})
	case repoSettingsRowCanAutoMerge:
		v := !m.repo.CanAutoMerge
		m.repo.CanAutoMerge = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:           m.repoID,
			CanAutoMerge: &v,
		})
	case repoSettingsRowCanAutoMergeDependabot:
		v := !m.repo.CanAutoMergeDependabot
		m.repo.CanAutoMergeDependabot = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:                     m.repoID,
			CanAutoMergeDependabot: &v,
		})
	case repoSettingsRowCanAutoRepair:
		v := !m.repo.CanAutoRepair
		m.repo.CanAutoRepair = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:            m.repoID,
			CanAutoRepair: &v,
		})
	case repoSettingsRowShouldArchiveSessionsAfterMerge:
		v := !m.repo.ShouldArchiveSessionsAfterMerge
		m.repo.ShouldArchiveSessionsAfterMerge = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:                              m.repoID,
			ShouldArchiveSessionsAfterMerge: &v,
		})
	case repoSettingsRowCanAutoDeleteBranches:
		v := !m.repo.CanAutoDeleteBranches
		m.repo.CanAutoDeleteBranches = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:                    m.repoID,
			CanAutoDeleteBranches: &v,
		})
	case repoSettingsRowLinearHeader:
		// UI-only expand/collapse toggle; never reads or writes credentials.
		m.linearExpanded = !m.linearExpanded
		m.clampCursor()
		return m, nil
	case repoSettingsRowSentryHeader:
		// UI-only expand/collapse toggle; never reads or writes credentials.
		m.sentryExpanded = !m.sentryExpanded
		m.clampCursor()
		return m, nil
	case repoSettingsRowLinearApiKey:
		m.editingField = repoSettingsRowLinearApiKey
		m.linearApiKeyInput.SetValue("") // Full replace, not edit
		return m, m.linearApiKeyInput.Focus()
	case repoSettingsRowSentryApiKey:
		m.editingField = repoSettingsRowSentryApiKey
		m.sentryApiKeyInput.SetValue("") // Full replace, not edit
		return m, m.sentryApiKeyInput.Focus()
	case repoSettingsRowSentryOrg:
		m.editingField = repoSettingsRowSentryOrg
		m.sentryOrgInput.SetValue(m.repo.SentryOrg) // Plain text, edit in place
		return m, m.sentryOrgInput.Focus()
	default:
		// repoSettingsRowNone or any unmapped row: no action.
	}
	return m, nil
}

func (m RepoSettingsModel) activateGitHubAction() (tea.Model, tea.Cmd) {
	if m.githubRepoNWO() == "" {
		return m, nil
	}
	if m.githubAppInstalled() {
		rawURL := githubAppInstallationSettingsURL(m.githubAppStatus)
		if rawURL == "" {
			m.err = fmt.Errorf("github app installation id unavailable")
			return m, nil
		}
		// Confirm before opening the browser, mirroring the connect flow, so a
		// single keystroke on a focused row never spawns a browser window.
		m.githubAppMode = repoSettingsGitHubModePrompt
		m.githubAppInstallURL = rawURL
		m.githubAppInstallErr = nil
		return m, nil
	}
	if m.githubAppClient == nil {
		m.err = fmt.Errorf("github app connection unavailable")
		return m, nil
	}
	m.githubAppMode = repoSettingsGitHubModePrompt
	m.githubAppInstallErr = nil
	return m, nil
}

func (m RepoSettingsModel) loadGitHubAppStatus() tea.Cmd {
	if m.githubAppClient == nil || m.githubRepoNWO() == "" {
		return nil
	}
	return func() tea.Msg {
		repos, err := m.githubAppClient.ListGitHubAppRepos(m.ctx)
		return repoSettingsGitHubStatusMsg{repos: repos, err: err}
	}
}

func (m RepoSettingsModel) startGitHubConnect() (RepoSettingsModel, tea.Cmd) {
	m.githubAppMode = repoSettingsGitHubModeOpening
	m.githubAppAttempt++
	m.githubAppInstallURL = ""
	m.githubAppInstallErr = nil
	m.githubAppInstallOpen = false
	return m, m.requestGitHubAppInstallURL(m.githubAppAttempt)
}

func (m RepoSettingsModel) requestGitHubAppInstallURL(attempt int) tea.Cmd {
	return func() tea.Msg {
		url, err := m.githubAppClient.GetGitHubAppInstallURL(m.ctx, "")
		if err == nil {
			url = enrichGitHubAppInstallURL(m.ctx, url, m.repo.GetOriginUrl())
		}
		return repoSettingsGitHubInstallURLMsg{url: url, err: err, attempt: attempt}
	}
}

func (m RepoSettingsModel) openGitHubAppURL(rawURL string, attempt int) tea.Cmd {
	return func() tea.Msg {
		err := openGitHubAppInstallURL(rawURL)
		return repoSettingsGitHubOpenedMsg{url: rawURL, err: err, attempt: attempt}
	}
}

func (m RepoSettingsModel) githubRepoNWO() string {
	if m.repo == nil {
		return ""
	}
	return vcs.GitHubNWO(m.repo.GetOriginUrl())
}

func (m RepoSettingsModel) githubAppInstalled() bool {
	if m.githubAppStatus == nil || !m.githubAppStatus.GetInstalled() {
		return false
	}
	return strings.EqualFold(m.githubAppStatus.GetOwner()+"/"+m.githubAppStatus.GetName(), m.githubRepoNWO())
}

func (m RepoSettingsModel) githubAppActionLabel() string {
	if m.githubRepoNWO() == "" {
		return ""
	}
	if m.githubAppInstalled() {
		return "[g]ithub app"
	}
	return "[g] connect Github"
}

func githubAppStatusForRepo(repos []*pb.GitHubAppRepoStatus, nwo string) *pb.GitHubAppRepoStatus {
	for _, repo := range repos {
		if repo.GetInstalled() && strings.EqualFold(repo.GetOwner()+"/"+repo.GetName(), nwo) {
			return repo
		}
	}
	return nil
}

func githubAppInstallationSettingsURL(status *pb.GitHubAppRepoStatus) string {
	if status == nil || status.GetInstallationId() <= 0 {
		return ""
	}
	owner := strings.TrimSpace(status.GetOwner())
	if owner == "" {
		return fmt.Sprintf("https://github.com/settings/installations/%d", status.GetInstallationId())
	}
	return fmt.Sprintf("https://github.com/organizations/%s/settings/installations/%d", url.PathEscape(owner), status.GetInstallationId())
}

func (m RepoSettingsModel) saveSettings(req *pb.UpdateRepoRequest) tea.Cmd {
	return func() tea.Msg {
		repo, err := m.client.UpdateRepo(m.ctx, req)
		return repoSettingsSavedMsg{repo: repo, err: err}
	}
}

// Cancelled returns true if the user exited the settings view.
func (m RepoSettingsModel) Cancelled() bool { return m.cancel }

// Done returns true if settings were saved and the view should close.
func (m RepoSettingsModel) Done() bool { return m.done }

// textEntryActive reports whether a row is in inline edit mode with its
// textinput focused, so App can leave ctrl+x alone rather than aliasing it onto
// Esc (BOS-660). Off the edit path Esc backs out one level — dismissing the
// GitHub App overlay when one is up, otherwise exiting the view.
func (m RepoSettingsModel) textEntryActive() bool {
	return m.editingField != repoSettingsRowNone
}

func (m RepoSettingsModel) View() tea.View {
	if m.githubAppMode == repoSettingsGitHubModePrompt {
		if m.githubAppInstalled() {
			return tea.NewView(m.githubAppSettingsPromptContent())
		}
		return tea.NewView(githubAppInstallPromptContent(m.githubAppRepoLabel()))
	}
	if m.githubAppMode == repoSettingsGitHubModeOpening {
		return tea.NewView(m.githubAppOpeningView())
	}
	if m.repo == nil {
		if m.err != nil {
			return tea.NewView(
				renderError(rpcErrorMessage(m.err), m.width) + "\n" +
					styleActionBar.Render("[esc] back"),
			)
		}
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading repository..."))
	}

	var b strings.Builder

	if m.err != nil {
		b.WriteString(renderError(rpcErrorMessage(m.err), m.width))
		b.WriteString("\n")
	}

	// cur is the rowID under the cursor; rows compare against it for focus.
	cur := m.currentRow()

	// Row 0: Name
	if m.editingField == repoSettingsRowName {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Name:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.nameInput.View()))
		b.WriteString("\n")
	} else {
		b.WriteString(renderFieldRow(
			cur == repoSettingsRowName,
			fmt.Sprintf("Name: %s", m.repo.DisplayName),
		))
		b.WriteString("\n")
	}

	// Row 1: Setup command
	if m.editingField == repoSettingsRowSetupScript {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Setup command:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.setupInput.View()))
		b.WriteString("\n")
	} else {
		val := m.repo.GetSetupScript()
		if val == "" {
			val = "(none)"
		}
		b.WriteString(renderFieldRow(
			cur == repoSettingsRowSetupScript,
			fmt.Sprintf("Setup command: %s", val),
		))
		b.WriteString("\n")
	}

	// Row 2: Merge strategy
	b.WriteString(renderFieldRow(
		cur == repoSettingsRowMergeStrategy,
		fmt.Sprintf("Merge strategy: %s", mergeStrategyLabel(m.repo.MergeStrategy)),
	))
	b.WriteString("\n")

	b.WriteString("\n")

	type checkboxRow struct {
		label   string
		checked bool
		row     rowID
	}
	renderCheckbox := func(cb checkboxRow) {
		check := " "
		if cb.checked {
			check = "x"
		}
		focused := cur == cb.row && m.editingField == repoSettingsRowNone
		b.WriteString(renderFieldRow(focused, fmt.Sprintf("[%s] %s", check, cb.label)))
		b.WriteString("\n")
	}

	// Automations section. Non-navigable heading label. Groups the per-repo
	// automation toggles: "Mark ready for review when checks pass" (CanAutoMerge,
	// daemon orchestrator behavior), Dependabot auto-merge, and automatic repair.
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Automations"))
	b.WriteString("\n")

	// Automation toggles, rendered contiguously under the heading. "Mark ready
	// for review when checks pass" is the CanAutoMerge flag, which only promotes a
	// passing draft PR to ready — it does not merge. "Automatic repair" gates the
	// repair plugin (CanAutoRepair).
	for _, cb := range []checkboxRow{
		{"Mark ready for review when checks pass", m.repo.CanAutoMerge, repoSettingsRowCanAutoMerge},
		{"Auto-merge Dependabot PRs", m.repo.CanAutoMergeDependabot, repoSettingsRowCanAutoMergeDependabot},
		{"Automatic repair (failing checks, conflicts, review feedback)", m.repo.CanAutoRepair, repoSettingsRowCanAutoRepair},
		{"Archive sessions after merging PRs", m.repo.ShouldArchiveSessionsAfterMerge, repoSettingsRowShouldArchiveSessionsAfterMerge},
		{"Delete branches after archiving", m.repo.CanAutoDeleteBranches, repoSettingsRowCanAutoDeleteBranches},
	} {
		renderCheckbox(cb)
	}

	b.WriteString("\n")

	// Integrations section. Non-navigable heading label. Groups the Linear and
	// Sentry integrations, which are active when their credentials are set.
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Integrations"))
	b.WriteString("\n")

	// Linear checkbox header (expand/collapse) + child field when expanded.
	b.WriteString(m.renderIntegrationHeader("Linear", m.linearExpanded, repoSettingsRowLinearHeader, cur))
	if m.linearExpanded {
		// Linear API key (masked, full-replace). Single field, never red.
		if m.editingField == repoSettingsRowLinearApiKey {
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render("API key:"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 6).Render(m.linearApiKeyInput.View()))
			b.WriteString("\n")
		} else {
			b.WriteString(m.renderChildField("API key", maskAPIKey(m.repo.LinearApiKey), repoSettingsRowLinearApiKey, cur, false))
		}
	}

	// Sentry checkbox header (expand/collapse) + child fields when expanded.
	b.WriteString(m.renderIntegrationHeader("Sentry", m.sentryExpanded, repoSettingsRowSentryHeader, cur))
	if m.sentryExpanded {
		missingKey, missingOrg := m.sentryMissingFields()

		// Sentry API key (masked, full-replace like the Linear key).
		if m.editingField == repoSettingsRowSentryApiKey {
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render("API key:"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 6).Render(m.sentryApiKeyInput.View()))
			b.WriteString("\n")
		} else {
			b.WriteString(m.renderChildField("API key", maskAPIKey(m.repo.SentryApiKey), repoSettingsRowSentryApiKey, cur, missingKey))
		}

		// Organization slug (plain text). Issues are listed org-wide across every
		// project, so no project slug is needed.
		if m.editingField == repoSettingsRowSentryOrg {
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render("Organization slug:"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 6).Render(m.sentryOrgInput.View()))
			b.WriteString("\n")
		} else {
			b.WriteString(m.renderChildField("Organization slug", valueOrNotSet(m.repo.SentryOrg), repoSettingsRowSentryOrg, cur, missingOrg))
		}
	}

	if m.editingField != repoSettingsRowNone {
		b.WriteString(actionBarWidth(m.width, []string{"[enter] save", "[esc] cancel"}))
	} else {
		actions := []string{"[enter/space] toggle/edit"}
		if label := m.githubAppActionLabel(); label != "" {
			actions = append(actions, label)
		}
		b.WriteString(actionBarWidth(m.width, actions, []string{"[esc] back"}))
	}

	return tea.NewView(b.String())
}

// renderIntegrationHeader renders an integration checkbox header row
// (`[x] Label` / `[ ] Label`) matching the automation checkbox style. The
// checkbox reflects expansion state, not a persisted enabled flag.
func (m RepoSettingsModel) renderIntegrationHeader(label string, expanded bool, row, cur rowID) string {
	check := " "
	if expanded {
		check = "x"
	}
	focused := cur == row && m.editingField == repoSettingsRowNone
	return renderFieldRow(focused, fmt.Sprintf("[%s] %s", check, label)) + "\n"
}

// repoSettingsChildIndent is how many columns an integration's child field rows
// sit right of their header row, marking them as nested under it. It is the
// indent the chevron idiom baked into its own cursor strings before BOS-567.
const repoSettingsChildIndent = 2

// renderChildField renders an indented integration child field row. When missing
// is true (partial-config validation), the value is shown in red.
func (m RepoSettingsModel) renderChildField(label, value string, row, cur rowID, missing bool) string {
	focused := cur == row && m.editingField == repoSettingsRowNone
	if missing {
		value = styleStatusDanger.Render(value)
	}
	return renderIndentedFieldRow(focused, repoSettingsChildIndent, fmt.Sprintf("%s: %s", label, value)) + "\n"
}

func (m RepoSettingsModel) githubAppRepoLabel() string {
	label := m.githubRepoNWO()
	if label == "" && m.repo != nil {
		label = m.repo.DisplayName
	}
	if label == "" {
		label = "this repository"
	}
	return label
}

func (m RepoSettingsModel) githubAppSettingsPromptContent() string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	body := "Open the Bossanova GitHub App settings for " + m.githubAppRepoLabel() + " in your browser?"
	return padding.Render(body) + "\n" +
		styleActionBar.Render("[enter] open  [esc] back")
}

func (m RepoSettingsModel) githubAppOpeningView() string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	body := "Opening GitHub App installation page..."
	if m.githubAppInstallOpen {
		body = "GitHub App installation page opened for " + m.githubAppRepoLabel() + "."
	}
	if m.githubAppInstallErr != nil && m.githubAppInstallURL != "" {
		body += "\nOpen this GitHub App URL: " + m.githubAppInstallURL
	}
	if m.githubAppInstallErr != nil && m.githubAppInstallURL == "" {
		body = "Could not start GitHub App installation: " + m.githubAppInstallErr.Error()
	}
	return padding.Render(body) + "\n" +
		styleActionBar.Render("[enter] re-open  [esc] back")
}
