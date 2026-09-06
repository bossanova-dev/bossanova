package views

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
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

// repoSettingsOrganizationsMsg carries the whole organization picture in one
// message: the caller's organizations and this repo's current mapping. They
// travel together because they are fetched together, chained off the existing
// settings load rather than opening a second fetch lifecycle in this view.
type repoSettingsOrganizationsMsg struct {
	orgs    []*pb.Organization
	mapping *pb.RepoOrganizationMapping
	// err is a ListOrganizations failure; mappingErr is a GetRepoOrganization
	// failure. They stay separate because the view says different things about
	// them: a missing list costs the user the picker, a missing mapping costs
	// them the current value, and reporting either as the other names a failure
	// that did not happen.
	err        error
	mappingErr error
}

// repoSettingsOrganizationSavedMsg carries the result of a set or a clear. A
// successful clear reports a nil mapping, which is the same shape an unmapped
// repo loads with, so both paths converge on one representation.
type repoSettingsOrganizationSavedMsg struct {
	mapping *pb.RepoOrganizationMapping
	// organizationID is the organization the write named -- the one being set,
	// or the one being released by a clear. A refusal is only classifiable
	// against it: whether the caller is a member of that organization is what
	// separates the sentences bosso's single PermissionDenied could mean.
	organizationID string
	err            error
}

// rowID is a stable identity for each logical row in the settings view. Rows are
// referenced by identity rather than position because collapsing an integration
// hides its child rows, so on-screen positions are dynamic.
type rowID int

const (
	repoSettingsRowNone rowID = iota - 1 // sentinel: not editing / no row

	repoSettingsRowName
	repoSettingsRowSetupScript
	repoSettingsRowOrganization
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

// RepoOrganizationClient is the narrow slice of the authenticated cloud client
// the repo settings view needs in order to read and write a repo's organization
// mapping. It is deliberately NOT part of client.BossClient: the mapping is
// bosso-owned, a local daemon holds none of it, and putting these four methods
// on the shared interface would force every BossClient fake in the tree to grow
// stubs it can never serve. The view receives it the same way it receives
// GitHubAppClient — a type assertion on App.cloudAccess — so a signed-out user,
// whose cloudAccess is nil, simply never gets the field.
type RepoOrganizationClient interface {
	ListOrganizations(ctx context.Context) ([]*pb.Organization, error)
	GetRepoOrganization(ctx context.Context, repoOriginURL string) (*pb.RepoOrganizationMapping, error)
	SetRepoOrganization(ctx context.Context, repoOriginURL, organizationID string) (*pb.RepoOrganizationMapping, error)
	ClearRepoOrganization(ctx context.Context, repoOriginURL, organizationID string) error
}

// repoSettingsNoOrgLabel names the unmapped state. It is a real, default choice
// rather than an empty one, and the label says so plainly.
//
// It reads "None" and not "Unmapped (Personal)". That earlier label tried to
// carry the consequence as well as the state -- an unmapped repo's sessions
// follow the serving daemon's own organization, which for a local daemon is the
// owner's personal one -- and ended up naming two things at once, neither
// obviously. "None" answers the question the field actually asks, which is
// which organization this repository is in.
const repoSettingsNoOrgLabel = "None"

// repoSettingsNotAMemberPrefix opens the lines that assert non-membership. It is
// used only where non-membership is actually established: a stored mapping
// naming an organization absent from the caller's own list, and the defensive
// set-time case of a write naming one. A refusal of a write the picker offered
// is not one of those -- see organizationSetRefusalMessage.
const repoSettingsNotAMemberPrefix = "You are not a member of"

// orgChoice is one row of the organization picker. An empty id is the Personal
// (unmapped) choice, which is always present and always first.
type orgChoice struct {
	id    string
	label string
}

// organizationLabel names an organization for display, falling back to its id
// so a nameless row is still legible and selectable rather than a blank line.
func organizationLabel(o *pb.Organization) string {
	if name := strings.TrimSpace(o.GetName()); name != "" {
		return name
	}
	return o.GetId()
}

// organizationSetRefusalMessage turns a refused set or clear into a line the
// user can act on.
//
// PermissionDenied at this endpoint is not evidence of non-membership. bosso
// raises it from two unrelated places: the cloud-access gate raises it for an
// entitlement lapse, and only the other case is a genuine membership refusal.
// The picker offers exactly what ListOrganizations returned — organizations the
// caller is already a member of — so for anything it can offer, "you are not a
// member" is the one reading that is certainly wrong, and it sends the user to
// the only thing they cannot fix. The caller's own list is evidence the server
// does not have, so it decides which sentence is true.
//
// There used to be a third source, and it was the common one: the membership
// gate compared the named id against the token's *active* organization rather
// than the membership table, so mapping a repo into any organization you were
// not currently signed in to was refused as "membership required" — which was
// false, and which this message had to hedge around. That gate now authorizes by
// membership, so an entitlement lapse is the only thing left for a member to
// check.
//
// FailedPrecondition is deliberately not classified here. bosso raises it as
// "re-authenticate with an organization-scoped token", a better instruction than
// anything this could substitute, so it travels to the ordinary error banner
// intact rather than being overwritten.
func (m RepoSettingsModel) organizationSetRefusalMessage(err error, organizationID string) string {
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		return ""
	}
	if organizationID == "" {
		// No id means no evidence either way, and the non-membership branch
		// below would otherwise claim one. Unreachable today — every saved
		// message carries the id it acted on — but the claim is only ever as
		// safe as the guard that makes it, so the guard is structural.
		return ""
	}
	label, isMember := m.callerOrganization(organizationID)
	if !isMember {
		return repoSettingsNotAMemberPrefix + " that organization, so the mapping was refused."
	}
	return "Bossanova refused the mapping to " + label +
		". You are a member of it, so check that your Bossanova Cloud subscription is active."
}

// callerOrganization reports an organization's display label and whether it is
// in the caller's own list at all. Membership of the list is the view's only
// first-hand evidence about membership of the organization.
func (m RepoSettingsModel) callerOrganization(organizationID string) (string, bool) {
	if organizationID == "" {
		return "", false
	}
	for _, o := range m.organizations {
		if o.GetId() == organizationID {
			return organizationLabel(o), true
		}
	}
	return "", false
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

	// Organization mapping. orgClient is nil for a local-only / signed-out user,
	// which is the whole of the "no picker" behaviour: visibleRows() then never
	// emits repoSettingsRowOrganization and View() never renders it.
	orgClient     RepoOrganizationClient
	organizations []*pb.Organization
	orgMapping    *pb.RepoOrganizationMapping
	orgLoadErr    error
	orgMappingErr error // GetRepoOrganization failure; the current value is unknown, not Personal
	// orgRefusal is the notice rendered above the field's load-error line, and
	// again inside the picker overlay, where it stands alone: a standing
	// membership refusal from the load, a set-time refusal from a failed write,
	// or a pick this view could not carry out at all. "" when none. It is not
	// only a membership refusal -- check every sender before adding a rule
	// about when it clears.
	orgRefusal      string
	orgPickerOpen   bool
	orgPickerCursor int
	orgSaving       bool

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
	aki.Placeholder = "lin_api_…"
	aki.SetWidth(60)

	ski := textinput.New()
	ski.Placeholder = "sntrys_…"
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

// SetRepoOrganizationClient injects the cloud client that backs the organization
// field. Leaving it unset is the supported local-only shape, not a degraded one.
func (m *RepoSettingsModel) SetRepoOrganizationClient(c RepoOrganizationClient) {
	m.orgClient = c
}

// organizationRowVisible reports whether the organization row exists at all. It
// needs two things: a cloud client to serve the mapping, and an origin URL to
// key it by.
//
// Those two structural preconditions are the whole rule; the size of the
// caller's organization list is deliberately not part of it. A signed-out or
// local-only user has no cloud client, so there is nothing to serve a mapping
// and no picker is offered rather than a row that could only ever report "not
// logged in". A repo with no git origin is the same shape arrived at from the
// other side — it cannot be mapped, so a confirm on it would send an empty
// origin for bosso to refuse.
//
// A signed-in user who belongs to no organization still gets the row, and its
// picker still holds the Personal choice alone. That is intended rather than an
// oversight of the rule above: Personal is a real destination, so a mapping made
// earlier (or made from another client) has to stay resettable by someone whose
// list is empty today.
//
// visibleRows(), View(), and activateRow() share this one predicate, so the
// cursor can never land on a row that does not render and no row can be
// activated unseen.
func (m RepoSettingsModel) organizationRowVisible() bool {
	return m.orgClient != nil && m.repo.GetOriginUrl() != ""
}

// visibleRows returns the ordered list of navigable rows given the current
// expansion state. Headers and non-integration rows are always present;
// integration child rows are appended only when their parent is expanded. The
// non-navigable "Automations" and "Integrations" heading labels are not rows.
func (m RepoSettingsModel) visibleRows() []rowID {
	rows := []rowID{
		repoSettingsRowName,
		repoSettingsRowSetupScript,
	}
	if m.organizationRowVisible() {
		rows = append(rows, repoSettingsRowOrganization)
	}
	rows = append(rows,
		repoSettingsRowMergeStrategy,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowShouldArchiveSessionsAfterMerge,
		repoSettingsRowCanAutoDeleteBranches,
		repoSettingsRowLinearHeader,
	)
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
		//
		// Both follow-on loads hang off this one message rather than off Init, so
		// the view keeps a single settings-load lifecycle: the repo has to be
		// known before either can be asked for (the GitHub status needs its NWO,
		// the organization mapping needs its origin URL).
		return m, tea.Batch(m.loadGitHubAppStatus(), m.loadOrganizations())

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

	case repoSettingsOrganizationsMsg:
		m.organizations = msg.orgs
		m.orgLoadErr = msg.err
		m.orgMappingErr = msg.mappingErr
		if msg.err == nil && msg.mappingErr == nil {
			m.orgMapping = msg.mapping
		}
		m.orgRefusal = m.orgMembershipRefusal()
		return m, nil

	case repoSettingsOrganizationSavedMsg:
		m.orgSaving = false
		if msg.err != nil {
			// A membership refusal is not a generic RPC failure: it is the one
			// outcome the user can do something about, so it gets its own line
			// next to the field instead of the shared error banner.
			if refusal := m.organizationSetRefusalMessage(msg.err, msg.organizationID); refusal != "" {
				m.orgRefusal = refusal
				return m, nil
			}
			m.err = msg.err
			return m, nil
		}
		m.orgMapping = msg.mapping
		m.orgMappingErr = nil
		m.err = nil
		m.orgRefusal = m.orgMembershipRefusal()
		return m, nil

	case tea.KeyMsg:
		if m.githubAppMode != repoSettingsGitHubModeNone {
			return m.updateGitHubMode(msg)
		}
		if m.orgPickerOpen {
			return m.updateOrgPicker(msg)
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
	case repoSettingsRowOrganization:
		if !m.organizationRowVisible() {
			return m, nil
		}
		if m.orgSaving {
			// A write is already in flight. Reopening the picker now would seed
			// its cursor from a mapping the server has been told to change, and
			// a second confirm would race the first: the replies land in arrival
			// order, so the field could settle on the earlier choice.
			return m, nil
		}
		// A picker rather than the merge-strategy cycle idiom: cycling would fire
		// a server write on every keypress, and each intermediate value is a real
		// remapping of the repo, not a local toggle.
		m.orgPickerOpen = true
		m.orgPickerCursor = m.orgPickerIndexForCurrent()
		return m, nil
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

// updateOrgPicker drives the organization picker overlay. Navigation is local;
// only enter reaches the network, and it does so through a tea.Cmd.
func (m RepoSettingsModel) updateOrgPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	choices := m.orgChoices()
	switch msg.String() {
	case "esc":
		m.orgPickerOpen = false
		return m, nil
	case "up", "k":
		if m.orgPickerCursor > 0 {
			m.orgPickerCursor--
		}
		return m, nil
	case "down", "j":
		if m.orgPickerCursor < len(choices)-1 {
			m.orgPickerCursor++
		}
		return m, nil
	case "enter", "space":
		if m.orgPickerCursor < 0 || m.orgPickerCursor >= len(choices) {
			m.orgPickerOpen = false
			return m, nil
		}
		m.orgPickerOpen = false
		return m.applyOrganizationChoice(choices[m.orgPickerCursor])
	}
	return m, nil
}

// applyOrganizationChoice writes the picked choice. Selecting the Personal row
// clears the mapping; selecting an organization sets it. Both are tea.Cmds — no
// RPC is issued on the Update goroutine.
func (m RepoSettingsModel) applyOrganizationChoice(choice orgChoice) (tea.Model, tea.Cmd) {
	current := m.orgMapping.GetOrganizationId()
	switch {
	// This case must precede the dedup below: organizationMappingUnknown()
	// implies current == "", so a reset would otherwise match `choice.id ==
	// current` and be discarded as a no-op — the bug
	// TestRepoSettingsOrganization_UnknownMappingDoesNotSwallowAReset guards. A
	// non-empty choice deliberately falls out of this case and proceeds to the
	// save.
	case m.organizationMappingUnknown():
		// Nothing is known to be in force, so no pick is a no-op. A reset is
		// the one pick that still cannot be carried out: ClearRepoOrganization
		// is organization-scoped and takes the id being cleared, which is
		// exactly what the unknown state does not supply. Say so here rather
		// than send a request the server refuses for a field the user never saw.
		//
		// The message names the unknown state and not the RPC behind it: the
		// predicate is an OR over two causes, and a ListOrganizations failure
		// means GetRepoOrganization was never issued at all. Which read failed
		// is a fact the notice below has and this one does not.
		//
		// It says "could not determine" and not "does not know" for the same
		// reason: a failed read is a fact about this attempt, not about the
		// server, which may still hold a mapping this view never received.
		if choice.id == "" {
			m.orgRefusal = "Bossanova could not determine this repo's current organization, so there is nothing this view can reset. Reopen this view to retry."
			return m, nil
		}
	case choice.id == current:
		return m, nil
	}
	m.orgSaving = true
	m.orgRefusal = ""
	m.err = nil

	// Capture what the closure needs off the receiver: m is a value, and the
	// command runs after this Update has returned a different copy of it.
	orgClient := m.orgClient
	ctx := m.ctx
	originURL := m.repo.GetOriginUrl()

	if choice.id == "" {
		// Reset to Personal. ClearRepoOrganization is organization-scoped as an
		// authorization backstop, so it takes the id currently mapped — not the
		// (empty) id being selected.
		clearID := current
		return m, func() tea.Msg {
			if err := orgClient.ClearRepoOrganization(ctx, originURL, clearID); err != nil {
				return repoSettingsOrganizationSavedMsg{organizationID: clearID, err: err}
			}
			return repoSettingsOrganizationSavedMsg{}
		}
	}
	organizationID := choice.id
	return m, func() tea.Msg {
		mapping, err := orgClient.SetRepoOrganization(ctx, originURL, organizationID)
		return repoSettingsOrganizationSavedMsg{organizationID: organizationID, mapping: mapping, err: err}
	}
}

// loadOrganizations fetches the caller's organizations and this repo's mapping
// in one command, emitting one message. Two RPCs, one lifecycle: the view has a
// single async organization state to reason about rather than two that can
// disagree about whether they have landed.
func (m RepoSettingsModel) loadOrganizations() tea.Cmd {
	if !m.organizationRowVisible() {
		return nil
	}
	orgClient := m.orgClient
	ctx := m.ctx
	originURL := m.repo.GetOriginUrl()
	return func() tea.Msg {
		orgs, err := orgClient.ListOrganizations(ctx)
		if err != nil {
			return repoSettingsOrganizationsMsg{err: err}
		}
		mapping, err := orgClient.GetRepoOrganization(ctx, originURL)
		if err != nil {
			// Keep the organizations: the picker is still usable, only the
			// current value is unknown, and reporting that beats a blank field.
			return repoSettingsOrganizationsMsg{orgs: orgs, mappingErr: err}
		}
		return repoSettingsOrganizationsMsg{orgs: orgs, mapping: mapping}
	}
}

// orgChoices is the picker's row set: the None sentinel first, then exactly the caller's
// organizations sorted case-insensitively by their displayed labels. IDs break
// equal-label ties so the order does not depend on the sorting implementation.
func (m RepoSettingsModel) orgChoices() []orgChoice {
	choices := make([]orgChoice, 0, len(m.organizations)+1)
	choices = append(choices, orgChoice{label: repoSettingsNoOrgLabel})
	for _, o := range m.organizations {
		choices = append(choices, orgChoice{id: o.GetId(), label: organizationLabel(o)})
	}
	slices.SortFunc(choices[1:], func(a, b orgChoice) int {
		if byLabel := strings.Compare(strings.ToLower(a.label), strings.ToLower(b.label)); byLabel != 0 {
			return byLabel
		}
		return strings.Compare(a.id, b.id)
	})
	return choices
}

// orgPickerIndexForCurrent opens the picker on the mapping in force, so enter
// on an unchanged selection is a no-op rather than a surprise remapping.
func (m RepoSettingsModel) orgPickerIndexForCurrent() int {
	current := m.orgMapping.GetOrganizationId()
	for i, c := range m.orgChoices() {
		if c.id == current {
			return i
		}
	}
	return 0
}

// organizationMappingUnknown reports that this repo's mapping was not read, as
// distinct from being read and found empty. Either RPC leaves it unread: a
// ListOrganizations failure returns before GetRepoOrganization is issued at all,
// and a GetRepoOrganization failure returns without a mapping.
//
// It is one predicate rather than a check at each site because the empty id the
// failure leaves behind is indistinguishable from a genuine Personal mapping,
// and every consumer that mistakes one for the other asserts something nobody
// established: the field would read "None", the picker would mark
// Personal "(current)", and a reset to Personal would be discarded as a no-op.
func (m RepoSettingsModel) organizationMappingUnknown() bool {
	return m.orgMapping.GetOrganizationId() == "" &&
		(m.orgLoadErr != nil || m.orgMappingErr != nil)
}

// organizationValue renders the row's current value. An unmapped repo shows the
// Personal default rather than "(not set)": there is no unset state here, only
// two real ones.
func (m RepoSettingsModel) organizationValue() string {
	if m.organizationMappingUnknown() {
		return "Unknown"
	}
	id := m.orgMapping.GetOrganizationId()
	if id == "" {
		return repoSettingsNoOrgLabel
	}
	if label, ok := m.callerOrganization(id); ok {
		return label
	}
	// Mapped to something outside the caller's list. Show the id so the row is
	// never blank; orgMembershipRefusal supplies the why underneath it.
	return id
}

// orgMembershipRefusal reports a stored mapping that names an organization the
// signed-in user is not a member of. bosso's read path normally hides such a
// row — GetRepoOrganization scopes the lookup to the caller's organization and
// answers an unset mapping — so this is the defensive half of the criterion,
// covering the case where a mapping does come back naming an id absent from
// ListOrganizations. The other half is at set time, in
// organizationSetRefusalMessage. Both share repoSettingsNotAMemberPrefix.
func (m RepoSettingsModel) orgMembershipRefusal() string {
	id := m.orgMapping.GetOrganizationId()
	if id == "" {
		return ""
	}
	if _, ok := m.callerOrganization(id); ok {
		return ""
	}
	return repoSettingsNotAMemberPrefix + " the mapped organization (" + id +
		"). New sessions for this repo will not be published to it until the mapping is changed."
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
	if m.orgPickerOpen {
		return tea.NewView(m.organizationPickerContent())
	}
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
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading repository…"))
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

	// Row 2: Organization. Present only for a signed-in user with a mappable
	// repo — visibleRows() and this block are gated on the same predicate, so
	// the cursor can never land on a row that does not render.
	if m.organizationRowVisible() {
		value := m.organizationValue()
		if m.orgSaving {
			// The round trip is a remote one and the field keeps showing the old
			// mapping until it lands, so say so rather than look unresponsive.
			value += "  (saving…)"
		}
		b.WriteString(renderFieldRow(
			cur == repoSettingsRowOrganization,
			fmt.Sprintf("Organization: %s", value),
		))
		b.WriteString("\n")
		for _, notice := range m.organizationNotices() {
			b.WriteString(m.renderOrgNotice(notice))
		}
	}

	// Row 3: Merge strategy
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
		focused := cur == cb.row && m.editingField == repoSettingsRowNone
		b.WriteString(renderFieldRow(focused, renderCheckboxLabel(cb.checked, cb.label)))
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

// renderIntegrationHeader renders an integration checkbox header row through
// the shared renderCheckboxLabel, so it wears the same checkbox glyph as the
// automation toggles above it. The checkbox reflects expansion state, not a
// persisted enabled flag.
func (m RepoSettingsModel) renderIntegrationHeader(label string, expanded bool, row, cur rowID) string {
	focused := cur == row && m.editingField == repoSettingsRowNone
	return renderFieldRow(focused, renderCheckboxLabel(expanded, label)) + "\n"
}

// orgNoticeWrapConsumed is how many columns an organization notice line spends
// before its text starts: the field-row gutter column, the child indent, and the
// gutter's own border plus padding.
const orgNoticeWrapConsumed = fieldRowGutterColumn + repoSettingsChildIndent + 2

// orgNoticeMinWrap is the narrowest text column worth wrapping into. Below it
// the notice is unreadable either way, so leave it unconstrained rather than
// shredding it one word per line.
const orgNoticeMinWrap = 20

// renderOrgNotice renders one organization notice line (the membership refusal
// or an organizations load failure) under the Organization row, wrapped to the
// terminal rather than truncated at its edge.
//
// The refusal is a full sentence naming the organization and what it costs the
// user, so it outruns a 100-column terminal on its own. An unwrapped status line
// that runs off the edge is a status line the user cannot read — the same defect
// BOS-507 fixed on Home, where the fix was likewise to wrap at the content width
// instead of letting the terminal cut the sentence mid-word.
// organizationNotices returns the organization row's diagnostic lines in render
// order.
//
// The refusal and the load error are two different facts -- what a pick could
// not do, and why the state behind it is unknown -- so both are returned rather
// than the first shadowing the second. A refusal is raised by a keystroke, so
// shadowing would delete the user's only diagnostic at exactly the moment they
// went looking for one.
//
// View() and organizationPickerContent() share this one helper. The overlay is
// the screen the user opened in order to act on the field, so it must not be
// the one screen that omits why the list or the mapping is missing: a failed
// ListOrganizations leaves a picker holding nothing but the Personal row, and
// without these lines there is nothing on screen to distinguish that from
// belonging to no organizations.
func (m RepoSettingsModel) organizationNotices() []string {
	var notices []string
	if m.orgRefusal != "" {
		notices = append(notices, m.orgRefusal)
	}
	switch {
	case m.orgLoadErr != nil:
		notices = append(notices, "Could not load organizations: "+rpcErrorMessage(m.orgLoadErr))
	case m.orgMappingErr != nil:
		notices = append(notices, "Could not read this repo's organization: "+rpcErrorMessage(m.orgMappingErr))
	}
	return notices
}

func (m RepoSettingsModel) renderOrgNotice(text string) string {
	style := styleStatusWarning
	if wrap := m.width - orgNoticeWrapConsumed; wrap >= orgNoticeMinWrap {
		style = style.Width(wrap)
	}
	return renderIndentedFieldRow(false, repoSettingsChildIndent, style.Render(text)) + "\n"
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

// organizationPickerContent renders the organization picker overlay. It lists
// Personal plus exactly the caller's organizations, marking the one in force.
func (m RepoSettingsModel) organizationPickerContent() string {
	var b strings.Builder
	// The two literal spaces are this file's header idiom, not a stray indent:
	// Padding(0, 2) plus them lands the header at column 4, the column
	// renderFieldRow draws its rows at, matching "  Automations" and
	// "  Integrations" above.
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Organization"))
	b.WriteString("\n")
	current := m.orgMapping.GetOrganizationId()
	unknown := m.organizationMappingUnknown()
	for i, c := range m.orgChoices() {
		label := c.label
		if !unknown && c.id == current {
			label += "  (current)"
		}
		b.WriteString(renderFieldRow(i == m.orgPickerCursor, label))
		b.WriteString("\n")
	}
	for _, notice := range m.organizationNotices() {
		b.WriteString(m.renderOrgNotice(notice))
	}
	b.WriteString(actionBarWidth(m.width, []string{"[enter] select"}, []string{"[esc] back"}))
	return b.String()
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
	body := "Opening GitHub App installation page…"
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
