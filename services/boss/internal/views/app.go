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
	client    *client.Client
	ctx       context.Context
	activeView View
	home      HomeModel
	err       error
	width     int
	height    int
	quitting  bool
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

func (a App) Init() tea.Cmd {
	return a.home.Init()
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
	}

	switch a.activeView {
	case ViewHome:
		updated, cmd := a.home.Update(msg)
		a.home = updated.(HomeModel)
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
	default:
		v = tea.NewView("Unknown view")
	}

	v.AltScreen = true
	return v
}
