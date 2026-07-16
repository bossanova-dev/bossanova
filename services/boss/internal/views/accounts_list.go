package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// emptyAccountsMessage is the exact empty-state sentence for the accounts list.
// Kept as a constant so a test can assert the wording verbatim (BOS-265).
const emptyAccountsMessage = "No managed accounts yet. Add one to let Bossanova rotate credentials automatically."

// --- Messages ---

// accountsLoadedMsg carries the result of ListAccounts.
type accountsLoadedMsg struct {
	accounts []*pb.Account
	err      error
}

// accountTestedMsg carries the result of a live TestAccount RPC.
type accountTestedMsg struct {
	id     string
	detail string
	err    error
}

// --- Accounts List Model ---

// AccountsListModel lists the managed provider accounts used by credential
// rotation (BOS-157). It is the entry point of the TUI account-management epic
// (BOS-265): read-only list + navigation, with row actions owned by sibling
// tickets degrading to a "coming soon" status line. `[t]est` is wired live
// against the existing TestAccount RPC.
//
// Structurally it mirrors CronListModel (a daemon-backed table.Model view with
// a returnView, loading/empty/error states, and a confirm prompt) minus the
// poll loop: accounts change only via explicit user action, so it fetches once
// in Init and re-fetches after a live test completes.
type AccountsListModel struct {
	client client.BossClient
	ctx    context.Context

	accounts []*pb.Account

	table  table.Model
	err    error
	cancel bool

	returnView View

	loading bool
	spinner spinner.Model

	// refreshing is set while a manual usage refresh (ListAccounts with
	// refresh=true, which re-probes every account's utilization on the daemon)
	// is in flight. Set on 'r' press, cleared on the resulting accountsLoadedMsg.
	refreshing bool

	// testing tracks account IDs whose TestAccount RPC is in flight so the
	// row can show a spinner. Set on 't' press, cleared on accountTestedMsg.
	testing map[string]bool

	// disabling / removing track account IDs whose disable-or-enable
	// (UpdateAccount) / remove (RemoveAccount) RPC is in flight so the row shows
	// a per-row pending status (BOS-268). Mirrors cron_list's deleting map.
	disabling map[string]bool
	removing  map[string]bool

	// confirm gates the destructive/semi-destructive disable+remove prompts
	// through the shared value-safe confirmPrompt (BOS-268), mirroring
	// cron_list.go. Polling is not used here, but the confirm still suppresses
	// normal key handling while active.
	confirm confirmPrompt
	// confirmAccountID / confirmRemoving record which account and operation the
	// active confirm targets so the per-row pending flip (disabling vs removing)
	// can be set on the confirm keystroke — the confirmPrompt itself holds no
	// model state (mirrors cron_list re-deriving the id on confirm).
	confirmAccountID string
	confirmRemoving  bool

	// sessions is a best-effort snapshot loaded once in Init (batched alongside
	// fetchAccounts) so disable/remove prompts can state how many live chats are
	// bound to the target account. sessionsKnown gates the count: when false
	// (never loaded or errored) the prompt copy degrades to a generic warning
	// rather than claiming zero.
	sessions      []*pb.Session
	sessionsKnown bool

	// transient status toast
	status    string
	statusErr bool
	statusAt  time.Time

	width  int
	height int
}

// NewAccountsListModel constructs an AccountsListModel wired to the daemon
// client. It starts in the loading state; Init fetches the account list.
func NewAccountsListModel(c client.BossClient, ctx context.Context) AccountsListModel {
	return AccountsListModel{
		client:    c,
		ctx:       ctx,
		testing:   make(map[string]bool),
		disabling: make(map[string]bool),
		removing:  make(map[string]bool),
		spinner:   newStatusSpinner(),
		table:     newBossTable(nil, nil, 0),
		loading:   true,
	}
}

// Cancelled reports whether the user dismissed the accounts view.
func (m AccountsListModel) Cancelled() bool { return m.cancel }

func (m AccountsListModel) Init() tea.Cmd {
	// Batch a one-shot ListSessions alongside the account fetch so the
	// disable/remove prompts (BOS-268) can state how many live chats are bound
	// to the target account. The session snapshot is advisory; a failed fetch
	// degrades the prompt to a generic warning (see sessionsLoadedMsg).
	return tea.Batch(m.fetchAccounts(), m.fetchSessions(), m.spinner.Tick)
}

// --- Commands ---

func (m AccountsListModel) fetchAccounts() tea.Cmd {
	return func() tea.Msg {
		accounts, err := m.client.ListAccounts(m.ctx, "", false)
		return accountsLoadedMsg{accounts: accounts, err: err}
	}
}

// fetchSessions loads the daemon's current sessions once so bound-session
// counts can be computed client-side (no dedicated RPC exists). Errors are
// non-fatal: sessionsKnown stays false and prompt copy degrades to generic.
func (m AccountsListModel) fetchSessions() tea.Cmd {
	return func() tea.Msg {
		sessions, err := m.client.ListSessions(m.ctx, &pb.ListSessionsRequest{})
		return sessionsLoadedMsg{sessions: sessions, err: err}
	}
}

// refreshAccountsCmd re-lists with refresh=true so the daemon re-probes every
// account's usage snapshot before returning, then reloads via accountsLoadedMsg.
func (m AccountsListModel) refreshAccountsCmd() tea.Cmd {
	return func() tea.Msg {
		accounts, err := m.client.ListAccounts(m.ctx, "", true)
		return accountsLoadedMsg{accounts: accounts, err: err}
	}
}

func (m AccountsListModel) testAccountCmd(id string) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.TestAccount(m.ctx, id)
		if err != nil {
			return accountTestedMsg{id: id, err: err}
		}
		return accountTestedMsg{id: id, detail: resp.GetDetail()}
	}
}

// --- Update ---

func (m AccountsListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case accountsLoadedMsg:
		wasRefreshing := m.refreshing
		m.loading = false
		m.refreshing = false
		if msg.err != nil {
			m.err = msg.err
			if wasRefreshing {
				m.setStatus(fmt.Sprintf("Usage refresh failed: %v", msg.err), true)
			}
		} else {
			m.err = nil
			m.accounts = msg.accounts
			m.rebuildTable()
			if wasRefreshing {
				m.setStatus("Usage refreshed.", false)
			}
		}
		return m, nil

	case accountTestedMsg:
		delete(m.testing, msg.id)
		// CREDENTIAL SAFETY: on a failed live smoke test the daemon returns
		// Detail byte-identical to last_test_error (recordAndRespond →
		// err.Error()), which can carry secret-shaped material. Both the Detail
		// and the RPC-error text pass through maskTestError before the transient
		// status line, mirroring the account-detail screen — so maskTestError is
		// genuinely the ONLY path a last-test error reaches the screen.
		if msg.err != nil {
			m.setStatus("Test failed: "+maskTestError(fmt.Sprintf("%v", msg.err)), true)
		} else {
			detail := maskTestError(msg.detail)
			if detail == "" {
				detail = "Test complete."
			}
			m.setStatus(detail, false)
		}
		m.rebuildTable()
		// Re-fetch so health / last-test cells reflect the test outcome.
		return m, m.fetchAccounts()

	case sessionsLoadedMsg:
		// Advisory bound-session snapshot for the disable/remove prompts. On
		// error, leave sessionsKnown false so prompt copy stays generic.
		if msg.err != nil {
			m.sessionsKnown = false
			m.sessions = nil
		} else {
			m.sessions = msg.sessions
			m.sessionsKnown = true
		}
		return m, nil

	case accountStatusUpdatedMsg:
		// Clear the in-flight disable/enable pending and reconcile from a fresh
		// ListAccounts so the STATUS cell reflects the persisted status.
		delete(m.disabling, msg.id)
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Status update failed: %v", msg.err), true)
			m.rebuildTable()
			return m, nil
		}
		if msg.status == accountStatusDisabled {
			m.setStatus("Account disabled.", false)
		} else {
			m.setStatus("Account enabled.", false)
		}
		m.rebuildTable()
		return m, m.fetchAccounts()

	case accountRemovedMsg:
		delete(m.removing, msg.id)
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Remove failed: %v", msg.err), true)
			m.rebuildTable()
			return m, nil
		}
		m.setStatus("Account removed.", false)
		m.rebuildTable()
		return m, m.fetchAccounts()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Keep animating while the initial load, a manual refresh, or any
		// per-row test / disable / remove is active.
		if m.loading || m.refreshing || len(m.testing) > 0 || len(m.disabling) > 0 || len(m.removing) > 0 {
			m.rebuildTable()
		}
		return m, cmd

	case tea.KeyMsg:
		if m.confirm.active {
			return m.updateConfirm(msg)
		}
		return m.updateNormal(msg)
	}

	// Forward non-key messages (mouse, resize, focus, …) to the table.
	updated, cmd := m.table.Update(msg)
	m.table = updated
	updateCursorColumn(&m.table)
	return m, cmd
}

func (m AccountsListModel) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "e", "enter":
		// Open the per-account inline-edit form (BOS-266), handing off the
		// selected account and asking it to return here on esc. e and enter are
		// combined into a single edit action (mirroring the cron list), so there
		// is no separate read-only detail screen.
		acct := m.selectedAccount()
		if acct == nil {
			return m, nil
		}
		return m, func() tea.Msg {
			return switchViewMsg{
				view:       ViewAccountEdit,
				account:    acct,
				returnView: ViewAccounts,
			}
		}

	case "a":
		// Launch the native add-account flow (BOS-267): a provider chooser
		// (claude | codex) then the reused accountflow walkthrough. On completion
		// the app returns to a refreshed accounts list so the new account appears.
		return m, func() tea.Msg {
			return switchViewMsg{
				view:       ViewAccountRegister,
				returnView: ViewAccounts,
			}
		}

	case "t":
		// Wired live: the TestAccount RPC and its result data already exist,
		// so this action is demonstrably functional (BOS-265 default).
		acct := m.selectedAccount()
		if acct == nil {
			return m, nil
		}
		id := acct.GetId()
		m.testing[id] = true
		m.rebuildTable()
		return m, m.testAccountCmd(id)

	case "space":
		// [space] toggle disable/enable (BOS-392 unifies this with the cron
		// list's [space] toggle; behavior unchanged since BOS-268). Enabling
		// (active-ward) is a bare metadata flip with no warning. Disabling an
		// account bound to live sessions (or when the bound count is unknown)
		// goes through a confirm prompt; a known-unbound disable flips
		// immediately.
		acct := m.selectedAccount()
		if acct == nil {
			return m, nil
		}
		id := acct.GetId()
		// Ignore the key while a status flip or remove for this row is already
		// in flight, so a rapid second press can't dispatch a duplicate RPC
		// (the row is only refetched after the RPC returns). Mirrors the
		// m.refreshing guard on [r].
		if m.disabling[id] || m.removing[id] {
			return m, nil
		}
		if accountStatusLabel(acct) == accountStatusDisabled {
			m.disabling[id] = true
			m.setStatus("Enabling…", false)
			m.rebuildTable()
			return m, updateAccountStatusCmd(m.client, m.ctx, id, accountStatusActive)
		}
		count, known := boundSessionCount(m.sessions, m.sessionsKnown, id)
		if known && count == 0 {
			m.disabling[id] = true
			m.setStatus("Disabling…", false)
			m.rebuildTable()
			return m, updateAccountStatusCmd(m.client, m.ctx, id, accountStatusDisabled)
		}
		m.confirmAccountID = id
		m.confirmRemoving = false
		m.confirm = newConfirmPrompt(
			accountDisablePromptText(acct, count, known),
			updateAccountStatusCmd(m.client, m.ctx, id, accountStatusDisabled),
		)
		return m, nil

	case "d":
		// Remove account (BOS-268) — always behind a confirm because removal
		// purges the keyring credential and is irreversible. The prompt copy
		// escalates and recommends disable over remove when the account is bound
		// to live sessions.
		acct := m.selectedAccount()
		if acct == nil {
			return m, nil
		}
		id := acct.GetId()
		// Ignore the key while a status flip or remove for this row is already
		// in flight (see [space]); prevents a second confirm firing a duplicate
		// RemoveAccount and the misleading post-removal error toast.
		if m.disabling[id] || m.removing[id] {
			return m, nil
		}
		count, known := boundSessionCount(m.sessions, m.sessionsKnown, id)
		m.confirmAccountID = id
		m.confirmRemoving = true
		m.confirm = newConfirmPrompt(
			accountRemovePromptText(acct, count, known),
			removeAccountCmd(m.client, m.ctx, id),
		)
		return m, nil

	case "r":
		// Manually refresh utilization: re-list with refresh=true so the daemon
		// re-probes every account's usage snapshot (distinct from [t]est, which
		// only runs a per-account credential smoke test). No-op while a refresh
		// is already in flight.
		if m.refreshing {
			return m, nil
		}
		m.refreshing = true
		m.setStatus("Refreshing usage…", false)
		m.rebuildTable()
		// The spinner is already ticking (started in Init and self-perpetuating
		// via the TickMsg handler), so only the refresh RPC needs to fire here.
		return m, m.refreshAccountsCmd()

	case "esc":
		m.cancel = true
		return m, nil
	}

	// Forward to the table for navigation.
	updated, cmd := m.table.Update(msg)
	m.table = updated
	updateCursorColumn(&m.table)
	return m, cmd
}

// updateConfirm routes a key to the active disable/remove confirm prompt. On
// confirm it flips the matching per-row pending (disabling vs removing) before
// the action command runs; on dismiss (n/esc) no RPC fires. Mirrors
// cron_list.go's confirm handling.
func (m AccountsListModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	confirmed := msg.String() == "y" || msg.String() == "enter"
	id := m.confirmAccountID
	removing := m.confirmRemoving

	var cmd tea.Cmd
	m.confirm, cmd = m.confirm.update(msg)
	if cmd != nil && confirmed && id != "" {
		if removing {
			m.removing[id] = true
		} else {
			m.disabling[id] = true
		}
		m.rebuildTable()
	}
	return m, cmd
}

// --- Helpers ---

func (m *AccountsListModel) setStatus(s string, isErr bool) {
	m.status = s
	m.statusErr = isErr
	m.statusAt = time.Now()
}

func (m AccountsListModel) selectedAccount() *pb.Account {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.accounts) {
		return nil
	}
	return m.accounts[i]
}

func (m AccountsListModel) tableHeight() int {
	return clampedTableHeight(len(m.accounts), m.height, bannerOverhead+1+actionBarPadY+1)
}

func (m *AccountsListModel) rebuildTable() {
	if len(m.accounts) == 0 {
		m.table.SetColumns(nil)
		m.table.SetRows(nil)
		return
	}

	n := len(m.accounts)
	labels := make([]string, n)
	providers := make([]string, n)
	statuses := make([]string, n)
	healths := make([]string, n)
	util5hs := make([]string, n)
	util7ds := make([]string, n)
	usageAges := make([]string, n)
	cooldowns := make([]string, n)
	lastTests := make([]string, n)

	now := time.Now()
	for i, a := range m.accounts {
		labels[i] = accountRowLabel(a)
		providers[i] = a.GetProvider()
		statuses[i] = accountStatusLabel(a)
		healths[i] = accountHealthLabel(a)
		u := a.GetUsage()
		util5hs[i] = accountUtilCell(u, u.GetUtil_5H(), u.GetReset_5H(), now)
		util7ds[i] = accountUtilCell(u, u.GetUtil_7D(), u.GetReset_7D(), now)
		usageAges[i] = accountUsageAgeCell(u, now)
		cooldowns[i] = accountCooldownCell(a, now)
		lastTests[i] = accountLastTestCell(a)
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "LABEL", Width: maxColWidth("LABEL", labels, 30) + tableColumnSep},
		{Title: "PROVIDER", Width: maxColWidth("PROVIDER", providers, 12) + tableColumnSep},
		{Title: "STATUS", Width: maxColWidth("STATUS", statuses, 12) + tableColumnSep},
		{Title: "HEALTH", Width: maxColWidth("HEALTH", healths, 10) + tableColumnSep},
		{Title: "UTIL5H", Width: maxColWidth("UTIL5H", util5hs, 12) + tableColumnSep},
		{Title: "UTIL7D", Width: maxColWidth("UTIL7D", util7ds, 12) + tableColumnSep},
		{Title: "AGE", Width: maxColWidth("AGE", usageAges, 6) + tableColumnSep},
		{Title: "COOLDOWN", Width: maxColWidth("COOLDOWN", cooldowns, 16) + tableColumnSep},
		{Title: "LAST TEST", Width: maxColWidth("LAST TEST", lastTests, 24) + tableColumnSep},
	}

	muted := lipgloss.NewStyle().Foreground(colorMuted)
	cursor := m.table.Cursor()
	rows := make([]table.Row, n)
	for i, a := range m.accounts {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}

		// CREDENTIAL SAFETY: every cell below is derived solely from Account
		// metadata (label/provider/status/health/cooldown/last-test). The
		// Account proto carries NO credential field — "NO credential field.
		// Ever." (proto/bossanova/v1/models.proto) — so this row-builder can
		// never render a secret, even accidentally.
		label, provider, status := labels[i], providers[i], statuses[i]
		health, cooldown, lastTest := healths[i], cooldowns[i], lastTests[i]
		util5h, util7d := util5hs[i], util7ds[i]
		usageAge := usageAges[i]

		disabled := a.GetStatus() == "disabled"
		if !disabled {
			// Colorize HEALTH only for enabled rows; a disabled row is muted
			// uniformly below (an inner color survives an outer muted style,
			// so pre-coloring a to-be-muted cell would leak the accent —
			// see cron_list BOS-313).
			health = accountHealthCell(health)
		}
		switch {
		case m.removing[a.GetId()]:
			status = renderRowPendingStatus(m.spinner, "removing")
		case m.disabling[a.GetId()]:
			// One in-flight label for both directions; the STATUS text tracks the
			// account's current status, so an enable shows "enabling" and a
			// disable shows "disabling".
			if disabled {
				status = renderRowPendingStatus(m.spinner, "enabling")
			} else {
				status = renderRowPendingStatus(m.spinner, "disabling")
			}
		case m.testing[a.GetId()]:
			status = renderRowPendingStatus(m.spinner, "testing")
		}
		if disabled {
			label = muted.Render(label)
			provider = muted.Render(provider)
			status = muted.Render(status)
			health = muted.Render(health)
			util5h = muted.Render(util5h)
			util7d = muted.Render(util7d)
			usageAge = muted.Render(usageAge)
			cooldown = muted.Render(cooldown)
			lastTest = muted.Render(lastTest)
		}

		rows[i] = table.Row{indicator, label, provider, status, health, util5h, util7d, usageAge, cooldown, lastTest}
	}

	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())

	// Clamp the cursor into range. SetRows lowers an out-of-range cursor but
	// leaves −1 (from a prior empty SetRows(nil)) untouched.
	newCursor := cursor
	if newCursor < 0 {
		newCursor = 0
	} else if newCursor >= n {
		newCursor = n - 1
	}
	m.table.SetCursor(newCursor)
}

// accountStatusLabel returns the display STATUS ("active" | "disabled"),
// defaulting to "active" when unset.
func accountStatusLabel(a *pb.Account) string {
	if a == nil || a.GetStatus() == "" {
		return "active"
	}
	return a.GetStatus()
}

// accountHealthCell colors a health label green ("ok") or red ("failed").
// Unknown values pass through uncolored.
func accountHealthCell(health string) string {
	switch health {
	case "ok":
		return styleStatusSuccess.Render(health)
	case "failed":
		return styleStatusDanger.Render(health)
	default:
		return health
	}
}

// accountUtilCell renders a usage-window utilization percentage with the
// window's reset countdown, e.g. "93% (5d)", matching the CLI account list
// (UTIL5H / UTIL7D columns, cmd/account.go). It returns an em dash when the
// snapshot carries no usable signal: nil, never probed, or an
// unsupported/unspecified rate-limit status. The honest-empty gate and percent
// format are shared with the detail-screen accountUsageWindowDetail via
// accountUtilParts; this cell differs only in the countdown decoration ("(<dur>)"
// vs the detail row's "· resets in <dur>").
func accountUtilCell(u *pb.UsageSnapshot, pct float64, reset *timestamppb.Timestamp, now time.Time) string {
	pctStr, countdown, ok := accountUtilParts(u, pct, reset, now)
	if !ok {
		return "—"
	}
	if countdown != "" {
		return pctStr + " (" + countdown + ")"
	}
	return pctStr
}

// usageSnapshotUnsupported reports whether a usage probe could not
// authoritatively determine utilization, so the cell renders an em dash instead
// of a fabricated number. It mirrors the CLI's usageSnapshotUnsupported
// (cmd/account.go) and rotation.UsageSnapshotProbeUnavailable, which this
// package cannot import across the module boundary.
func usageSnapshotUnsupported(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "unsupported", "rate_limit_plan_status_unsupported",
		"unspecified", "rate_limit_plan_status_unspecified":
		return true
	default:
		return false
	}
}

// accountCooldownCell renders the COOLDOWN column: a relative future time while
// the account is cooling down, else an em dash.
func accountCooldownCell(a *pb.Account, now time.Time) string {
	ts := a.GetCooldownUntil()
	if ts == nil || !ts.AsTime().After(now) {
		return "—"
	}
	return relTimeFuture(ts.AsTime())
}

// accountLastTestCell renders the LAST TEST column: the most recent test
// failure detail when the last test failed, else a relative time since the
// last passing test, else an em dash. CREDENTIAL SAFETY: a failed test's raw
// error can carry secret-shaped material (auth headers, provider keys, env
// assignments), so it is masked via maskTestError BEFORE the column truncate —
// never rendered raw.
func accountLastTestCell(a *pb.Account) string {
	if e := a.GetLastTestError(); e != "" {
		return truncateDisplay(maskTestError(e), 22)
	}
	if ts := a.GetLastTestOkAt(); ts != nil && !ts.AsTime().IsZero() {
		return relTimeAgo(ts.AsTime())
	}
	return "—"
}

// --- View ---

func (m AccountsListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	var b strings.Builder

	if m.loading {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			strings.TrimRight(m.spinner.View(), " ") + " Loading accounts…",
		))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return tea.NewView(b.String())
	}

	if len(m.accounts) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(emptyAccountsMessage))
		b.WriteString("\n")
		b.WriteString(actionBar(
			[]string{"[a]dd"},
			[]string{"[esc] back"},
		))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	// The disable/remove confirm prompt takes over the footer while active
	// (mirrors cron_list.go): danger-styled prompt + confirm/cancel action bar.
	if m.confirm.active {
		b.WriteString("\n")
		b.WriteString(m.confirm.footer(m.width))
		return tea.NewView(b.String())
	}

	// While a manual refresh is in flight, show a persistent spinner line (a
	// usage re-probe can outlast the 5s transient-status window). Otherwise show
	// the transient status toast.
	if m.refreshing {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			strings.TrimRight(m.spinner.View(), " ") + " Refreshing usage…",
		))
		b.WriteString("\n")
	} else if m.status != "" && time.Since(m.statusAt) < 5*time.Second {
		b.WriteString("\n")
		style := styleStatusInfo
		if m.statusErr {
			style = styleStatusDanger
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(style.Render(m.status)))
		b.WriteString("\n")
	}
	b.WriteString(actionBar(
		[]string{"[e/enter]dit", "[a]dd", "[t]est", "[r]efresh", "[space] toggle", "[d] remove"},
		[]string{"[esc] back"},
	))

	return tea.NewView(b.String())
}
