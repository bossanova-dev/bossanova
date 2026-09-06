package views

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// accountEditSavedMsg carries the result of an UpdateAccount call.
//
// statusFlip is the status the request asked for when the save was the status
// row's active⇄disabled toggle, and "" for a label or priority save. It is the
// only thing that distinguishes the two on the way back, and it is set from the
// request rather than the response so a failed flip is still attributable.
type accountEditSavedMsg struct {
	account    *pb.Account
	statusFlip string
	err        error
}

// accountEditTestedMsg carries the result of a live TestAccount RPC fired from
// the detail/edit screen (BOS-269). On success it carries the refreshed
// Account (so health / last-test cells splice in place) plus the daemon's safe
// Detail string; on failure it carries err.
type accountEditTestedMsg struct {
	account *pb.Account
	detail  string
	err     error
}

// The edit form shows ONLY the editable fields — Label, Status, Priority.
// NOTE: email is intentionally absent — the Account / UpdateAccountRequest
// protos reserve field "email" on this HEAD, so there is no email to edit
// (see proto/bossanova/v1/{models,daemon}.proto).
const (
	accountEditRowLabel    = 0
	accountEditRowStatus   = 1
	accountEditRowPriority = 2
	accountEditRows        = 3
)

// AccountEditModel is the TUI form for inline-editing a single managed provider
// account (BOS-266). It is reached by pressing e or enter on a row in the
// accounts list (BOS-265), which hands off the selected *pb.Account. It edits
// only Label, Status, and Priority; a read-only Details section above the
// editable rows surfaces the full safe operational breakdown (BOS-269).
//
// It mirrors SessionSettingsModel: a cursor over editable rows, an
// editingField (-1 = not editing) with a textinput per free-text field, local
// validation in commitEdit, and a save tea.Cmd that returns a saved message.
// On save failure the edited input values are retained so the operator can
// retry. Status is a constrained active⇄disabled toggle (never free text) so an
// invalid status can never be submitted. The [t]est action fires the live
// TestAccount RPC and splices the refreshed Account back in place.
//
// CREDENTIAL SAFETY: the Account proto carries no credential field. The only
// secret-shaped daemon string is last_test_error, and it reaches the screen
// ONLY via maskTestError (agenterr redaction) in the read-only Details section
// — never raw.
type AccountEditModel struct {
	client    client.BossClient
	ctx       context.Context
	telemetry telemetry.Client
	account   *pb.Account
	cursor    int
	cancel    bool
	err       error

	// returnView is the view to pop back to on esc (set by the App wiring).
	returnView View

	// Inline editing (-1 = not editing, otherwise the row being edited).
	editingField  int
	labelInput    textinput.Model
	priorityInput textinput.Model

	// confirm gates the `d` = remove action (BOS-268) through the shared
	// value-safe confirmPrompt, using the same accountRemovePromptText +
	// removeAccountCmd helpers as the list so the two surfaces cannot drift. It
	// only opens when not editing a field, so field editing is unaffected.
	confirm confirmPrompt

	// testing is set while a live TestAccount RPC fired from this screen is in
	// flight; testStatus holds its transient result line (testStatusErr colors
	// it as an error). Both are display-only and never block the UI.
	testing       bool
	testStatus    string
	testStatusErr bool

	width  int
	height int
}

// NewAccountEditModel constructs an edit form seeded from the selected account
// handed off by the accounts list.
func NewAccountEditModel(c client.BossClient, ctx context.Context, acct *pb.Account) AccountEditModel {
	li := textinput.New()
	li.Placeholder = "Account label"
	li.SetWidth(60)

	pi := textinput.New()
	pi.Placeholder = "Priority (integer; lower = preferred)"
	pi.SetWidth(60)

	m := AccountEditModel{
		client:        c,
		ctx:           ctx,
		account:       acct,
		editingField:  -1,
		labelInput:    li,
		priorityInput: pi,
	}
	m.seedInputs()
	return m
}

// SetTelemetry installs a telemetry client for the completed status-flip and
// remove actions reachable from the edit screen. Both are the same logical
// actions the accounts list instruments, so leaving this screen uninstrumented
// would make the event's denominator depend on which screen the operator used.
func (m *AccountEditModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// seedInputs (re)loads the editable inputs from the current account.
func (m *AccountEditModel) seedInputs() {
	if m.account == nil {
		return
	}
	m.labelInput.SetValue(m.account.GetLabel())
	m.priorityInput.SetValue(strconv.Itoa(int(m.account.GetPriority())))
}

// Init satisfies tea.Model. The account is seeded at construction, so there is
// nothing to load.
func (m AccountEditModel) Init() tea.Cmd { return nil }

// Cancelled reports whether the user dismissed the edit form.
func (m AccountEditModel) Cancelled() bool { return m.cancel }

// textEntryActive reports whether a row is in inline edit mode with its
// textinput focused, so App can leave ctrl+x alone rather than aliasing it onto
// Esc (BOS-660). editingField is -1 when nothing is being edited, and Esc there
// backs out one level — dismissing the remove-confirm when it is up, otherwise
// exiting the view.
func (m AccountEditModel) textEntryActive() bool { return m.editingField >= 0 }

func (m AccountEditModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Async results and resizes must be handled regardless of editing state.
	// A save command runs asynchronously, so its accountEditSavedMsg can
	// arrive after the operator has opened another field's editor; routing it
	// to the textinput (the editing branch below) would silently drop the
	// result — a failed save would surface no error.
	switch msg := msg.(type) {
	case accountEditSavedMsg:
		// The status flip's action is captured at the app root
		// (App.handleAccountEditSavedResult): [esc] is accepted while the save
		// is in flight, and once App routes away this handler never runs.
		if msg.err != nil {
			// Retain the in-flight edits; only surface the error.
			m.err = msg.err
			return m, nil
		}
		m.account = msg.account
		m.err = nil
		// Only re-seed inputs when not mid-edit so a save result arriving while
		// the operator is typing in another field does not clobber their input.
		if m.editingField < 0 {
			m.seedInputs()
		}
		return m, nil

	case accountRemovedMsg:
		// account_removed is captured at the app root
		// (App.handleAccountRemoveResult). It used to fire here, on the
		// premise that exactly one of the list and this screen is the active
		// view — true, but not enough: neither is active once the operator
		// escapes out while the RPC is still in flight, and then nobody fired.
		// Removal succeeded — pop back to the list (mirrors esc). On failure,
		// keep the form open and surface the error so the operator can retry.
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.cancel = true
		return m, nil

	case accountEditTestedMsg:
		m.testing = false
		if msg.err != nil {
			// Non-blocking: surface the failure as a transient status line and
			// leave the account unchanged. CREDENTIAL SAFETY: the RPC error text
			// is masked too — a wrapped daemon/keyring error could echo a
			// secret-shaped fragment, so it never reaches the screen raw.
			m.testStatus = "Test failed: " + maskTestError(rpcStatusDetail(msg.err))
			m.testStatusErr = true
			return m, nil
		}
		if msg.account != nil {
			m.account = msg.account
			// Re-seed only when not mid-edit so a result arriving while the
			// operator is typing does not clobber their input.
			if m.editingField < 0 {
				m.seedInputs()
			}
		}
		// CREDENTIAL SAFETY: on a failed live smoke test the daemon returns
		// Detail byte-identical to last_test_error (recordAndRespond), which can
		// carry secret-shaped material. It must pass through maskTestError before
		// the transient status line — the same seam the Details row uses — so the
		// masked "Last tested" row is never contradicted by a raw string below it.
		detail := maskTestError(msg.detail)
		if detail == "" {
			detail = "Test complete."
		}
		m.testStatus = detail
		m.testStatusErr = false
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}

	// A remove confirm prompt intercepts keys while active (it only opens when
	// not editing a field, so the editing branch below is unaffected).
	if m.confirm.active {
		if keyMsg, ok := msg.(tea.KeyMsg); ok {
			var cmd tea.Cmd
			m.confirm, cmd = m.confirm.update(keyMsg)
			return m, cmd
		}
		return m, nil
	}

	// While editing a free-text field, forward all message types (not just
	// KeyMsg) to the active textinput so paste/IME work.
	if m.editingField >= 0 {
		return m.updateEditing(msg)
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < accountEditRows-1 {
				m.cursor++
			}
		case "d":
			// Remove account (BOS-268) — always behind a confirm because removal
			// purges the keyring credential and is irreversible. The edit view has
			// no preloaded session snapshot, so the bound count is unknown and the
			// prompt uses the generic bound-session warning.
			if m.account == nil {
				return m, nil
			}
			id := m.account.GetId()
			count, known := boundSessionCount(nil, false, id)
			m.confirm = newConfirmPrompt(
				accountRemovePromptText(m.account, count, known),
				removeAccountCmd(m.client, m.ctx, id),
			)
			return m, nil
		case "enter", "space":
			return m.activateRow()
		case "t":
			// Live-test this account (BOS-269): fire the TestAccount RPC and
			// splice the refreshed Account back in on the result. No-op while a
			// test is already in flight.
			if m.account == nil || m.testing {
				return m, nil
			}
			m.testing = true
			m.testStatus = ""
			return m, m.testAccountCmd()
		}
	}

	return m, nil
}

// testAccountCmd fires the live TestAccount RPC for the current account and
// maps the result into an accountEditTestedMsg. The daemon persists the outcome
// and returns the refreshed Account, so the result carries a fresh row to
// splice in place.
func (m AccountEditModel) testAccountCmd() tea.Cmd {
	id := m.account.GetId()
	return func() tea.Msg {
		resp, err := m.client.TestAccount(m.ctx, id)
		if err != nil {
			return accountEditTestedMsg{err: err}
		}
		return accountEditTestedMsg{account: resp.GetAccount(), detail: resp.GetDetail()}
	}
}

func (m AccountEditModel) updateEditing(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			return m.commitEdit()
		case "esc":
			return m.cancelEdit(), nil
		}
	}

	var cmd tea.Cmd
	switch m.editingField {
	case accountEditRowLabel:
		m.labelInput, cmd = m.labelInput.Update(msg)
	case accountEditRowPriority:
		m.priorityInput, cmd = m.priorityInput.Update(msg)
	}
	return m, cmd
}

// activateRow handles enter/space on the selected editable row. Label and
// priority open a text editor; status is a constrained toggle that saves
// immediately (no free-text entry).
func (m AccountEditModel) activateRow() (tea.Model, tea.Cmd) {
	if m.account == nil {
		return m, nil
	}
	switch m.cursor {
	case accountEditRowLabel:
		m.editingField = accountEditRowLabel
		return m, m.labelInput.Focus()
	case accountEditRowPriority:
		m.editingField = accountEditRowPriority
		return m, m.priorityInput.Focus()
	case accountEditRowStatus:
		// The constants, not literals: accountStatusAction reads this exact
		// value back off the result message to decide whether the flip is
		// reported as account_disabled or account_enabled.
		next := accountStatusDisabled
		if accountStatusLabel(m.account) == accountStatusDisabled {
			next = accountStatusActive
		}
		return m, m.saveAccount(&pb.UpdateAccountRequest{Id: m.account.GetId(), Status: &next})
	}
	return m, nil
}

func (m AccountEditModel) commitEdit() (tea.Model, tea.Cmd) {
	switch m.editingField {
	case accountEditRowLabel:
		label := strings.TrimSpace(m.labelInput.Value())
		if label == "" {
			// Matches the server rule (account.go): reject locally, keep the
			// input so the edit is not lost.
			m.err = errors.New("label cannot be empty")
			return m, nil
		}
		m.editingField = -1
		m.err = nil
		m.labelInput.Blur()
		return m, m.saveAccount(&pb.UpdateAccountRequest{Id: m.account.GetId(), Label: &label})

	case accountEditRowPriority:
		raw := strings.TrimSpace(m.priorityInput.Value())
		// ParseInt with bitSize 32 both parses and range-checks so the value
		// always fits the proto's int32 priority field (no overflow).
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			m.err = fmt.Errorf("priority must be an integer")
			return m, nil
		}
		p := int32(n)
		m.editingField = -1
		m.err = nil
		m.priorityInput.Blur()
		return m, m.saveAccount(&pb.UpdateAccountRequest{Id: m.account.GetId(), Priority: &p})
	}
	return m, nil
}

func (m AccountEditModel) cancelEdit() AccountEditModel {
	switch m.editingField {
	case accountEditRowLabel:
		m.labelInput.Blur()
		if m.account != nil {
			m.labelInput.SetValue(m.account.GetLabel())
		}
	case accountEditRowPriority:
		m.priorityInput.Blur()
		if m.account != nil {
			m.priorityInput.SetValue(strconv.Itoa(int(m.account.GetPriority())))
		}
	}
	m.editingField = -1
	m.err = nil
	return m
}

// saveAccount issues an UpdateAccount carrying Id plus only the changed field.
// allowed_models is never populated from this screen.
func (m AccountEditModel) saveAccount(req *pb.UpdateAccountRequest) tea.Cmd {
	// Captured from the request before the RPC so the result message can be
	// attributed to a status flip even when the call failed and no Account came
	// back. Label/priority saves leave Status nil, so this stays "".
	statusFlip := req.GetStatus()
	return func() tea.Msg {
		acct, err := m.client.UpdateAccount(m.ctx, req)
		return accountEditSavedMsg{account: acct, statusFlip: statusFlip, err: err}
	}
}

// --- View ---

func (m AccountEditModel) View() tea.View {
	if m.account == nil {
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("No account selected."))
	}
	a := m.account

	var b strings.Builder

	// Title: which account is being edited (a heading for orientation, not an
	// editable field). Provider + label keep the operator oriented after the
	// list handoff without displaying any non-editable field rows.
	title := "Edit account"
	if p := a.GetProvider(); p != "" {
		title += " · " + p
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render(title))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(renderError(rpcErrorMessage(m.err), m.width))
		b.WriteString("\n")
	}

	// Read-only Details: the full SAFE operational breakdown (BOS-269). Rendered
	// above the editable rows so the operator sees health / cooldown / last-test
	// context before editing. The last-test error is ALWAYS masked.
	b.WriteString(m.renderDetails(a))

	// Editable fields: Label, Status, Priority.
	b.WriteString(m.renderEditableRow(accountEditRowLabel, "Label", a.GetLabel(), &m.labelInput))
	b.WriteString(m.renderStatusRow(a))
	b.WriteString(m.renderEditableRow(accountEditRowPriority, "Priority", strconv.Itoa(int(a.GetPriority())), &m.priorityInput))

	// Transient test-status line (from [t]est) — testing… while in flight, then
	// the daemon's safe Detail string (or a non-blocking error).
	if m.testing {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(styleStatusInfo.Render("testing…")))
		b.WriteString("\n")
	} else if m.testStatus != "" {
		style := styleStatusInfo
		if m.testStatusErr {
			style = styleStatusDanger
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(style.Render(m.testStatus)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	switch {
	case m.confirm.active:
		b.WriteString(m.confirm.footer(m.width))
	case m.editingField >= 0:
		b.WriteString(actionBarWidth(m.width, []string{"[enter] save", "[esc] cancel"}))
	default:
		b.WriteString(actionBarWidth(m.width,
			[]string{"[enter/space] edit", "[t]est", "[d] remove"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}

// renderDetails renders the read-only account breakdown shown above the
// editable rows (BOS-269): provider, status, health, priority, tier, cooldown,
// last-used, last-tested, created, and updated. Every value is derived from the
// Account metadata proto (no credential field), and the last-tested line routes
// its error through maskTestError so a secret-shaped daemon message is never
// rendered raw.
func (m AccountEditModel) renderDetails(a *pb.Account) string {
	now := time.Now()
	u := a.GetUsage()
	rows := [][2]string{
		{"Provider", a.GetProvider()},
		{"Status", accountStatusLabel(a)},
		{"Health", accountHealthCellFor(a, accountHealthLabel(a))},
		// Directly under Health, because it is the field that can veto it. The
		// credential check is ordered BEFORE the long masked diagnostics below:
		// a load-bearing verdict placed after a multi-line payload is the field
		// that disappears (BOS-892).
		{"Credential check", accountCheckedDetail(a)},
		{"Priority", strconv.Itoa(int(a.GetPriority()))},
		{"Tier", detailOrDash(a.GetTier())},
		{"Usage 5h", accountUsageWindowDetail(u, u.GetUtil_5H(), u.GetReset_5H(), now)},
		{"Usage 7d", accountUsageWindowDetail(u, u.GetUtil_7D(), u.GetReset_7D(), now)},
		{"Plan tier", detailOrDash(u.GetPlanTier())},
		{"Usage age", accountUsageAgeDetail(u)},
		{"Cooldown", accountCooldownDetail(a, now)},
		{"Last used", accountLastUsedDetail(a)},
		{"Last tested", accountLastTestedDetail(a)},
		{"Created", detailTimestamp(a.GetCreatedAt())},
		{"Updated", detailTimestamp(a.GetUpdatedAt())},
	}

	label := lipgloss.NewStyle().Foreground(colorMuted)
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render("Details"))
	b.WriteString("\n")
	for _, r := range rows {
		line := "  " + label.Render(r[0]+":") + " " + r[1]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// detailOrDash returns s, or an em dash when s is empty, for detail rows whose
// value may be unset.
func detailOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// detailTimestamp renders an optional timestamp as a relative time, or an em
// dash when unset.
func detailTimestamp(ts *timestamppb.Timestamp) string {
	if ts == nil || ts.AsTime().IsZero() {
		return "—"
	}
	return relTimeAgo(ts.AsTime())
}

// renderEditableRow renders a label/priority row: the inline textinput when it
// is being edited, otherwise a "Label: value" line with cursor chevron and
// selection styling.
func (m AccountEditModel) renderEditableRow(row int, label, value string, input *textinput.Model) string {
	if m.editingField == row {
		var b strings.Builder
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", label)))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(input.View()))
		b.WriteString("\n")
		return b.String()
	}
	return m.renderSelectableLine(row, fmt.Sprintf("%s: %s", label, value))
}

// renderStatusRow renders the status toggle row with a hint that enter flips it.
func (m AccountEditModel) renderStatusRow(a *pb.Account) string {
	line := fmt.Sprintf("Status: %s  (enter toggles active/disabled)", accountStatusLabel(a))
	return m.renderSelectableLine(accountEditRowStatus, line)
}

// renderSelectableLine renders a single cursor-selectable line, carrying the
// focus gutter and bold styling when the cursor is on it.
func (m AccountEditModel) renderSelectableLine(row int, text string) string {
	return renderFieldRow(m.cursor == row, text) + "\n"
}
