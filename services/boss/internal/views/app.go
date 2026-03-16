// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
)

// View identifies which screen is currently active.
type View int

const (
	ViewHome View = iota
	ViewNewSession
	ViewAttach
	ViewRepoAdd
	ViewRepoList
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client     *client.Client
	ctx        context.Context
	activeView View
	home       HomeModel
	newSession NewSessionModel
	width      int
	height     int
	quitting   bool
}

// NewApp creates a new App wired to the daemon client.
func NewApp(c *client.Client) App {
	ctx := context.Background()
	home := NewHomeModel(c, ctx)
	return App{
		client:     c,
		ctx:        ctx,
		activeView: ViewHome,
		home:       home,
	}
}

// SetInitialView overrides the default initial view before running the program.
func (a *App) SetInitialView(v View) {
	a.activeView = v
	if v == ViewNewSession {
		a.newSession = NewNewSessionModel(a.client, a.ctx)
	}
}

func (a App) Init() tea.Cmd {
	switch a.activeView {
	case ViewNewSession:
		return a.newSession.Init()
	default:
		return a.home.Init()
	}
}

// switchViewMsg requests the app to switch to a different view.
type switchViewMsg struct {
	view View
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.home.width = msg.Width
		a.home.height = msg.Height

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
			return a, a.newSession.Init()
		case ViewHome:
			a.home = NewHomeModel(a.client, a.ctx)
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
		if a.newSession.Cancelled() || a.newSession.Done() {
			a.activeView = ViewHome
			a.home = NewHomeModel(a.client, a.ctx)
			return a, a.home.Init()
		}
		return a, cmd
	}

	return a, nil
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
	default:
		v = tea.NewView("Unknown view")
	}

	v.AltScreen = true
	return v
}
