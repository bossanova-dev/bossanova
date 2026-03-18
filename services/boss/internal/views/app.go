// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
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
	ViewTrash
	ViewSettings
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client     client.BossClient
	ctx        context.Context
	manager    *bosspty.Manager
	activeView View
	home       HomeModel
	newSession NewSessionModel
	chatPicker ChatPickerModel
	repoAdd    RepoAddModel
	repoList   RepoListModel
	trash      TrashModel
	settings   SettingsModel
	attach     AttachModel
	width      int
	height     int
	quitting   bool
}

// NewApp creates a new App wired to the daemon client.
func NewApp(c client.BossClient) App {
	ctx := context.Background()
	mgr := bosspty.NewManager()
	home := NewHomeModel(c, ctx, mgr)
	return App{
		client:     c,
		ctx:        ctx,
		manager:    mgr,
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
	a.attach = NewAttachModel(a.client, a.ctx, a.manager, sessionID, resumeID)
}

func (a App) Init() tea.Cmd {
	switch a.activeView {
	case ViewNewSession:
		return a.newSession.Init()
	case ViewChatPicker:
		return a.chatPicker.Init()
	case ViewRepoAdd:
		return a.repoAdd.Init()
	case ViewRepoList:
		return a.repoList.Init()
	case ViewAttach:
		return a.attach.Init()
	default:
		return a.home.Init()
	}
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
		a.trash.width = msg.Width
		a.settings.width = msg.Width

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
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, a.manager, msg.sessionID, "")
			return a, a.chatPicker.Init()
		case ViewRepoAdd:
			a.repoAdd = NewRepoAddModel(a.client, a.ctx)
			a.repoAdd.width = a.width
			return a, a.repoAdd.Init()
		case ViewRepoList:
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.width = a.width
			return a, a.repoList.Init()
		case ViewTrash:
			a.trash = NewTrashModel(a.client, a.ctx)
			a.trash.width = a.width
			return a, a.trash.Init()
		case ViewSettings:
			a.settings = NewSettingsModel()
			a.settings.width = a.width
			return a, a.settings.Init()
		case ViewAttach:
			a.attach = NewAttachModel(a.client, a.ctx, a.manager, msg.sessionID, msg.resumeID)
			return a, a.attach.Init()
		case ViewHome:
			a.home = NewHomeModel(a.client, a.ctx, a.manager)
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
				a.attach = NewAttachModel(a.client, a.ctx, a.manager, sess.Id, "")
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
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewRepoAdd:
		updated, cmd := a.repoAdd.Update(msg)
		a.repoAdd = updated.(RepoAddModel)
		if a.repoAdd.Cancelled() {
			return a, a.switchToHome()
		}
		if a.repoAdd.Done() {
			a.repoList = NewRepoListModel(a.client, a.ctx)
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
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, a.manager, sessionID, claudeID)
			a.activeView = ViewChatPicker
			// Batch the attach cleanup cmd (e.g. orphan delete) with the chat picker init.
			return a, tea.Batch(cmd, a.chatPicker.Init())
		}
		return a, cmd
	}

	return a, nil
}

func (a *App) switchToHome() tea.Cmd {
	a.activeView = ViewHome
	a.home = NewHomeModel(a.client, a.ctx, a.manager)
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
	case ViewTrash:
		v = a.trash.View()
	case ViewSettings:
		v = a.settings.View()
	case ViewAttach:
		v = a.attach.View()
	default:
		v = tea.NewView("Unknown view")
	}

	v.AltScreen = true
	return v
}
