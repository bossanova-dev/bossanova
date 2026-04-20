// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
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
	ViewAutopilot
	ViewSessionSettings
	ViewLogin
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client          client.BossClient
	auth            *auth.Manager
	ctx             context.Context
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
	autopilot       AutopilotModel
	attach          AttachModel
	login           LoginModel
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
	a.attach = NewAttachModel(a.client, a.ctx, sessionID, resumeID)
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
	return viewCmd
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
		a.autopilot.width = msg.Width
		a.autopilot.height = msg.Height
		a.login.width = msg.Width

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			a.quitting = true
			return a, tea.Quit
		}

	case switchViewMsg:
		a.activeView = msg.view
		switch msg.view {
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
		case ViewAutopilot:
			a.autopilot = NewAutopilotModel(a.client, a.ctx)
			a.autopilot.width = a.width
			a.autopilot.height = a.height
			return a, a.autopilot.Init()
		case ViewAttach:
			a.attach = NewAttachModel(a.client, a.ctx, msg.sessionID, msg.resumeID)
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
				a.attach = NewAttachModel(a.client, a.ctx, sess.Id, "")
				a.activeView = ViewAttach
				return a, a.attach.Init()
			}
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewChatPicker:
		updated, cmd := a.chatPicker.Update(msg)
		a.chatPicker = updated.(ChatPickerModel)
		if a.chatPicker.Cancelled() {
			a.activeView = ViewHome
			a.home = NewHomeModel(a.client, a.ctx, a.auth)
			a.home.highlightSessionID = a.chatPicker.sessionID
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
	case ViewAutopilot:
		updated, cmd := a.autopilot.Update(msg)
		a.autopilot = updated.(AutopilotModel)
		if a.autopilot.Cancelled() {
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
	}

	return a, nil
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
	case ViewAutopilot:
		v = a.autopilot.View()
	case ViewAttach:
		v = a.attach.View()
	case ViewLogin:
		v = a.login.View()
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
		case ViewAutopilot:
			opts.line1 = "Autopilot"
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
		}
		v.Content = renderBanner(a.activeView, opts) + "\n" + v.Content
	}

	v.AltScreen = true
	return v
}
