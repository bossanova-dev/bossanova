package chatpicker

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

type tickMsg struct{}

func tickCmd() tea.Cmd { return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }
