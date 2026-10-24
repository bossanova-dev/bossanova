package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/newsession"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func oneRepo() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main"},
	}
}

// keyPress creates a KeyPressMsg for a printable rune (e.g. "j", "q").
func keyPress(ch rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ch, Text: string(ch)}
}

// specialKeyPress creates a KeyPressMsg for a special key (e.g. tea.KeyEnter).
func specialKeyPress(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

// readyNewSession reaches the type picker through the wizard's actual fetch commands.
func readyNewSession(t *testing.T) newsession.Model {
	t.Helper()
	m := newsession.NewModel(&stubClient{repos: oneRepo()}, context.Background())
	batch := m.Init()().(tea.BatchMsg)
	for _, cmd := range batch[:2] {
		next, _ := m.Update(cmd())
		m = next.(newsession.Model)
	}
	return m
}
func readyNewSessionForm(t *testing.T) newsession.Model {
	t.Helper()
	m := readyNewSession(t)
	next, _ := m.Update(specialKeyPress(tea.KeyEnter))
	return next.(newsession.Model)
}
