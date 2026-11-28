package home

import (
	"errors"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/preflight"
)

// preflightRetryInterval controls how often the daemon-wait screen probes
// for the socket coming back online. Short enough to feel responsive, long
// enough not to thrash launchd's restart backoff if the daemon is crashing.
const preflightRetryInterval = time.Second

// ErrPreflightCancelled is returned by RunDaemonWait when the user dismisses
// the screen before the daemon recovers, or by RunPreflight when the user
// cancels a warning screen with ctrl+c. Callers translate this into a clean
// exit instead of an error message.
var ErrPreflightCancelled = errors.New("preflight cancelled")

// PreflightModel displays a preflight screen when a startup check fails or
// warns. Blocking screens accept q / esc / ctrl+c to quit; warning screens
// accept enter / space / esc / q to continue and ctrl+c to cancel startup.
//
// When retryCheck is non-nil, the screen polls it on a ticker and exits
// successfully (recovered=true) the first time it returns true — the
// caller can then continue startup as if the original check had passed.
type PreflightModel struct {
	issue      preflight.Issue
	Width      int
	retryCheck func() bool
	recovered  bool
	cancelled  bool
}

// NewPreflightModel wraps an Issue in a Bubble Tea model with no auto-retry.
func NewPreflightModel(issue preflight.Issue) PreflightModel {
	return PreflightModel{issue: issue}
}

type preflightTickMsg struct{}
type preflightRecoveredMsg struct{}

func preflightTickCmd() tea.Cmd {
	return tea.Tick(preflightRetryInterval, func(time.Time) tea.Msg {
		return preflightTickMsg{}
	})
}

func (m PreflightModel) Init() tea.Cmd {
	if m.retryCheck != nil {
		return preflightTickCmd()
	}
	return nil
}

func (m PreflightModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
	case tea.KeyMsg:
		if m.issue.Warning {
			switch msg.String() {
			case "ctrl+c":
				m.cancelled = true
				return m, tea.Quit
			case "enter", " ", "space", "q", "esc":
				return m, tea.Quit
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case preflightTickMsg:
		if m.retryCheck != nil && m.retryCheck() {
			return m, func() tea.Msg { return preflightRecoveredMsg{} }
		}
		return m, preflightTickCmd()
	case preflightRecoveredMsg:
		m.recovered = true
		return m, tea.Quit
	}
	return m, nil
}

func (m PreflightModel) View() tea.View {
	titleColor := ui.ColorDanger
	if m.issue.Warning {
		titleColor = ui.ColorWarning
	}
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(titleColor).Padding(0, 2)
	bodyStyle := lipgloss.NewStyle().Padding(0, 2)
	if m.Width > 0 {
		// KNOWN BUG, do NOT copy: .Width() already includes the padding, so
		// subtracting it renders these blocks 4 columns narrower than the
		// terminal. BOS-531 removed the identical mistake from renderError but
		// left this branch alone (no test covers m.width > 0); widen it under
		// its own change.
		titleStyle = titleStyle.Width(m.Width - 4)
		bodyStyle = bodyStyle.Width(m.Width - 4)
	}

	footer := ui.StyleActionBar.Render("Press q to quit.")
	footerGap := "\n"
	if m.retryCheck != nil {
		waiting := lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorInfo).
			Render("Waiting for the daemon… will reconnect automatically.")
		footer = waiting + "\n" + ui.StyleActionBar.Render("[q]uit")
		footerGap = "\n\n"
	} else if m.issue.Warning {
		footer = ui.StyleActionBar.Render("Press enter to continue.")
	}

	content := RenderBanner(ui.ViewHome, BannerOpts{}) + "\n" +
		titleStyle.Render(m.issue.Title) + "\n\n" +
		bodyStyle.Render(m.issue.Detail) + footerGap +
		footer

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// RunPreflight runs a tea.Program that displays the preflight issue and
// blocks until the user dismisses it. Warning issues return
// ErrPreflightCancelled on ctrl+c and nil on any continue key.
func RunPreflight(issue preflight.Issue) error {
	p := tea.NewProgram(NewPreflightModel(issue))
	return preflightResult(p.Run())
}

func preflightResult(final tea.Model, err error) error {
	if err != nil {
		return err
	}
	if pm, ok := final.(PreflightModel); ok && pm.cancelled {
		return ErrPreflightCancelled
	}
	return nil
}

// RunDaemonWait shows the preflight screen and polls check on a ticker
// until either the daemon comes back (returns nil) or the user quits
// (returns ErrPreflightCancelled). Callers should treat the cancellation
// error as a clean exit, not a failure to surface.
func RunDaemonWait(issue preflight.Issue, check func() bool) error {
	m := PreflightModel{issue: issue, retryCheck: check}
	p := tea.NewProgram(m)
	final, err := p.Run()
	if err != nil {
		return err
	}
	if pm, ok := final.(PreflightModel); ok && pm.recovered {
		return nil
	}
	return ErrPreflightCancelled
}

// IsPreflightCancelled reports whether err is the sentinel returned by
// RunDaemonWait when the user dismisses the screen. The cmd layer uses
// it to swallow the error and exit 0 instead of printing a message.
func IsPreflightCancelled(err error) bool {
	return errors.Is(err, ErrPreflightCancelled)
}
