package views

import (
	"context"
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
)

// loginPhase tracks the current state of the login flow.
type loginPhase int

const (
	loginPhaseRequesting loginPhase = iota
	loginPhasePolling
	loginPhaseSuccess
	loginPhaseError
)

// deviceCodeMsg carries the result of requesting a device code.
type deviceCodeMsg struct {
	resp *auth.DeviceCodeResponse
	err  error
}

// loginCompleteMsg signals that login polling completed successfully.
type loginCompleteMsg struct {
	email string
}

// loginErrorMsg signals that login polling failed.
type loginErrorMsg struct {
	err error
}

// loginAutoReturnMsg signals the success screen should auto-return to home.
type loginAutoReturnMsg struct{}

// LoginModel handles the interactive device code login flow.
type LoginModel struct {
	mgr       *auth.Manager
	ctx       context.Context
	cancel    context.CancelFunc
	spinner   spinner.Model
	phase     loginPhase
	userCode  string
	verifyURL string
	email     string
	err       error
	cancelled bool
	done      bool
	width     int
}

// NewLoginModel creates a new login model that will start the device code flow.
func NewLoginModel(mgr *auth.Manager, parentCtx context.Context) LoginModel {
	ctx, cancel := context.WithCancel(parentCtx)
	return LoginModel{
		mgr:     mgr,
		ctx:     ctx,
		cancel:  cancel,
		spinner: newStatusSpinner(),
		phase:   loginPhaseRequesting,
	}
}

// Cancelled returns true if the user cancelled the login flow.
func (m LoginModel) Cancelled() bool { return m.cancelled }

// Done returns true if the login flow completed (success or dismissed error).
func (m LoginModel) Done() bool { return m.done }

func (m LoginModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.requestDeviceCode())
}

func (m LoginModel) requestDeviceCode() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.mgr.StartLogin(m.ctx)
		return deviceCodeMsg{resp: resp, err: err}
	}
}

func (m LoginModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.cancel()
			m.cancelled = true
			return m, nil
		}
		// Any key on success or error returns to home.
		if m.phase == loginPhaseSuccess || m.phase == loginPhaseError {
			m.done = true
			return m, nil
		}

	case deviceCodeMsg:
		if msg.err != nil {
			m.phase = loginPhaseError
			m.err = msg.err
			return m, nil
		}
		m.phase = loginPhasePolling
		m.userCode = msg.resp.UserCode
		m.verifyURL = msg.resp.VerificationURIComplete
		if m.verifyURL == "" {
			m.verifyURL = msg.resp.VerificationURI
		}

		// Open browser (best-effort).
		_ = auth.OpenBrowser(m.verifyURL)

		// Start polling with a timeout based on ExpiresIn.
		pollCtx, pollCancel := context.WithTimeout(m.ctx, time.Duration(msg.resp.ExpiresIn)*time.Second)
		deviceCode := msg.resp.DeviceCode
		interval := msg.resp.Interval
		return m, func() tea.Msg {
			defer pollCancel()
			err := m.mgr.PollLogin(pollCtx, deviceCode, interval)
			if err != nil {
				return loginErrorMsg{err: err}
			}
			status := m.mgr.Status()
			return loginCompleteMsg{email: status.Email}
		}

	case loginCompleteMsg:
		m.phase = loginPhaseSuccess
		m.email = msg.email
		return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
			return loginAutoReturnMsg{}
		})

	case loginErrorMsg:
		// If context was cancelled (user pressed esc), treat as cancellation.
		if m.ctx.Err() != nil {
			m.cancelled = true
			return m, nil
		}
		m.phase = loginPhaseError
		m.err = msg.err
		return m, nil

	case loginAutoReturnMsg:
		m.done = true
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m LoginModel) View() tea.View {
	padding := lipgloss.NewStyle().Padding(0, 2)

	var content string
	switch m.phase {
	case loginPhaseRequesting:
		content = padding.Render(m.spinner.View() + " Requesting device code...")

	case loginPhasePolling:
		content = padding.Render(
			fmt.Sprintf("Your authentication code: %s\n\n", styleSelected.Render(m.userCode))+
				fmt.Sprintf("Visit: %s\n\n", m.verifyURL)+
				m.spinner.View()+" Waiting for authentication...",
		) + "\n" +
			styleActionBar.Render("[esc] cancel")

	case loginPhaseSuccess:
		label := "Login successful!"
		if m.email != "" {
			label = fmt.Sprintf("Logged in as %s", m.email)
		}
		content = padding.Render(lipgloss.NewStyle().Foreground(colorSuccess).Render(label))

	case loginPhaseError:
		content = padding.Render(renderError(fmt.Sprintf("Login failed: %v", m.err), m.width)) + "\n" +
			styleActionBar.Render("[esc] back")
	}

	return tea.NewView(content)
}
