package views

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
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

type loginCompleteHook func(context.Context)

var openLoginVerificationURL = auth.OpenBrowser

// LoginModel handles the interactive device code login flow.
type LoginModel struct {
	mgr       *auth.Manager
	client    client.BossClient
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
	afterAuth loginCompleteHook

	cloudAccess       CloudAccessClient
	checkoutReturnURL string
	checkoutCancelURL string
	subscriptionURL   string
	subscription      subscriptionState
}

// NewLoginModel creates a new login model that will start the device code flow.
func NewLoginModel(mgr *auth.Manager, c client.BossClient, parentCtx context.Context) LoginModel {
	ctx, cancel := context.WithCancel(parentCtx)
	return LoginModel{
		mgr:     mgr,
		client:  c,
		ctx:     ctx,
		cancel:  cancel,
		spinner: newStatusSpinner(),
		phase:   loginPhaseRequesting,
	}
}

func (m *LoginModel) SetAfterAuth(hook loginCompleteHook) {
	m.afterAuth = hook
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
		if m.subscription.phase != subscriptionPhaseIdle {
			switch msg.String() {
			case "esc", "ctrl+c":
				if m.cancel != nil {
					m.cancel()
				}
				m.cancelled = true
				m.done = true
				return m, nil
			case "o":
				if m.subscription.checkoutURL != "" {
					return m, m.subscriptionOpenBrowser(m.subscription.checkoutURL, m.subscription.attempt)
				}
				if m.subscription.phase == subscriptionPhaseWaiting ||
					m.subscription.phase == subscriptionPhaseTimedOut ||
					m.subscription.phase == subscriptionPhaseError {
					updated, cmd := m.startSubscriptionAttempt()
					return updated, cmd
				}
			case "enter":
				// The unavailable phase is terminal (e.g. billing is down).
				// Acknowledge and return home; local sessions remain available.
				if m.subscription.phase == subscriptionPhaseUnavailable {
					m.done = true
					return m, nil
				}
				if m.subscription.phase == subscriptionPhaseWaiting && m.subscription.checkoutURL != "" {
					return m, m.subscriptionOpenBrowser(m.subscription.checkoutURL, m.subscription.attempt)
				}
				if m.subscription.phase == subscriptionPhaseWaiting ||
					m.subscription.phase == subscriptionPhaseTimedOut ||
					m.subscription.phase == subscriptionPhaseError {
					updated, cmd := m.startSubscriptionAttempt()
					return updated, cmd
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "esc":
			if m.cancel != nil {
				m.cancel()
			}
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
		_ = openLoginVerificationURL(m.verifyURL)

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
		cmds := []tea.Cmd{func() tea.Msg {
			if m.client != nil {
				_ = m.client.NotifyAuthChange(m.ctx, "login")
			}
			return nil
		}}
		if m.afterAuth != nil {
			cmds = append(cmds, func() tea.Msg {
				m.afterAuth(m.ctx)
				return nil
			})
		}
		if m.cloudAccess != nil {
			updated, cmd := m.startSubscriptionAttempt()
			m = updated
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}
		cmds = append(cmds, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
			return loginAutoReturnMsg{}
		}))
		return m, tea.Batch(cmds...)

	case subscriptionAccessMsg:
		return m.updateSubscriptionAccess(msg)

	case subscriptionCheckoutMsg:
		return m.updateSubscriptionCheckout(msg)

	case subscriptionBrowserOpenedMsg:
		return m.updateSubscriptionBrowserOpened(msg), nil

	case subscriptionPollTickMsg:
		return m.updateSubscriptionPollTick(msg)

	case subscriptionTimeoutMsg:
		if msg.attempt != m.subscription.attempt || m.subscription.phase != subscriptionPhaseWaiting {
			return m, nil
		}
		m.subscription.phase = subscriptionPhaseTimedOut
		m.subscription.err = errors.New("subscription activation is taking longer than expected")
		return m, nil

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
	if m.subscription.phase != subscriptionPhaseIdle {
		return tea.NewView(m.subscriptionView())
	}

	padding := lipgloss.NewStyle().Padding(0, 2)

	var content string
	switch m.phase {
	case loginPhaseRequesting:
		content = padding.Render(m.spinner.View() + "Requesting device code...")

	case loginPhasePolling:
		authCodeLine := lipgloss.NewStyle().Foreground(colorInfo).Render(
			fmt.Sprintf(
				"Your authentication code: %s",
				lipgloss.NewStyle().Bold(true).Foreground(colorInfo).Render(m.userCode),
			),
		)
		content = padding.Render(
			authCodeLine+"\n\n"+
				fmt.Sprintf("Visit: %s\n\n", m.verifyURL)+
				m.spinner.View()+"Waiting for authentication...",
		) + "\n" +
			styleActionBar.Render("[esc] cancel")

	case loginPhaseSuccess:
		label := "Login successful!"
		if m.email != "" {
			label = fmt.Sprintf("Logged in as %s", m.email)
		}
		content = padding.Render(lipgloss.NewStyle().Foreground(colorSuccess).Render(label))

	case loginPhaseError:
		// renderError already applies its own Padding(0, 2) and, given a width,
		// renders a block exactly that wide. Wrapping it in the local padding
		// style would add 2 more columns each side and overflow the terminal by
		// 4, so pass it through bare — the shape every other renderError caller
		// uses, and the one that lines the message up with the action bar's own
		// 2-column indent (BOS-531).
		content = renderError(fmt.Sprintf("Login failed: %v", m.err), m.width) + "\n" +
			styleActionBar.Render("[esc] back")
	}

	return tea.NewView(content)
}
