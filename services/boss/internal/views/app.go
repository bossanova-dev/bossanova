// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// View identifies which screen is currently active.
type View int

const (
	ViewHome View = iota
	ViewNewSession
	ViewAttach
	ViewChatPicker
	ViewRepoAdd
	ViewRepoList
	ViewRepoSettings
	ViewTrash
	ViewSettings
	ViewSessionSettings
	ViewLogin
	ViewBugReport
	ViewCron
	ViewCronForm
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client          client.BossClient
	auth            *auth.Manager
	ctx             context.Context
	ptyManager      *bosspty.Manager
	activeView      View
	home            HomeModel
	newSession      NewSessionModel
	chatPicker      ChatPickerModel
	repoAdd         RepoAddModel
	repoList        RepoListModel
	repoSettings    RepoSettingsModel
	sessionSettings SessionSettingsModel
	trash           TrashModel
	settings        SettingsModel
	attach          AttachModel
	login           LoginModel
	bugReport       BugReportModel
	cronList        CronListModel
	cronForm        CronFormModel
	width           int
	height          int
	quitting        bool
}

// NewApp creates a new App wired to the daemon client.
func NewApp(c client.BossClient, authMgr *auth.Manager) App {
	ctx := context.Background()
	home := NewHomeModel(c, ctx, authMgr)
	return App{
		client:     c,
		auth:       authMgr,
		ctx:        ctx,
		ptyManager: bosspty.NewManager(),
		activeView: ViewHome,
		home:       home,
	}
}

// SetInitialView overrides the default initial view before running the program.
func (a *App) SetInitialView(v View) {
	a.activeView = v
	switch v {
	case ViewNewSession:
		a.newSession = NewNewSessionModel(a.client, a.ctx)
	case ViewRepoAdd:
		a.repoAdd = NewRepoAddModel(a.client, a.ctx)
	case ViewRepoList:
		a.repoList = NewRepoListModel(a.client, a.ctx)
	default:
	}
}

// SetAttachSession sets the session ID to attach to. Must be called after SetInitialView(ViewAttach).
func (a *App) SetAttachSession(sessionID, resumeID string) {
	a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, sessionID, resumeID)
}

func (a App) Init() tea.Cmd {
	var viewCmd tea.Cmd
	switch a.activeView {
	case ViewNewSession:
		viewCmd = a.newSession.Init()
	case ViewChatPicker:
		viewCmd = a.chatPicker.Init()
	case ViewRepoAdd:
		viewCmd = a.repoAdd.Init()
	case ViewRepoList:
		viewCmd = a.repoList.Init()
	case ViewAttach:
		viewCmd = a.attach.Init()
	default:
		viewCmd = a.home.Init()
	}
	return tea.Batch(viewCmd, heartbeatTickCmd())
}

// switchViewMsg requests the app to switch to a different view.
type switchViewMsg struct {
	view      View
	sessionID string // used for ViewAttach and ViewChatPicker
	resumeID  string // Claude Code session UUID to resume (ViewAttach only)
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.home.width = msg.Width
		a.home.height = msg.Height
		a.newSession.width = msg.Width
		a.repoAdd.width = msg.Width
		a.repoList.width = msg.Width
		a.repoList.height = msg.Height
		a.repoSettings.width = msg.Width
		a.trash.width = msg.Width
		a.trash.height = msg.Height
		a.chatPicker.width = msg.Width
		a.chatPicker.height = msg.Height
		a.settings.width = msg.Width
		a.sessionSettings.width = msg.Width
		a.login.width = msg.Width
		a.bugReport.width = msg.Width
		a.cronList.width = msg.Width
		a.cronList.height = msg.Height
		a.cronForm.width = msg.Width

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			a.quitting = true
			return a, tea.Quit
		case "ctrl+b":
			if a.activeView == ViewBugReport {
				break
			}
			a.bugReport = NewBugReportModel(a.client, a.ctx, a.auth, a.activeView, a.currentSession(), a.currentDaemonStatuses())
			a.bugReport.width = a.width
			a.activeView = ViewBugReport
			return a, a.bugReport.Init()
		}

	case heartbeatTickMsg:
		// Snapshot the local PTY manager and ship per-chat statuses to bossd
		// so other clients (web UI, second TUI instance) see this client's
		// "working/idle/question" state. Re-arm the ticker so the loop is
		// self-perpetuating.
		return a, tea.Batch(
			sendHeartbeatsCmd(a.ctx, a.client, a.ptyManager),
			heartbeatTickCmd(),
		)

	case switchViewMsg:
		a.activeView = msg.view
		switch msg.view { //nolint:exhaustive // ViewBugReport is pushed via ctrl+b, not switchViewMsg
		case ViewNewSession:
			a.newSession = NewNewSessionModel(a.client, a.ctx)
			a.newSession.width = a.width
			return a, a.newSession.Init()
		case ViewChatPicker:
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, msg.sessionID, "")
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			return a, a.chatPicker.Init()
		case ViewRepoAdd:
			a.repoAdd = NewRepoAddModel(a.client, a.ctx)
			a.repoAdd.width = a.width
			return a, a.repoAdd.Init()
		case ViewRepoList:
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.width = a.width
			a.repoList.height = a.height
			return a, a.repoList.Init()
		case ViewRepoSettings:
			a.repoSettings = NewRepoSettingsModel(a.client, a.ctx, msg.sessionID)
			a.repoSettings.width = a.width
			return a, a.repoSettings.Init()
		case ViewSessionSettings:
			a.sessionSettings = NewSessionSettingsModel(a.client, a.ctx, msg.sessionID)
			a.sessionSettings.width = a.width
			return a, a.sessionSettings.Init()
		case ViewTrash:
			a.trash = NewTrashModel(a.client, a.ctx)
			a.trash.width = a.width
			a.trash.height = a.height
			return a, a.trash.Init()
		case ViewSettings:
			a.settings = NewSettingsModel()
			a.settings.width = a.width
			return a, a.settings.Init()
		case ViewAttach:
			a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, msg.sessionID, msg.resumeID)
			return a, a.attach.Init()
		case ViewLogin:
			a.login = NewLoginModel(a.auth, a.client, a.ctx)
			a.login.width = a.width
			return a, a.login.Init()
		case ViewHome:
			a.home = NewHomeModel(a.client, a.ctx, a.auth)
			a.home.width = a.width
			a.home.height = a.height
			return a, a.home.Init()
		case ViewCron:
			a.cronList = NewCronListModel(a.client, a.ctx)
			a.cronList.width = a.width
			a.cronList.height = a.height
			return a, a.cronList.Init()
		case ViewCronForm:
			a.cronForm = NewCronFormModel(a.client, a.ctx)
			a.cronForm.width = a.width
			return a, a.cronForm.Init()
		}
		return a, nil
	}

	switch a.activeView {
	case ViewHome:
		updated, cmd := a.home.Update(msg)
		a.home = updated.(HomeModel)
		return a, cmd
	case ViewNewSession:
		updated, cmd := a.newSession.Update(msg)
		a.newSession = updated.(NewSessionModel)
		if a.newSession.Cancelled() {
			return a, a.switchToHome()
		}
		if a.newSession.Done() {
			sess := a.newSession.CreatedSession()
			if sess != nil {
				a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, sess.Id, "")
				a.activeView = ViewAttach
				return a, a.attach.Init()
			}
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewChatPicker:
		updated, cmd := a.chatPicker.Update(msg)
		a.chatPicker = updated.(ChatPickerModel)
		if a.chatPicker.Cancelled() || a.chatPicker.Merged() {
			sessionID := a.chatPicker.sessionID
			merged := a.chatPicker.Merged()
			a.activeView = ViewHome
			a.home = NewHomeModel(a.client, a.ctx, a.auth)
			a.home.highlightSessionID = sessionID
			if merged {
				a.home.mergedOptimisticID = sessionID
			}
			a.home.width = a.width
			a.home.height = a.height
			return a, a.home.Init()
		}
		return a, cmd
	case ViewRepoAdd:
		updated, cmd := a.repoAdd.Update(msg)
		a.repoAdd = updated.(RepoAddModel)
		if a.repoAdd.Cancelled() || a.repoAdd.Done() {
			var highlightID string
			if cursor := a.repoList.table.Cursor(); cursor >= 0 && cursor < len(a.repoList.repos) {
				highlightID = a.repoList.repos[cursor].Id
			}
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.highlightRepoID = highlightID
			a.repoList.width = a.width
			a.repoList.height = a.height
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		return a, cmd
	case ViewRepoList:
		updated, cmd := a.repoList.Update(msg)
		a.repoList = updated.(RepoListModel)
		if a.repoList.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewRepoSettings:
		updated, cmd := a.repoSettings.Update(msg)
		a.repoSettings = updated.(RepoSettingsModel)
		if a.repoSettings.Cancelled() || a.repoSettings.Done() {
			// Return to repo list, highlighting the repo we came from.
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.highlightRepoID = a.repoSettings.repoID
			a.repoList.width = a.width
			a.repoList.height = a.height
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		return a, cmd
	case ViewSessionSettings:
		updated, cmd := a.sessionSettings.Update(msg)
		a.sessionSettings = updated.(SessionSettingsModel)
		if a.sessionSettings.Cancelled() || a.sessionSettings.Done() {
			// Return to chat picker, highlighting the chat we came from.
			var highlightID string
			if cursor := a.chatPicker.table.Cursor(); cursor >= 0 && cursor < len(a.chatPicker.chats) {
				highlightID = a.chatPicker.chats[cursor].ClaudeId
			}
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, a.sessionSettings.sessionID, highlightID)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			a.activeView = ViewChatPicker
			return a, a.chatPicker.Init()
		}
		return a, cmd
	case ViewTrash:
		updated, cmd := a.trash.Update(msg)
		a.trash = updated.(TrashModel)
		if a.trash.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewSettings:
		updated, cmd := a.settings.Update(msg)
		a.settings = updated.(SettingsModel)
		if a.settings.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewAttach:
		updated, cmd := a.attach.Update(msg)
		a.attach = updated.(AttachModel)
		if a.attach.Detached() {
			sessionID := a.attach.SessionID()
			claudeID := a.attach.ClaudeID()
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, sessionID, claudeID)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			a.activeView = ViewChatPicker
			// Batch the attach cleanup cmd (e.g. orphan delete) with the chat picker init.
			return a, tea.Batch(cmd, a.chatPicker.Init())
		}
		return a, cmd
	case ViewLogin:
		updated, cmd := a.login.Update(msg)
		a.login = updated.(LoginModel)
		if a.login.Cancelled() || a.login.Done() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewBugReport:
		updated, cmd := a.bugReport.Update(msg)
		a.bugReport = updated.(BugReportModel)
		if a.bugReport.Cancelled() || a.bugReport.Done() {
			// Restore the prior view without recreating it so existing state
			// (table cursor, loaded data, spinners) is preserved. The prior
			// view's self-perpetuating tick chain was swallowed by the modal,
			// so restart it when the restored view depends on tickMsg.
			a.activeView = a.bugReport.PreviousView()
			return a, resumeTickCmd(a.activeView)
		}
		return a, cmd
	case ViewCron:
		updated, cmd := a.cronList.Update(msg)
		a.cronList = updated.(CronListModel)
		if a.cronList.Cancelled() {
			return a, a.switchToHome()
		}
		// cronFormOpenMsg is emitted by CronListModel as a Cmd; when it
		// arrives here as a message we open the cron form.
		if ofm, ok := msg.(cronFormOpenMsg); ok {
			a.activeView = ViewCronForm
			a.cronForm = NewCronFormModel(a.client, a.ctx)
			a.cronForm.job = ofm.job
			a.cronForm.width = a.width
			return a, a.cronForm.Init()
		}
		return a, cmd
	case ViewCronForm:
		updated, cmd := a.cronForm.Update(msg)
		a.cronForm = updated.(CronFormModel)
		if a.cronForm.Cancelled() {
			// Return to cron list without refreshing (user cancelled).
			a.activeView = ViewCron
			a.cronList = NewCronListModel(a.client, a.ctx)
			a.cronList.width = a.width
			a.cronList.height = a.height
			return a, a.cronList.Init()
		}
		// cronFormDoneMsg is emitted by CronFormModel as a Cmd; when it
		// arrives here as a message we return to the cron list and refresh.
		if _, ok := msg.(cronFormDoneMsg); ok {
			a.activeView = ViewCron
			a.cronList = NewCronListModel(a.client, a.ctx)
			a.cronList.width = a.width
			a.cronList.height = a.height
			return a, a.cronList.Init()
		}
		return a, cmd
	}

	return a, nil
}

// currentSession returns the session associated with the active view, or nil
// if none. Only views that expose a *pb.Session participate.
func (a App) currentSession() *pb.Session {
	if a.activeView == ViewChatPicker {
		return a.chatPicker.Session()
	}
	return nil
}

// currentDaemonStatuses returns the daemon heartbeat statuses tracked by the
// active view, or nil if the view doesn't track any. Keys differ by view —
// Home is session-id keyed, ChatPicker is claude-id keyed — which is fine
// for diagnostic triage.
func (a App) currentDaemonStatuses() map[string]string {
	switch a.activeView { //nolint:exhaustive // only views that track statuses participate
	case ViewChatPicker:
		return a.chatPicker.DaemonStatuses()
	case ViewHome:
		return a.home.DaemonStatuses()
	}
	return nil
}

// resumeTickCmd returns a tickCmd for views whose status refresh depends on a
// self-perpetuating tick chain. The bug-report modal swallows tickMsg while
// it is active, so the chain needs restarting when the modal dismisses back
// to one of these views. Returns nil for views that don't use ticks.
func resumeTickCmd(v View) tea.Cmd {
	switch v { //nolint:exhaustive // only tick-driven views participate
	case ViewHome, ViewChatPicker:
		return tickCmd()
	}
	return nil
}

func (a *App) switchToHome() tea.Cmd {
	highlightID := a.home.selectedSessionID()
	a.activeView = ViewHome
	a.home = NewHomeModel(a.client, a.ctx, a.auth)
	a.home.highlightSessionID = highlightID
	a.home.width = a.width
	a.home.height = a.height
	return a.home.Init()
}

func (a App) View() tea.View {
	if a.quitting {
		return tea.NewView("")
	}

	var v tea.View
	switch a.activeView {
	case ViewHome:
		v = a.home.View()
	case ViewNewSession:
		v = a.newSession.View()
	case ViewChatPicker:
		v = a.chatPicker.View()
	case ViewRepoAdd:
		v = a.repoAdd.View()
	case ViewRepoList:
		v = a.repoList.View()
	case ViewRepoSettings:
		v = a.repoSettings.View()
	case ViewSessionSettings:
		v = a.sessionSettings.View()
	case ViewTrash:
		v = a.trash.View()
	case ViewSettings:
		v = a.settings.View()
	case ViewAttach:
		v = a.attach.View()
	case ViewLogin:
		v = a.login.View()
	case ViewBugReport:
		v = a.bugReport.View()
	case ViewCron:
		v = a.cronList.View()
	case ViewCronForm:
		v = a.cronForm.View()
	default:
		v = tea.NewView("Unknown view")
	}

	// Prepend the banner to every screen except during tea.Exec (AttachModel
	// returns empty content while Claude Code owns the terminal).
	if v.Content != "" {
		var opts bannerOpts
		switch a.activeView { //nolint:exhaustive // only override for specific views
		case ViewChatPicker:
			opts.session = a.chatPicker.session
			opts.spinner = a.chatPicker.spinner
		case ViewRepoSettings:
			opts.repo = a.repoSettings.repo
		case ViewSessionSettings:
			opts.session = a.sessionSettings.session
		case ViewRepoList:
			opts.line1 = "Repositories"
		case ViewTrash:
			opts.line1 = "Archived Sessions"
		case ViewSettings:
			opts.line1 = "Settings"
		case ViewNewSession:
			opts.line1 = "New Session"
		case ViewRepoAdd:
			opts.line1 = "Add Repository"
		case ViewLogin:
			opts.line1 = "Login"
		case ViewBugReport:
			opts.line1 = "Report a bug"
		case ViewCron:
			opts.line1 = "Scheduled Jobs"
		}
		v.Content = renderBanner(a.activeView, opts) + "\n" + v.Content
	}

	v.AltScreen = true
	return v
}
