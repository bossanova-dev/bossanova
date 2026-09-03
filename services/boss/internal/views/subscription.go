package views

import (
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	subscriptionPollInterval = 3 * time.Second
	subscriptionWaitTimeout  = 2 * time.Minute
)

var openSubscriptionCheckoutURL = auth.OpenBrowser
var subscriptionPollIntervalOverride time.Duration

func SetSubscriptionPollIntervalOverride(interval time.Duration) {
	subscriptionPollIntervalOverride = interval
}

func subscriptionPollIntervalValue() time.Duration {
	if subscriptionPollIntervalOverride > 0 {
		return subscriptionPollIntervalOverride
	}
	return subscriptionPollInterval
}

type subscriptionPhase int

const (
	subscriptionPhaseIdle subscriptionPhase = iota
	subscriptionPhaseChecking
	subscriptionPhaseCreatingCheckout
	subscriptionPhaseOpeningBrowser
	subscriptionPhaseWaiting
	subscriptionPhaseTimedOut
	subscriptionPhaseSuccess
	subscriptionPhaseUnavailable
	subscriptionPhaseError
)

type subscriptionState struct {
	phase       subscriptionPhase
	attempt     int
	checkoutURL string
	err         error
	active      bool
	// checkoutStarted records that a checkout session has already been created
	// (and the browser opened) for the current attempt. Once set, poll results
	// that still report a non-active billing state keep waiting rather than
	// re-creating checkout or reopening the browser.
	checkoutStarted bool
}

type subscriptionAccessMsg struct {
	status  *pb.CloudAccessStatus
	err     error
	attempt int
}

type subscriptionCheckoutMsg struct {
	url     string
	err     error
	attempt int
}

type subscriptionBrowserOpenedMsg struct {
	url     string
	err     error
	attempt int
}

type subscriptionPollTickMsg struct {
	attempt int
}

type subscriptionTimeoutMsg struct {
	attempt int
}

func (m *LoginModel) SetCloudSubscription(c CloudAccessClient, returnURL, cancelURL string) {
	m.cloudAccess = c
	m.checkoutReturnURL = returnURL
	m.checkoutCancelURL = cancelURL
}

func (m *LoginModel) SetSubscriptionURL(rawURL string) {
	m.subscriptionURL = rawURL
}

func (m LoginModel) startSubscriptionAttempt() (LoginModel, tea.Cmd) {
	if m.cloudAccess == nil {
		m.done = true
		return m, nil
	}
	m.subscription.attempt++
	m.subscription.phase = subscriptionPhaseChecking
	m.subscription.err = nil
	m.subscription.checkoutURL = ""
	m.subscription.checkoutStarted = false
	attempt := m.subscription.attempt
	return m, m.subscriptionCheck(m.cloudAccess, attempt)
}

func (m LoginModel) subscriptionCheck(c CloudAccessClient, attempt int) tea.Cmd {
	return func() tea.Msg {
		status, err := c.GetCloudAccessStatus(m.ctx)
		return subscriptionAccessMsg{status: status, err: err, attempt: attempt}
	}
}

func (m LoginModel) subscriptionCreateCheckout(c CloudAccessClient, attempt int) tea.Cmd {
	return func() tea.Msg {
		if m.subscriptionURL != "" {
			return subscriptionCheckoutMsg{url: m.subscriptionURL, attempt: attempt}
		}
		url, err := c.CreateCheckoutSession(m.ctx, m.checkoutReturnURL, m.checkoutCancelURL)
		return subscriptionCheckoutMsg{url: url, err: err, attempt: attempt}
	}
}

func (m LoginModel) subscriptionOpenBrowser(rawURL string, attempt int) tea.Cmd {
	return func() tea.Msg {
		err := openSubscriptionCheckoutURL(rawURL)
		return subscriptionBrowserOpenedMsg{url: rawURL, err: err, attempt: attempt}
	}
}

func (m LoginModel) subscriptionPoll(c CloudAccessClient, attempt int) tea.Cmd {
	return func() tea.Msg {
		status, err := c.RefreshCloudEntitlements(m.ctx)
		return subscriptionAccessMsg{status: status, err: err, attempt: attempt}
	}
}

func (m LoginModel) subscriptionPollTick(attempt int) tea.Cmd {
	return tea.Tick(subscriptionPollIntervalValue(), func(time.Time) tea.Msg {
		return subscriptionPollTickMsg{attempt: attempt}
	})
}

func (m LoginModel) subscriptionTimeout(attempt int, _ time.Duration) func(time.Time) tea.Msg {
	return func(time.Time) tea.Msg {
		return subscriptionTimeoutMsg{attempt: attempt}
	}
}

func (m LoginModel) subscriptionTimeoutCmd(attempt int) tea.Cmd {
	return tea.Tick(subscriptionWaitTimeout, m.subscriptionTimeout(attempt, subscriptionWaitTimeout))
}

func subscriptionNeedsCheckout(status *pb.CloudAccessStatus) bool {
	switch status.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		return true
	default:
		return false
	}
}

func subscriptionIsActive(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE
}

func subscriptionCanPoll(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH
}

func subscriptionTerminalLocal(status *pb.CloudAccessStatus, err error) bool {
	if err != nil {
		return true
	}
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE
}

// subscriptionUnavailableError builds the user-facing message rendered in the
// subscription flow's unavailable phase. It preserves the original casing of the
// underlying detail (so redacted identifiers stay readable) and always returns a
// non-nil error so the view has something to render.
func subscriptionUnavailableError(status *pb.CloudAccessStatus, err error) error {
	if err != nil {
		return errors.New(cloudAccessUnavailableLine(err))
	}
	// These are terminal, user-facing display sentences (never wrapped into
	// another error), so ST1005's no-trailing-punctuation rule does not apply.
	if status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		return errors.New(cloudBillingUnavailableLine) //nolint:staticcheck // user-facing copy, see above
	}
	return errors.New(cloudAccessUnavailableFallbackLine) //nolint:staticcheck // user-facing copy, see above
}

func (m LoginModel) updateSubscriptionAccess(msg subscriptionAccessMsg) (LoginModel, tea.Cmd) {
	if msg.attempt != m.subscription.attempt {
		return m, nil
	}
	alreadyWaiting := m.subscription.phase == subscriptionPhaseWaiting
	if msg.err != nil && (alreadyWaiting || m.subscription.checkoutStarted) {
		m.subscription.phase = subscriptionPhaseWaiting
		return m, m.subscriptionWaitCmds(msg.attempt, !alreadyWaiting)
	}
	if subscriptionTerminalLocal(msg.status, msg.err) {
		// Surface the error in the unavailable phase and wait for the user to
		// acknowledge it. Returning home immediately (m.done) would discard this
		// LoginModel before subscriptionView could render the message.
		m.subscription.phase = subscriptionPhaseUnavailable
		m.subscription.err = subscriptionUnavailableError(msg.status, msg.err)
		return m, nil
	}
	if subscriptionIsActive(msg.status) {
		m.subscription.phase = subscriptionPhaseSuccess
		m.subscription.active = true
		return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
			return loginAutoReturnMsg{}
		})
	}
	// We arm the wait timeout only on the first transition into the waiting
	// phase for an attempt; subsequent poll results re-arm only the poll tick so
	// the 2-minute timeout can actually fire instead of being reset every cycle.
	if msg.status.GetCanCreateCheckout() && !m.subscription.checkoutStarted {
		m.subscription.phase = subscriptionPhaseCreatingCheckout
		m.subscription.checkoutStarted = true
		return m, m.subscriptionCreateCheckout(m.cloudAccess, msg.attempt)
	}
	if msg.status.GetCheckoutStarted() || subscriptionCanPoll(msg.status) {
		m.subscription.phase = subscriptionPhaseWaiting
		m.subscription.checkoutStarted = true
		return m, m.subscriptionWaitCmds(msg.attempt, !alreadyWaiting)
	}
	// Once checkout has been created for this attempt, keep waiting for the
	// webhook to activate the subscription. Never re-create checkout or reopen
	// the browser from a poll result, even if the state still reads as
	// needs-subscription while the user completes payment.
	if m.subscription.checkoutStarted {
		m.subscription.phase = subscriptionPhaseWaiting
		return m, m.subscriptionWaitCmds(msg.attempt, !alreadyWaiting)
	}
	if subscriptionNeedsCheckout(msg.status) {
		m.subscription.phase = subscriptionPhaseCreatingCheckout
		m.subscription.checkoutStarted = true
		return m, m.subscriptionCreateCheckout(m.cloudAccess, msg.attempt)
	}
	// Unexpected, unrecoverable state: surface it and wait for the user to
	// acknowledge rather than silently returning home.
	m.subscription.phase = subscriptionPhaseUnavailable
	m.subscription.err = subscriptionUnavailableError(msg.status, msg.err)
	return m, nil
}

// subscriptionWaitCmds arms the next poll tick, plus the wait timeout when this
// is the first entry into the waiting phase for the attempt.
func (m LoginModel) subscriptionWaitCmds(attempt int, armTimeout bool) tea.Cmd {
	if armTimeout {
		return tea.Batch(m.subscriptionPollTick(attempt), m.subscriptionTimeoutCmd(attempt))
	}
	return m.subscriptionPollTick(attempt)
}

func (m LoginModel) updateSubscriptionCheckout(msg subscriptionCheckoutMsg) (LoginModel, tea.Cmd) {
	if msg.attempt != m.subscription.attempt {
		return m, nil
	}
	if msg.err != nil {
		if checkoutRefusedByAccountState(msg.err) {
			m.subscription.phase = subscriptionPhaseWaiting
			m.subscription.err = nil
			m.subscription.checkoutStarted = true
			return m, m.subscriptionWaitCmds(msg.attempt, true)
		}
		m.subscription.phase = subscriptionPhaseError
		m.subscription.err = fmt.Errorf("cloud checkout unavailable; local sessions are still available: %w", msg.err)
		return m, nil
	}
	m.subscription.phase = subscriptionPhaseWaiting
	m.subscription.checkoutURL = msg.url
	m.subscription.checkoutStarted = true
	return m, tea.Batch(
		m.subscriptionOpenBrowser(msg.url, msg.attempt),
		m.subscriptionPollTick(msg.attempt),
		m.subscriptionTimeoutCmd(msg.attempt),
	)
}

func (m LoginModel) updateSubscriptionBrowserOpened(msg subscriptionBrowserOpenedMsg) LoginModel {
	if msg.attempt != m.subscription.attempt {
		return m
	}
	// Polling was already armed when the checkout was created; here we only
	// record the browser-open outcome so the view can surface the URL fallback
	// if the browser failed to open.
	m.subscription.phase = subscriptionPhaseWaiting
	m.subscription.checkoutURL = msg.url
	m.subscription.err = msg.err
	return m
}

func (m LoginModel) updateSubscriptionPollTick(msg subscriptionPollTickMsg) (LoginModel, tea.Cmd) {
	if msg.attempt != m.subscription.attempt || m.subscription.phase != subscriptionPhaseWaiting {
		return m, nil
	}
	return m, m.subscriptionPoll(m.cloudAccess, msg.attempt)
}

func (m LoginModel) subscriptionView() string {
	padding := lipgloss.NewStyle().Padding(0, 2)

	var body string
	switch m.subscription.phase {
	case subscriptionPhaseSuccess:
		body = "Bossanova Cloud is ready. Returning home…"
	case subscriptionPhaseTimedOut:
		body = "Subscription activation is taking longer than expected.\nPress enter to check again or reopen checkout."
	case subscriptionPhaseError:
		body = "Cloud checkout unavailable. Local sessions are still available.\nPress enter to retry."
	case subscriptionPhaseUnavailable:
		if m.subscription.err != nil {
			body = m.subscription.err.Error()
		} else {
			body = cloudAccessUnavailableFallbackLine
		}
	default:
		if m.subscription.phase == subscriptionPhaseWaiting && m.subscription.checkoutStarted && m.subscription.checkoutURL == "" {
			body = m.spinner.View() + "Activating your subscription. This can take a few minutes…"
		} else {
			body = m.spinner.View() + "Loading your account…"
		}
		if m.subscription.err != nil && m.subscription.checkoutURL != "" {
			body += "\nOpen this billing URL: " + m.subscription.checkoutURL
		}
	}

	actionBar := "[enter] retry  [esc] cancel"
	if m.subscription.phase == subscriptionPhaseUnavailable {
		actionBar = "[enter] continue  [esc] cancel"
	} else if m.subscription.phase == subscriptionPhaseWaiting && m.subscription.checkoutURL != "" {
		actionBar = "[enter] re-open subscription page  [esc] cancel"
	} else if m.subscription.phase == subscriptionPhaseWaiting && m.subscription.checkoutStarted {
		actionBar = "[enter] check again  [esc] cancel"
	}
	content := padding.Render(body) + "\n" +
		styleActionBar.Render(actionBar)
	return content
}
