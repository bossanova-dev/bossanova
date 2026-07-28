package views

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
)

// openBillingPortalURL opens the Stripe billing portal in the default
// browser. It mirrors openSubscriptionCheckoutURL (subscription.go) so tests
// can stub the browser-open without launching a real browser.
var openBillingPortalURL = auth.OpenBrowser

// billingPortalMsg carries the result of creating a billing portal session
// and attempting to open it. It is emitted asynchronously so the Bubble Tea
// update loop never blocks on the network call.
type billingPortalMsg struct {
	url        string
	err        error
	browserErr error
}

// billingOpeningStatus is the in-flight status line for the billing portal.
// It is a constant so the initial press and the in-flight re-press cannot
// drift apart.
const billingOpeningStatus = "Opening the billing portal..."

// settingsSection is one row of the Settings hub menu: a destination view (or
// the in-place billing action) plus the letter accelerator that reaches it
// without moving the cursor.
type settingsSection struct {
	Key         string // letter accelerator
	Label       string
	Description string
	Target      View // destination view; unused for the billing action
	Billing     bool // true for the in-place billing action
}

// SettingsModel is the Settings hub: a menu of settings sections (BOS-511).
// Every row is a destination the app already routes to, so the letters that
// used to be the *only* way to reach Repos / Cron / Accounts / Trash survive
// as accelerators over a list that also shows them.
type SettingsModel struct {
	ctx context.Context

	cursor int
	cancel bool
	width  int

	// cloudAccess opens the Stripe billing portal. It is nil for local-only
	// daemons / logged-out users, in which case the Billing section is omitted.
	cloudAccess    CloudAccessClient
	cloudReturnURL string
	// billingStatus holds the user-facing outcome of the most recent billing
	// portal action (success, browser-open fallback URL, or unavailable note).
	billingStatus string
	// billingInFlight guards against spawning concurrent billing-portal RPCs
	// when [b] is pressed repeatedly. It is tracked separately from
	// billingStatus because the status string is cleared on each keypress.
	billingInFlight bool

	showCloudSettings bool
}

// NewSettingsModel constructs the Settings hub. The client parameter is kept
// so callers match the other settings-family constructors, but the menu itself
// issues no RPCs — the sections it opens do, and each builds its own model.
func NewSettingsModel(_ client.BossClient, ctx context.Context, authMgr ...*auth.Manager) SettingsModel {
	return SettingsModel{
		ctx:               ctx,
		showCloudSettings: shouldShowCloudSettings(authMgr...),
	}
}

// SetCloudAccess wires the cloud access client and the Stripe return URL used
// by the [b]illing action. Stripe requires a return URL, so callers should
// pass the resolved cloud base URL (it may be empty for clients that don't
// configure one). A nil client hides the billing action entirely.
func (m *SettingsModel) SetCloudAccess(c CloudAccessClient, returnURL string) {
	m.cloudAccess = c
	m.cloudReturnURL = returnURL
}

func shouldShowCloudSettings(authMgr ...*auth.Manager) bool {
	if len(authMgr) == 0 || authMgr[0] == nil {
		return false
	}
	status := authMgr[0].Status()
	return status == nil || !status.LoggedIn
}

// sections returns the menu in display order. Billing is appended only when a
// cloud client is wired: it is omitted entirely rather than rendered disabled,
// matching the pre-BOS-511 action bar, which only advertised [b]illing then.
func (m SettingsModel) sections() []settingsSection {
	out := []settingsSection{
		{
			Key:         "g",
			Label:       "General",
			Description: "Worktree location, polling, notifications, agents, tracing",
			Target:      ViewGeneralSettings,
		},
		{
			Key:         "r",
			Label:       "Repositories",
			Description: "Registered repos and their per-repo options",
			Target:      ViewRepoList,
		},
		{
			Key:         "c",
			Label:       "Cron jobs",
			Description: "Scheduled skill runs",
			Target:      ViewCron,
		},
		{
			Key:         "a",
			Label:       "Accounts",
			Description: "Agent provider accounts and rotation",
			Target:      ViewAccounts,
		},
		{
			Key:         "t",
			Label:       "Trash",
			Description: "Archived sessions",
			Target:      ViewTrash,
		},
	}
	if m.cloudAccess != nil {
		out = append(out, settingsSection{
			Key:         "b",
			Label:       "Billing",
			Description: "Subscription and payment",
			Billing:     true,
		})
	}
	return out
}

// activate is the single activation path for a section, used by both enter on
// the cursor row and the letter accelerator, so a row and its hotkey can never
// diverge.
func (m SettingsModel) activate(s settingsSection) (SettingsModel, tea.Cmd) {
	if s.Billing {
		return m.startBillingPortal()
	}
	if s.Target == ViewGeneralSettings {
		// General's parent is statically the Settings hub, so it carries no
		// returnView slot and app.go ignores the field for this view.
		return m, func() tea.Msg { return switchViewMsg{view: ViewGeneralSettings} }
	}
	target := s.Target
	return m, func() tea.Msg { return switchViewMsg{view: target, returnView: ViewSettings} }
}

func (m SettingsModel) Init() tea.Cmd { return nil }

// Cancelled reports whether the user left the Settings hub.
func (m SettingsModel) Cancelled() bool { return m.cancel }

func (m SettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case billingPortalMsg:
		return m.updateBillingPortal(msg), nil
	case tea.KeyMsg:
		// A new key interaction supersedes any previously-shown billing
		// status, so clear it before processing. Activating Billing sets
		// "Opening the billing portal..." again (including when a request is
		// already in flight, so re-pressing [b] does not blank the only
		// feedback the user has), and the async billingPortalMsg (not a
		// KeyMsg) sets the final status, so both survive until the next
		// keypress.
		m.billingStatus = ""
		sections := m.sections()
		switch key := msg.String(); key {
		case "esc":
			m.cancel = true
			return m, nil
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(+1)
		case "enter":
			if m.cursor >= 0 && m.cursor < len(sections) {
				return m.activate(sections[m.cursor])
			}
		default:
			for i, s := range sections {
				if key == s.Key {
					m.cursor = i
					return m.activate(s)
				}
			}
		}
	}
	return m, nil
}

// moveCursor advances the cursor by delta, clamped to the section list. There
// are no header pseudo-rows in the menu, so every index is selectable.
func (m *SettingsModel) moveCursor(delta int) {
	c := m.cursor + delta
	if c < 0 || c >= len(m.sections()) {
		return
	}
	m.cursor = c
}

// selectedKey returns the key at the current cursor, with General as the safe
// fallback for a stale or otherwise invalid cursor.
func (m SettingsModel) selectedKey() string {
	sections := m.sections()
	if m.cursor >= 0 && m.cursor < len(sections) {
		return sections[m.cursor].Key
	}
	return sections[0].Key
}

// restoreSection moves the cursor to key in the current section list. The list
// can change when cloud access adds Billing, so missing and unknown keys safely
// fall back to General instead of relying on a persisted row index.
func (m *SettingsModel) restoreSection(key string) {
	for i, s := range m.sections() {
		if s.Key == key {
			m.cursor = i
			return
		}
	}
	m.cursor = 0
}

// startBillingPortal kicks off the async billing-portal flow. It is a no-op
// (no command) when no cloud client is wired, so the key stays inert rather
// than dead-ending. The returned command performs the RPC + browser open off
// the update loop and reports back via billingPortalMsg.
func (m SettingsModel) startBillingPortal() (SettingsModel, tea.Cmd) {
	if m.cloudAccess == nil {
		return m, nil
	}
	if m.billingInFlight {
		// The KeyMsg preamble blanked the status, so restore it rather than
		// leaving the user with no feedback until the RPC lands.
		m.billingStatus = billingOpeningStatus
		return m, nil
	}
	m.billingInFlight = true
	m.billingStatus = billingOpeningStatus
	return m, m.billingPortalCmd(m.cloudAccess, m.cloudReturnURL)
}

// billingPortalCmd creates a Stripe billing portal session and opens it in the
// browser, mirroring subscription.go's checkout command structure.
func (m SettingsModel) billingPortalCmd(c CloudAccessClient, returnURL string) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		url, err := c.CreateBillingPortalSession(ctx, returnURL)
		if err != nil {
			return billingPortalMsg{err: err}
		}
		return billingPortalMsg{url: url, browserErr: openBillingPortalURL(url)}
	}
}

// updateBillingPortal records the outcome of the billing-portal flow as a
// user-facing status line. The "local daemon" / unimplemented error becomes a
// clear unavailable note; a browser-open failure surfaces the URL as
// selectable text (mirroring subscription.go's fallback).
func (m SettingsModel) updateBillingPortal(msg billingPortalMsg) SettingsModel {
	m.billingInFlight = false
	switch {
	case msg.err != nil:
		if billingUnavailableLocal(msg.err) {
			m.billingStatus = "Billing isn't available for a local daemon. Log in to Bossanova Cloud to manage billing."
		} else {
			m.billingStatus = "Couldn't open the billing portal: " + cloudAccessErrorSummary(msg.err)
		}
	case msg.browserErr != nil:
		m.billingStatus = "Couldn't open a browser. Open this billing URL: " + msg.url
	default:
		m.billingStatus = "Opened the billing portal in your browser."
	}
	return m
}

// billingUnavailableLocal reports whether err is the local-only daemon error
// returned by LocalClient.CreateBillingPortalSession (a connect Unimplemented
// error: "cloud billing is only available on a local daemon").
func billingUnavailableLocal(err error) bool {
	return connect.CodeOf(err) == connect.CodeUnimplemented
}

func (m SettingsModel) View() tea.View {
	var b strings.Builder

	sections := m.sections()
	for i, s := range sections {
		cursor := "  "
		if i == m.cursor {
			cursor = cursorChevron + " "
		}
		line := cursor + s.Label
		if i == m.cursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2, 0, 4).Render(styleSubtle.Render(s.Description)))
		b.WriteString("\n")
		// Blank line between entries, matching the provider select-list in
		// onboarding.go. Without it the label and description lines all start
		// at the same column and are separated only by the faint style, so
		// "Repositories" reads as a continuation of General's description.
		if i < len(sections)-1 {
			b.WriteString("\n")
		}
	}

	if m.showCloudSettings {
		b.WriteString("\n")
		b.WriteString(cloudSettingsBlock())
		b.WriteString("\n")
	}
	if m.billingStatus != "" {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(m.billingStatus))
		b.WriteString("\n")
	}

	actions := []string{"[enter] open", "[g]eneral", "[r]epos", "[c]ron", "[a]ccounts", "[t]rash"}
	if m.cloudAccess != nil {
		actions = append(actions, "[b]illing")
	}
	b.WriteString(actionBar(actions, []string{"[esc] back"}))

	return tea.NewView(b.String())
}
