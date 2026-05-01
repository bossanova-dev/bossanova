package views

import (
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/recurser/boss/internal/preflight"
)

// preflightRetryInterval controls how often the daemon-wait screen probes
// for the socket coming back online. Short enough to feel responsive, long
// enough not to thrash launchd's restart backoff if the daemon is crashing.
const preflightRetryInterval = time.Second

// errPreflightCancelled is returned by RunDaemonWait when the user dismisses
// the screen before the daemon recovers. Callers translate this into a
// clean exit instead of an error message.
var errPreflightCancelled = errors.New("preflight cancelled")

// PreflightModel is a blocking screen shown when a startup check fails
// (missing required software, daemon unreachable). It accepts q / esc /
// ctrl+c to quit so the user is never stranded.
//
// When retryCheck is non-nil, the screen polls it on a ticker and exits
// successfully (recovered=true) the first time it returns true — the
// caller can then continue startup as if the original check had passed.
type PreflightModel struct {
	issue      preflight.Issue
	width      int
	retryCheck func() bool
	recovered  bool
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
		m.width = msg.Width
	case tea.KeyMsg:
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
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorDanger).Padding(0, 2)
	bodyStyle := lipgloss.NewStyle().Padding(0, 2)
	if m.width > 0 {
		titleStyle = titleStyle.Width(m.width - 4)
		bodyStyle = bodyStyle.Width(m.width - 4)
	}

	footer := styleActionBar.Render("Press q to quit.")
	if m.retryCheck != nil {
		waiting := lipgloss.NewStyle().Padding(0, 2).Foreground(colorInfo).
			Render("Waiting for the daemon... will reconnect automatically.")
		footer = waiting + "\n" + styleActionBar.Render("[q]uit")
	}

	content := renderBanner(ViewHome, bannerOpts{}) + "\n" +
		titleStyle.Render(m.issue.Title) + "\n\n" +
		bodyStyle.Render(m.issue.Detail) + "\n" +
		footer

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// RunPreflight runs a tea.Program that displays the preflight issue and
// blocks until the user quits. Use this for unrecoverable failures (e.g.
// missing software) where retrying in-place wouldn't help.
func RunPreflight(issue preflight.Issue) error {
	p := tea.NewProgram(NewPreflightModel(issue))
	_, err := p.Run()
	return err
}

// RunDaemonWait shows the preflight screen and polls check on a ticker
// until either the daemon comes back (returns nil) or the user quits
// (returns errPreflightCancelled). Callers should treat the cancellation
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
	return errPreflightCancelled
}

// IsPreflightCancelled reports whether err is the sentinel returned by
// RunDaemonWait when the user dismisses the screen. The cmd layer uses
// it to swallow the error and exit 0 instead of printing a message.
func IsPreflightCancelled(err error) bool {
	return errors.Is(err, errPreflightCancelled)
}
