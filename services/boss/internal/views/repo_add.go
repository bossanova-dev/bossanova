package views

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

// --- Repo Add Wizard ---

// repoAddPhase tracks the current phase in the add-repo wizard.
type repoAddPhase int

const (
	repoAddPhaseSource          repoAddPhase = iota // Phase 1: table pick (open/clone)
	repoAddPhaseInput                               // Phase 2: path/URL input
	repoAddPhaseDetails                             // Phase 3: name + setup + confirm
	repoAddPhaseGitHubAppPrompt                     // Phase 4: confirm GitHub App install
	repoAddPhaseGitHubApp                           // Phase 5: GitHub App install follow-up
	repoAddPhaseDone                                // Terminal state
)

// sourceMode selects between open-project and clone flows.
const (
	sourceModeOpen  = 0
	sourceModeClone = 1
)

// Defaults applied by bossd when a repo is first created (see
// services/bossd/internal/db/repo_store.go SQLiteRepoStore.Create). The add
// wizard's details form is seeded to these so its controls reflect the real
// post-create state, and add-time options are only persisted (via a follow-up
// UpdateRepo) when the user changes something. Keep in sync with that INSERT.
const defaultRepoMergeStrategy = string(models.MergeStrategyMerge)

const (
	defaultCanAutoMerge           = false
	defaultCanAutoMergeDependabot = true
	defaultCanAutoRepair          = true
)

// Automation option values for the details-form multi-select. They map onto the
// repo's CanAuto* flags in repoConfigUpdate.
const (
	automationAutoMerge  = "auto_merge"
	automationDependabot = "dependabot"
	automationRepair     = "repair"
)

// defaultRepoAutomation lists the automation options enabled by default on a
// freshly-created repo (auto-merge off; the rest on).
func defaultRepoAutomation() []string {
	return []string{automationDependabot, automationRepair}
}

// repoRegisteredMsg carries the result of a RegisterRepo RPC call. configErr is
// set when the repo was created but the follow-up UpdateRepo persisting its
// add-time options failed — a non-fatal condition (the repo exists).
type repoRegisteredMsg struct {
	repo      *pb.Repo
	err       error
	configErr error
}

// repoClonedMsg carries the result of a CloneAndRegisterRepo RPC call. configErr
// has the same non-fatal meaning as on repoRegisteredMsg.
type repoClonedMsg struct {
	repo      *pb.Repo
	err       error
	configErr error
}

// repoValidatedMsg carries the result of a ValidateRepoPath RPC call.
type repoValidatedMsg struct {
	resp *pb.ValidateRepoPathResponse
	err  error
}

type githubAppInstallURLMsg struct {
	url     string
	err     error
	attempt int
}

type githubAppBrowserOpenedMsg struct {
	url     string
	err     error
	attempt int
}

type githubAppReposMsg struct {
	repos   []*pb.GitHubAppRepoStatus
	err     error
	attempt int
}

type githubAppPollTickMsg struct {
	attempt int
}

// GitHubAppClient is the authenticated cloud subset used by the repo-add
// follow-up to start GitHub App setup and observe completion.
type GitHubAppClient interface {
	GetGitHubAppInstallURL(ctx context.Context, returnURL string) (string, error)
	ListGitHubAppRepos(ctx context.Context) ([]*pb.GitHubAppRepoStatus, error)
}

const githubAppInstallPollInterval = 3 * time.Second

// githubAppInstallMaxPolls bounds the automatic installation poll loop so a
// user who never completes the GitHub App install doesn't leave boss polling
// the cloud forever. At the default 3s interval this is ~5 minutes, after which
// the view pauses and offers to keep checking or skip.
const githubAppInstallMaxPolls = 100

var (
	openGitHubAppInstallURL              = openURLFunc
	githubAppInstallPollIntervalOverride time.Duration
)

func SetGitHubAppInstallPollIntervalOverride(interval time.Duration) {
	githubAppInstallPollIntervalOverride = interval
}

func githubAppPollIntervalValue() time.Duration {
	if githubAppInstallPollIntervalOverride > 0 {
		return githubAppInstallPollIntervalOverride
	}
	return githubAppInstallPollInterval
}

// sourceOptions defines the rows for the source-selection table.
var sourceOptions = []struct {
	label string
	desc  string
	mode  int
}{
	{"Open project", "Register an existing local repo", sourceModeOpen},
	{"Clone from URL", "Clone a repo and register it", sourceModeClone},
}

// repoAddFormData holds huh form-bound values on the heap so that Value()
// pointers remain valid across bubbletea value-receiver copies of RepoAddModel.
type repoAddFormData struct {
	gitURL    string
	clonePath string
	localPath string
	name      string
	setup     string

	// Core repo-behavior config collected at add time. Seeded to the bossd
	// creation defaults; persisted via a follow-up UpdateRepo only when changed.
	// Integrations (Linear/Sentry) are intentionally NOT here: they need the
	// repo-settings screen's collapsible, validated layout (key+org are a unit),
	// which huh can't reproduce inline. They're configured there after add.
	mergeStrategy string   // "merge" | "rebase" | "squash"
	automation    []string // subset of the automation* values

	confirm bool
}

// repoConfigSnapshot is an immutable copy of the add-form options that feed the
// post-create UpdateRepo. submitRepo captures it before launching the async
// create/clone RPC: fd stays bound to the live form, and the view keeps handling
// keys while the RPC is in flight (esc can rebuild the form), so reading fd after
// the RPC returns could persist edits the user never submitted.
type repoConfigSnapshot struct {
	mergeStrategy string
	automation    []string
}

// configSnapshot copies the config fields into an immutable snapshot, cloning the
// automation slice so later form mutations cannot alias it.
func (fd *repoAddFormData) configSnapshot() repoConfigSnapshot {
	return repoConfigSnapshot{
		mergeStrategy: fd.mergeStrategy,
		automation:    slices.Clone(fd.automation),
	}
}

// RepoAddModel is the wizard for registering a new repository.
type RepoAddModel struct {
	client client.BossClient
	auth   *auth.Manager
	ctx    context.Context

	phase repoAddPhase
	err   error
	// configErr is set when the repo was created but persisting its add-time
	// options failed; the wizard shows a non-fatal notice and esc completes.
	configErr error
	done      bool
	cancel    bool

	// Source selection table (phase 1)
	sourceTable table.Model

	sourceMode int

	// Form-bound values (heap-allocated for stable pointers)
	fd *repoAddFormData

	// Async state
	validating bool
	cloning    bool
	spinner    spinner.Model

	// Validation results
	isGithub           bool
	detectedBaseBranch string
	detectedOriginURL  string

	createdRepo *pb.Repo

	githubAppClient        GitHubAppClient
	githubAppAttempt       int
	githubAppInstallURL    string
	githubAppInstallErr    error
	githubAppInstallOpen   bool
	githubAppInstallNWO    string
	githubAppInstallReady  bool
	githubAppPollCount     int
	githubAppPollExhausted bool

	// Form (used for clone/open input fields + details phase)
	form *huh.Form
	// formFields is the ordered field slice handed to huh.NewGroup, retained
	// because huh.Form exposes no enumeration; it backs click-to-focus (BOS-512).
	// Rebuilt by buildInputForm/buildDetailsForm alongside form itself.
	formFields formFields

	// Layout
	width  int
	height int

	// returnHomeOnCancel routes back to ViewHome (instead of ViewRepoList) when
	// the user cancels. Set by the App when the add-repo wizard was opened via
	// the zero-repo auto-redirect.
	returnHomeOnCancel bool
}

// NewRepoAddModel creates a RepoAddModel with sensible defaults.
func NewRepoAddModel(c client.BossClient, ctx context.Context) RepoAddModel {
	home, _ := os.UserHomeDir()

	m := RepoAddModel{
		client:             c,
		ctx:                ctx,
		phase:              repoAddPhaseSource,
		detectedBaseBranch: "main",
		spinner:            newStatusSpinner(),
		fd: &repoAddFormData{
			localPath:     home + "/",
			name:          filepath.Base(home),
			mergeStrategy: defaultRepoMergeStrategy,
			automation:    defaultRepoAutomation(),
			confirm:       true,
		},
	}
	m.buildSourceTable()
	return m
}

func (m *RepoAddModel) SetGitHubAppInstall(authMgr *auth.Manager, c GitHubAppClient) {
	m.auth = authMgr
	m.githubAppClient = c
}

func (m *RepoAddModel) buildSourceTable() {
	cols := []table.Column{
		cursorColumn,
		{Title: "", Width: 14 + tableColumnSep},
		{Title: "", Width: 32 + tableColumnSep},
	}
	rows := make([]table.Row, len(sourceOptions))
	for i, opt := range sourceOptions {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.sourceTable = newBossTable(cols, rows, len(sourceOptions)+1)
	m.sourceTable.SetWidth(columnsWidth(cols))
}

// repoAddChrome is the number of rendered lines that surround the huh form in
// the form phases, subtracted from terminal height when sizing the form so the
// action bar stays on screen. It covers the banner App.View prepends
// (bannerOverhead), the title + blank line, and the action bar (top+bottom
// padding plus its formActionBarLines text lines). Mirrors cronFormChrome in
// cron_form.go.
const repoAddChrome = bannerOverhead + 2 /*title+blank*/ + (actionBarPadY*2 + formActionBarLines) /*action bar*/

// formHeight returns the height to constrain the huh form to so the action bar
// below it stays on screen. It returns 0 when the terminal height is unknown
// (WithHeight(0) is a no-op in huh, leaving the form unconstrained). Without it
// the 5-field details form renders 22 lines which, plus the banner, overflows an
// 80x24 terminal and clips the action bar this view exists to show.
func (m RepoAddModel) formHeight() int {
	if m.height <= 0 {
		return 0
	}
	return max(m.height-repoAddChrome, 3)
}

func (m *RepoAddModel) buildInputForm() {
	var fields []huh.Field
	if m.sourceMode == sourceModeClone {
		fields = []huh.Field{
			huh.NewInput().
				Title("Git URL").
				Placeholder("https://github.com/user/repo.git").
				Value(&m.fd.gitURL).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("URL is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Clone path").
				Placeholder("Clone destination path").
				Value(&m.fd.clonePath).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("clone path is required")
					}
					return nil
				}),
		}
	} else {
		fields = []huh.Field{
			huh.NewInput().
				Title("Path").
				Placeholder("Repository path").
				Value(&m.fd.localPath).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("path is required")
					}
					return nil
				}),
		}
	}
	m.form = newBossForm(fields...)
	m.formFields = newFormFields(fields...)
}

func (m *RepoAddModel) buildDetailsForm() {
	automationOptions := []huh.Option[string]{
		huh.NewOption("Mark ready for review when checks pass", automationAutoMerge),
		huh.NewOption("Auto-merge Dependabot PRs", automationDependabot),
		huh.NewOption("Automatic repair (failing checks, conflicts, review feedback)", automationRepair),
	}
	// Hoisted so the select is sized from len() rather than a literal: a fourth
	// strategy added inline would otherwise be clipped with no error.
	mergeStrategyOptions := []huh.Option[string]{
		huh.NewOption("Merge commit", string(models.MergeStrategyMerge)),
		huh.NewOption("Rebase", string(models.MergeStrategyRebase)),
		huh.NewOption("Squash", string(models.MergeStrategySquash)),
	}
	fields := []huh.Field{
		huh.NewInput().
			Title("Name").
			Placeholder("Display name").
			Value(&m.fd.name).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("name is required")
				}
				return nil
			}),
		huh.NewInput().
			Title("Setup command").
			Placeholder("Optional, e.g. make setup").
			Value(&m.fd.setup),
		bossSelect[string](len(mergeStrategyOptions), 1).
			Title("Merge strategy").
			Options(mergeStrategyOptions...).
			Value(&m.fd.mergeStrategy),
		// The explicit height is why bossMultiSelect takes the option count:
		// with huh v2's auto-height the option viewport is sized to
		// (options height - title height) and silently clips the last option
		// (the "Automatic repair" row never rendered otherwise).
		//
		// The count only buys that up to bossSelectMaxHeight; past the cap huh
		// scrolls the viewport, and it draws no scroll indicator, so a seventh
		// automation option would be off-screen with nothing to say so. Three
		// today — revisit the cap here if the list ever gets that long.
		bossMultiSelect[string](len(automationOptions), 1).
			Title("Automation").
			Value(&m.fd.automation).
			Options(automationOptions...),
		bossConfirm().
			Affirmative("Add Repository").
			Negative("Cancel").
			Value(&m.fd.confirm),
	}
	// Only the details form needs a height budget: it is the one form here
	// tall enough to push the action bar off a short terminal. The input
	// forms are 1-2 fields, and huh's viewport pads to the height it is
	// given, so constraining them would inflate them instead.
	m.form = newBossForm(fields...).WithHeight(m.formHeight())
	m.formFields = newFormFields(fields...)
}

func (m RepoAddModel) Init() tea.Cmd {
	if m.form != nil {
		return m.form.Init()
	}
	return nil
}

func (m RepoAddModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.form != nil && m.phase == repoAddPhaseDetails {
			return m, resizeForm(m.form, m.formHeight(), msg)
		}
		return m, nil

	case repoRegisteredMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.createdRepo = msg.repo
		return m.afterRepoCreated(msg.configErr), nil

	case repoClonedMsg:
		m.cloning = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.createdRepo = msg.repo
		return m.afterRepoCreated(msg.configErr), nil

	case repoValidatedMsg:
		m.validating = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		if !msg.resp.IsValid {
			m.err = fmt.Errorf("%s", msg.resp.ErrorMessage)
			// Go back to input phase so user can fix path.
			m.phase = repoAddPhaseInput
			m.buildInputForm()
			return m, m.form.Init()
		}
		// Auto-populate fields from validation response.
		m.isGithub = msg.resp.IsGithub
		m.detectedOriginURL = msg.resp.OriginUrl
		if msg.resp.DefaultBranch != "" {
			m.detectedBaseBranch = msg.resp.DefaultBranch
		}
		if msg.resp.IsGithub {
			if nwo := vcs.GitHubNWO(msg.resp.OriginUrl); nwo != "" {
				m.fd.name = "@" + nwo
			} else {
				m.fd.name = filepath.Base(m.fd.localPath)
			}
		} else if n := parseRepoNameFromURL(msg.resp.OriginUrl); n != "" {
			m.fd.name = n
		} else {
			m.fd.name = filepath.Base(m.fd.localPath)
		}
		// Advance to details phase.
		m.phase = repoAddPhaseDetails
		m.buildDetailsForm()
		return m, m.form.Init()

	case githubAppInstallURLMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		if msg.err != nil {
			m.githubAppInstallErr = msg.err
			return m, nil
		}
		m.githubAppInstallURL = msg.url
		m.githubAppInstallOpen = true
		return m, tea.Batch(
			m.openGitHubAppInstall(msg.url, msg.attempt),
			m.githubAppPollTick(msg.attempt),
		)

	case githubAppBrowserOpenedMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		m.githubAppInstallURL = msg.url
		m.githubAppInstallErr = msg.err
		return m, nil

	case githubAppPollTickMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		return m, m.listGitHubAppRepos(msg.attempt)

	case githubAppReposMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		if msg.err != nil {
			m.githubAppInstallErr = msg.err
			cmd := m.nextGitHubAppPoll(msg.attempt)
			return m, cmd
		}
		if m.githubAppRepoInstalled(msg.repos) {
			m.githubAppInstallReady = true
			m.done = true
			return m, nil
		}
		cmd := m.nextGitHubAppPoll(msg.attempt)
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.phase == repoAddPhaseGitHubApp || m.cloning {
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		if m.phase == repoAddPhaseGitHubAppPrompt {
			switch msg.String() {
			case "esc":
				m.done = true
				return m, nil
			case "enter":
				return m.startGitHubAppInstall()
			}
			return m, nil
		}
		if m.phase == repoAddPhaseGitHubApp {
			switch msg.String() {
			case "esc":
				m.done = true
				return m, nil
			case "enter", "o":
				m.githubAppInstallErr = nil
				if m.githubAppPollExhausted {
					// Resume automatic polling and re-open the install page.
					m.githubAppPollExhausted = false
					m.githubAppPollCount = 0
					return m, tea.Batch(m.reopenGitHubAppInstallCmd(), m.githubAppPollTick(m.githubAppAttempt))
				}
				return m, m.reopenGitHubAppInstallCmd()
			}
			return m, nil
		}
		if msg.String() == "esc" {
			// The repo was created but its options failed to save: esc dismisses
			// the notice and completes the add (the repo exists either way).
			if m.configErr != nil {
				m.done = true
				return m, nil
			}
			// If showing an error, go back to the input form with data preserved.
			if m.err != nil {
				m.err = nil
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				return m, m.form.Init()
			}
			if m.phase == repoAddPhaseInput {
				// Go back to source selection.
				m.phase = repoAddPhaseSource
				m.form = nil
				return m, nil
			}
			if m.phase == repoAddPhaseDetails {
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				return m, m.form.Init()
			}
			m.cancel = true
			return m, nil
		}
	}

	// Source phase: table navigation.
	if m.phase == repoAddPhaseSource {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "enter" {
				m.sourceMode = sourceOptions[m.sourceTable.Cursor()].mode
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				return m, m.form.Init()
			}
			var cmd tea.Cmd
			m.sourceTable, cmd = m.sourceTable.Update(msg)
			updateCursorColumn(&m.sourceTable)
			return m, cmd
		}
		return m, nil
	}

	// Delegate to form when active (input + details phases). Once the repo is
	// created but its options failed to save, the config-error notice owns the
	// screen — keep stray keys away from the now-hidden details form so they
	// can't re-submit it.
	if m.form != nil && m.phase != repoAddPhaseDone && m.configErr == nil {
		// Click-to-focus, ahead of huh: huh v2 has no mouse support, so every
		// mouse-shaped message is consumed here rather than forwarded (BOS-512).
		if cmd, handled := m.formFields.handleMouse(msg, m.form, linesBefore(m.formPrefix())); handled {
			return m, cmd
		}

		_, cmd := m.form.Update(msg)

		if m.form.State == huh.StateAborted {
			if m.phase == repoAddPhaseInput {
				m.phase = repoAddPhaseSource
				m.form = nil
				m.err = nil
				return m, nil
			}
			if m.phase == repoAddPhaseDetails {
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				m.err = nil
				return m, m.form.Init()
			}
			m.cancel = true
			return m, nil
		}

		if m.form.State == huh.StateCompleted {
			return m.handleFormCompleted()
		}

		return m, cmd
	}

	return m, nil
}

func (m RepoAddModel) shouldOfferGitHubAppInstall() bool {
	if m.auth == nil || m.githubAppClient == nil {
		return false
	}
	if status := m.auth.Status(); status == nil || !status.LoggedIn {
		return false
	}
	origin := m.githubAppRepoOrigin()
	return vcs.GitHubNWO(origin) != ""
}

func (m RepoAddModel) githubAppRepoOrigin() string {
	if m.sourceMode == sourceModeClone {
		return strings.TrimSpace(m.fd.gitURL)
	}
	if m.detectedOriginURL != "" {
		return strings.TrimSpace(m.detectedOriginURL)
	}
	if m.createdRepo != nil {
		return strings.TrimSpace(m.createdRepo.OriginUrl)
	}
	return ""
}

func (m RepoAddModel) promptGitHubAppInstall() RepoAddModel {
	m.phase = repoAddPhaseGitHubAppPrompt
	m.githubAppInstallErr = nil
	m.githubAppInstallNWO = vcs.GitHubNWO(m.githubAppRepoOrigin())
	return m
}

// afterRepoCreated advances the wizard once the repo exists. A non-nil configErr
// means the repo was created but persisting its add-time options failed; that is
// non-fatal, so the wizard shows a notice (esc completes) instead of offering
// the GitHub App install. Otherwise it offers the install follow-up when
// applicable, or finishes.
func (m RepoAddModel) afterRepoCreated(configErr error) RepoAddModel {
	if configErr != nil {
		m.configErr = configErr
		return m
	}
	if m.shouldOfferGitHubAppInstall() {
		return m.promptGitHubAppInstall()
	}
	m.done = true
	return m
}

func (m RepoAddModel) startGitHubAppInstall() (RepoAddModel, tea.Cmd) {
	m.phase = repoAddPhaseGitHubApp
	m.githubAppAttempt++
	m.githubAppInstallURL = ""
	m.githubAppInstallErr = nil
	m.githubAppInstallOpen = false
	m.githubAppInstallReady = false
	m.githubAppPollCount = 0
	m.githubAppPollExhausted = false
	m.githubAppInstallNWO = vcs.GitHubNWO(m.githubAppRepoOrigin())
	return m, tea.Batch(m.spinner.Tick, m.requestGitHubAppInstallURL(m.githubAppAttempt))
}

// nextGitHubAppPoll schedules the next installation poll, or stops and marks
// the loop exhausted once githubAppInstallMaxPolls have elapsed without the app
// appearing. Pointer receiver so it updates the poll counter on the caller's
// model copy before that copy is returned from Update.
func (m *RepoAddModel) nextGitHubAppPoll(attempt int) tea.Cmd {
	m.githubAppPollCount++
	if m.githubAppPollCount >= githubAppInstallMaxPolls {
		m.githubAppPollExhausted = true
		return nil
	}
	return m.githubAppPollTick(attempt)
}

// reopenGitHubAppInstallCmd re-opens the install page when the URL is already
// known, otherwise re-requests it.
func (m RepoAddModel) reopenGitHubAppInstallCmd() tea.Cmd {
	if m.githubAppInstallURL != "" {
		return m.openGitHubAppInstall(m.githubAppInstallURL, m.githubAppAttempt)
	}
	return m.requestGitHubAppInstallURL(m.githubAppAttempt)
}

func (m RepoAddModel) requestGitHubAppInstallURL(attempt int) tea.Cmd {
	return func() tea.Msg {
		url, err := m.githubAppClient.GetGitHubAppInstallURL(m.ctx, "")
		if err == nil {
			url = enrichGitHubAppInstallURL(m.ctx, url, m.githubAppRepoOrigin())
		}
		return githubAppInstallURLMsg{url: url, err: err, attempt: attempt}
	}
}

func (m RepoAddModel) openGitHubAppInstall(rawURL string, attempt int) tea.Cmd {
	return func() tea.Msg {
		err := openGitHubAppInstallURL(rawURL)
		return githubAppBrowserOpenedMsg{url: rawURL, err: err, attempt: attempt}
	}
}

func (m RepoAddModel) githubAppPollTick(attempt int) tea.Cmd {
	return tea.Tick(githubAppPollIntervalValue(), func(time.Time) tea.Msg {
		return githubAppPollTickMsg{attempt: attempt}
	})
}

func (m RepoAddModel) listGitHubAppRepos(attempt int) tea.Cmd {
	return func() tea.Msg {
		repos, err := m.githubAppClient.ListGitHubAppRepos(m.ctx)
		return githubAppReposMsg{repos: repos, err: err, attempt: attempt}
	}
}

func (m RepoAddModel) githubAppRepoInstalled(repos []*pb.GitHubAppRepoStatus) bool {
	for _, repo := range repos {
		if repo.GetInstalled() && strings.EqualFold(repo.GetOwner()+"/"+repo.GetName(), m.githubAppInstallNWO) {
			return true
		}
	}
	return false
}

func (m *RepoAddModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	switch m.phase {
	case repoAddPhaseInput:
		if m.sourceMode == sourceModeClone {
			// Derive defaults from URL.
			repoName := parseRepoNameFromURL(m.fd.gitURL)
			if repoName != "" {
				home, _ := os.UserHomeDir()
				if m.fd.clonePath == "" {
					m.fd.clonePath = filepath.Join(home, "Code", repoName)
				}
				if vcs.IsGitHubURL(m.fd.gitURL) {
					if nwo := vcs.GitHubNWO(m.fd.gitURL); nwo != "" {
						m.fd.name = "@" + nwo
					} else {
						m.fd.name = repoName
					}
				} else {
					m.fd.name = repoName
				}
			}
			// Go straight to details.
			m.phase = repoAddPhaseDetails
			m.buildDetailsForm()
			return *m, m.form.Init()
		}
		// Open mode — validate path first.
		m.validating = true
		m.err = nil
		localPath := m.fd.localPath
		return *m, func() tea.Msg {
			resp, err := m.client.ValidateRepoPath(m.ctx, localPath)
			return repoValidatedMsg{resp: resp, err: err}
		}

	case repoAddPhaseDetails:
		if !m.fd.confirm {
			m.cancel = true
			return *m, nil
		}
		return *m, m.submitRepo()

	case repoAddPhaseSource:
		// Source table selection — no form completion to handle.
	case repoAddPhaseGitHubAppPrompt:
		// GitHub App prompt is handled by key messages, not form completion.
	case repoAddPhaseGitHubApp:
		// GitHub App installation is driven by cloud poll messages, not form completion.
	case repoAddPhaseDone:
		// Nothing to do.
	}
	return *m, nil
}

func (m *RepoAddModel) submitRepo() tea.Cmd {
	cfg, _ := config.Load()
	c := m.client
	ctx := m.ctx
	fd := m.fd
	// Snapshot the config fields now, while we know they reflect what the user
	// submitted; the closures below run after the create RPC, by which time fd may
	// have been mutated by in-flight key handling.
	cfgSnap := fd.configSnapshot()

	if m.sourceMode == sourceModeClone {
		m.cloning = true
		req := &pb.CloneAndRegisterRepoRequest{
			CloneUrl:          fd.gitURL,
			LocalPath:         fd.clonePath,
			DisplayName:       fd.name,
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   cfg.WorktreeBaseDir,
		}
		if s := fd.setup; s != "" {
			req.SetupScript = &s
		}
		return func() tea.Msg {
			repo, err := c.CloneAndRegisterRepo(ctx, req)
			if err != nil {
				return repoClonedMsg{err: err}
			}
			repo, cfgErr := applyRepoConfig(ctx, c, repo, cfgSnap)
			return repoClonedMsg{repo: repo, configErr: cfgErr}
		}
	}

	req := &pb.RegisterRepoRequest{
		LocalPath:         fd.localPath,
		DisplayName:       fd.name,
		DefaultBaseBranch: m.detectedBaseBranch,
		WorktreeBaseDir:   cfg.WorktreeBaseDir,
	}
	if s := fd.setup; s != "" {
		req.SetupScript = &s
	}
	return func() tea.Msg {
		repo, err := c.RegisterRepo(ctx, req)
		if err != nil {
			return repoRegisteredMsg{err: err}
		}
		repo, cfgErr := applyRepoConfig(ctx, c, repo, cfgSnap)
		return repoRegisteredMsg{repo: repo, configErr: cfgErr}
	}
}

// applyRepoConfig persists the add-form merge-strategy / automation options onto
// a freshly-created repo via UpdateRepo. It returns the updated repo (or the
// original when nothing changed) and any error from the settings write. The repo
// already exists, so callers treat that error as non-fatal.
func applyRepoConfig(ctx context.Context, c client.BossClient, repo *pb.Repo, snap repoConfigSnapshot) (*pb.Repo, error) {
	req, changed := repoConfigUpdate(repo.GetId(), snap)
	if !changed {
		return repo, nil
	}
	updated, err := c.UpdateRepo(ctx, req)
	if err != nil {
		return repo, err
	}
	return updated, nil
}

// changedBool reports whether cur differs from the creation default def. Wrapping
// the comparison keeps the named default constants readable (a bare `cur != true`
// literal would be folded away by staticcheck) while documenting the WYSIWYG
// "persist only what the user changed" intent.
func changedBool(cur, def bool) bool { return cur != def }

// repoConfigUpdate builds an UpdateRepoRequest for any add-form options that
// differ from the bossd creation defaults. The bool reports whether anything
// needs persisting.
func repoConfigUpdate(repoID string, snap repoConfigSnapshot) (*pb.UpdateRepoRequest, bool) {
	req := &pb.UpdateRepoRequest{Id: repoID}
	changed := false

	if ms := snap.mergeStrategy; ms != "" && ms != defaultRepoMergeStrategy {
		req.MergeStrategy = &ms
		changed = true
	}

	autoMerge := slices.Contains(snap.automation, automationAutoMerge)
	dependabot := slices.Contains(snap.automation, automationDependabot)
	repair := slices.Contains(snap.automation, automationRepair)
	if changedBool(autoMerge, defaultCanAutoMerge) {
		req.CanAutoMerge = &autoMerge
		changed = true
	}
	if changedBool(dependabot, defaultCanAutoMergeDependabot) {
		req.CanAutoMergeDependabot = &dependabot
		changed = true
	}
	if changedBool(repair, defaultCanAutoRepair) {
		req.CanAutoRepair = &repair
		changed = true
	}

	return req, changed
}

// Cancelled returns true if the user cancelled.
func (m RepoAddModel) Cancelled() bool { return m.cancel }

// Done returns true if registration succeeded.
func (m RepoAddModel) Done() bool { return m.done }

func (m RepoAddModel) View() tea.View {
	if m.phase == repoAddPhaseGitHubAppPrompt {
		return tea.NewView(m.githubAppInstallPromptView())
	}

	if m.phase == repoAddPhaseGitHubApp {
		return tea.NewView(m.githubAppInstallView())
	}

	if m.validating {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorInfo).Render(
				fmt.Sprintf("Validating %s...", m.fd.localPath)),
		)
	}

	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				actionBar([]string{"[esc] back"}),
		)
	}

	if m.cloning {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorInfo).Render(
				fmt.Sprintf("Cloning %s...", m.fd.gitURL)),
		)
	}

	if m.configErr != nil && !m.done && m.createdRepo != nil {
		name := m.createdRepo.DisplayName
		body := fmt.Sprintf(
			"Repository %q was added, but saving its options failed:\n\n  %v\n\nYou can adjust them later in the repo settings.",
			name, m.configErr)
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(body) + "\n" +
				actionBar([]string{"[esc] continue"}),
		)
	}

	if m.done && m.createdRepo != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render("Repository registered!") + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render(
					fmt.Sprintf("  ID:   %s\n  Name: %s\n  Path: %s",
						m.createdRepo.Id, m.createdRepo.DisplayName, m.createdRepo.LocalPath)),
		)
	}

	if m.phase == repoAddPhaseSource {
		var b strings.Builder
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.sourceTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.form != nil {
		var b strings.Builder
		b.WriteString(m.formPrefix())
		// formOnScreen, not "form != nil": huh renders nothing once the form is
		// completed, which is the state this branch is in while the RegisterRepo
		// round-trip runs (that path sets neither validating nor cloning, so
		// every early return above is skipped). Advertising [click] and enabling
		// mouse reporting on a screen with no fields would be a lie (BOS-512).
		onScreen := formOnScreen(m.form)
		submit := "[enter] continue"
		if m.phase == repoAddPhaseDetails {
			submit = "[enter] add repo"
		}
		if !onScreen {
			b.WriteString(actionBar([]string{submit}, []string{"[esc] back"}))
			return tea.NewView(b.String())
		}

		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()))
		// styleActionBar's Padding(actionBarPadY, 2) supplies the blank line
		// above the bar, so the form ends with a single newline and no more.
		b.WriteString("\n")
		// Verbs are kept short on purpose. The nav hints moved onto their own
		// line when the [click] hint joined them (BOS-512), so they no longer
		// share a line with the verb — but repoAddChrome budgets exactly
		// formActionBarLines, so a verb long enough to wrap its own line would
		// still clip the bar this view exists to show.
		b.WriteString(formActionBar([]string{submit}, []string{"[esc] back"}))
		v := tea.NewView(b.String())
		// Mouse reporting is scoped to screens that actually render a form
		// (BOS-512): the source table, validating, cloning, error, config-error
		// and done branches all return above, and a completed form takes the
		// !onScreen path, all keeping the zero value MouseModeNone.
		v.MouseMode = tea.MouseModeCellMotion
		return v
	}

	return tea.NewView("")
}

// formPrefix is everything the form branch of View renders above the huh form:
// the phase title and the blank line under it. The click hit test measures this
// same string, so a line added here moves both the render and the hit test
// together (BOS-512).
func (m RepoAddModel) formPrefix() string {
	title := "Add a local repository"
	if m.sourceMode == sourceModeClone {
		title = "Clone a repository from URL"
	}
	return styleTitle.Render(title) + "\n\n"
}

func (m RepoAddModel) githubAppInstallPromptView() string {
	repoLabel := m.githubAppInstallNWO
	if repoLabel == "" && m.createdRepo != nil {
		repoLabel = m.createdRepo.DisplayName
	}
	if repoLabel == "" {
		repoLabel = "this repository"
	}

	return githubAppInstallPromptContent(repoLabel)
}

func githubAppInstallPromptContent(repoLabel string) string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	body := "Install the Bossanova Github App on " + repoLabel + "?\n\n" +
		"This enables automated realtime response to conflicts, failing tests and code reviews.\n\n" +
		"You can always set this up later if you don't want to decide yet."
	return padding.Render(body) + "\n" +
		styleActionBar.Render("[enter] install  [esc] skip")
}

func (m RepoAddModel) githubAppInstallView() string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	repoLabel := m.githubAppInstallNWO
	if repoLabel == "" && m.createdRepo != nil {
		repoLabel = m.createdRepo.DisplayName
	}
	if repoLabel == "" {
		repoLabel = "this repository"
	}

	if m.githubAppPollExhausted {
		body := "Still waiting for the GitHub App installation on " + repoLabel + ".\n" +
			"Automatic checking has paused."
		if m.githubAppInstallURL != "" {
			body += "\nInstall URL: " + m.githubAppInstallURL
		}
		return padding.Render(body) + "\n" +
			styleActionBar.Render("[enter] keep checking  [esc] skip")
	}

	body := m.spinner.View() + "Waiting for GitHub App installation on " + repoLabel + "..."
	if !m.githubAppInstallOpen {
		body = m.spinner.View() + "Opening GitHub App installation page..."
	}
	if m.githubAppInstallErr != nil && m.githubAppInstallURL != "" {
		body += "\nOpen this GitHub App URL: " + m.githubAppInstallURL
	}
	if m.githubAppInstallErr != nil && m.githubAppInstallURL == "" {
		body = "Could not start GitHub App installation: " + m.githubAppInstallErr.Error()
	}

	return padding.Render(body) + "\n" +
		styleActionBar.Render("[enter] re-open install page  [esc] skip")
}
