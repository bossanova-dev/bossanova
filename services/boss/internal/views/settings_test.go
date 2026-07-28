package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
)

// settingsAgentStub embeds *stubClient so it satisfies BossClient — only
// ListAgents matters for these tests, the rest panic.
type settingsAgentStub struct {
	*stubClient
	agents []client.AgentInfo
}

func (s *settingsAgentStub) ListAgents(context.Context) ([]client.AgentInfo, error) {
	return s.agents, nil
}

// newBillingSettingsModel builds a settings model wired with a cloud client so
// the [b]illing action is live, and overrides the browser-open hook so tests
// observe the open call without launching a real browser. The returned cleanup
// restores the global hook.
func newBillingSettingsModel(t *testing.T, fake *fakeHomeCloudAccessClient, returnURL string, opened *[]string) (SettingsModel, func()) {
	t.Helper()
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m.SetCloudAccess(fake, returnURL)

	original := openBillingPortalURL
	openBillingPortalURL = func(rawURL string) error {
		*opened = append(*opened, rawURL)
		return nil
	}
	return m, func() { openBillingPortalURL = original }
}

// pressBilling sends the [b] key and runs the resulting async command,
// applying the emitted billingPortalMsg so the model reflects the outcome.
func pressBilling(t *testing.T, m SettingsModel) SettingsModel {
	t.Helper()
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	m = updated.(SettingsModel)
	if cmd == nil {
		t.Fatal("key b: got nil cmd, want billing portal command")
	}
	msg := cmd()
	if _, ok := msg.(billingPortalMsg); !ok {
		t.Fatalf("billing command returned %T, want billingPortalMsg", msg)
	}
	updated, _ = m.Update(msg)
	return updated.(SettingsModel)
}

// --- Section menu (BOS-511) ---

// newMenuSettingsModel builds the Settings hub without a cloud client, so the
// menu carries the five always-present sections.
func newMenuSettingsModel(t *testing.T) SettingsModel {
	t.Helper()
	withTempConfigHome(t)
	return NewSettingsModel(nil, context.Background())
}

// wantSections is the documented menu order and copy. The tests below drive
// every assertion off this table so a reordering or reworded description has a
// single place to change.
var wantSections = []struct {
	key         string
	label       string
	description string
	target      View
	billing     bool
}{
	{key: "g", label: "General", description: "Worktree location, polling, notifications, agents, tracing", target: ViewGeneralSettings},
	{key: "r", label: "Repositories", description: "Registered repos and their per-repo options", target: ViewRepoList},
	{key: "c", label: "Cron jobs", description: "Scheduled skill runs", target: ViewCron},
	{key: "a", label: "Accounts", description: "Agent provider accounts and rotation", target: ViewAccounts},
	{key: "t", label: "Trash", description: "Archived sessions", target: ViewTrash},
	{key: "b", label: "Billing", description: "Subscription and payment", billing: true},
}

func TestSettings_MenuRendersEverySectionInOrderWithDescriptions(t *testing.T) {
	m := newMenuSettingsModel(t)
	content := m.View().Content

	prev := -1
	for _, want := range wantSections {
		if want.billing {
			continue // no cloud client wired in this model
		}
		at := strings.Index(content, want.label)
		if at < 0 {
			t.Fatalf("settings menu missing section %q:\n%s", want.label, content)
		}
		if at <= prev {
			t.Fatalf("section %q rendered out of order (index %d, previous %d):\n%s", want.label, at, prev, content)
		}
		prev = at
		if !strings.Contains(content, want.description) {
			t.Fatalf("section %q missing its description %q:\n%s", want.label, want.description, content)
		}
	}
}

func TestSettings_BillingSectionOnlyWithCloudAccess(t *testing.T) {
	without := newMenuSettingsModel(t)
	if got := len(without.sections()); got != len(wantSections)-1 {
		t.Fatalf("sections without a cloud client = %d, want %d", got, len(wantSections)-1)
	}
	for _, s := range without.sections() {
		if s.Billing {
			t.Fatalf("Billing section present without a cloud client: %+v", s)
		}
	}
	if strings.Contains(without.View().Content, "Subscription and payment") {
		t.Fatalf("settings rendered the Billing row without a cloud client:\n%s", without.View().Content)
	}

	with := newMenuSettingsModel(t)
	with.SetCloudAccess(&fakeHomeCloudAccessClient{}, "")
	sections := with.sections()
	if got := len(sections); got != len(wantSections) {
		t.Fatalf("sections with a cloud client = %d, want %d", got, len(wantSections))
	}
	last := sections[len(sections)-1]
	if !last.Billing || last.Label != "Billing" {
		t.Fatalf("last section = %+v, want the Billing action", last)
	}
	content := with.View().Content
	if !strings.Contains(content, "Billing") || !strings.Contains(content, "Subscription and payment") {
		t.Fatalf("settings did not render the Billing row with a cloud client:\n%s", content)
	}
}

func TestSettings_CursorMovesAndEnterOpensSectionUnderCursor(t *testing.T) {
	m := newMenuSettingsModel(t)

	// down twice → Cron jobs (index 2); one up → Repositories (index 1).
	keys := []tea.KeyPressMsg{keyPress('j'), keyPress('j'), {Code: tea.KeyUp}}
	for _, key := range keys {
		updated, _ := m.Update(key)
		m = updated.(SettingsModel)
	}
	if m.cursor != 1 {
		t.Fatalf("cursor after down/down/up = %d, want 1", m.cursor)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	svm, ok := runCmd(cmd).(switchViewMsg)
	if !ok {
		t.Fatalf("enter on the Repositories row produced %T, want switchViewMsg", runCmd(cmd))
	}
	if svm.view != ViewRepoList || svm.returnView != ViewSettings {
		t.Fatalf("enter routed to %v (return %v), want ViewRepoList/ViewSettings", svm.view, svm.returnView)
	}
}

// TestSettings_CursorClampsAtBothEnds runs against BOTH menu shapes. The
// conditional Billing row is appended after the five fixed sections, so a
// clamp that used a fixed bound instead of len(m.sections()) would leave the
// user able to see the Billing row but never move the cursor onto it — a bug
// the no-cloud shape cannot expose.
func TestSettings_CursorClampsAtBothEnds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withCloud bool
		wantLast  int
	}{
		{name: "without a cloud client", wantLast: 4},
		{name: "with the Billing row", withCloud: true, wantLast: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMenuSettingsModel(t)
			if tc.withCloud {
				m.SetCloudAccess(&fakeHomeCloudAccessClient{}, "")
			}
			if got := len(m.sections()) - 1; got != tc.wantLast {
				t.Fatalf("last section index = %d, want %d", got, tc.wantLast)
			}

			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
			m = updated.(SettingsModel)
			if m.cursor != 0 {
				t.Fatalf("cursor after up from the top = %d, want 0 (no wrap)", m.cursor)
			}

			// Press down more times than there are rows, so the clamp (not the
			// row count) is what stops the cursor.
			for range tc.wantLast + 3 {
				updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				m = updated.(SettingsModel)
			}
			if m.cursor != tc.wantLast {
				t.Fatalf("cursor after running past the bottom = %d, want %d (no wrap)", m.cursor, tc.wantLast)
			}
		})
	}
}

// TestSettings_MenuUsesTheSelectListRenderingConventions pins the shape the
// BOS-511 plan mandates (the onboarding.go provider list): a "❯ " cursor
// prefix that moves with the cursor, each description on its own line beneath
// its label, and a blank line between entries so a label is never mistaken for
// a continuation of the description above it.
// Every bound is derived from m.sections() rather than the fixture's length, so
// adding a section cannot silently fall outside the assertions. Both menu
// shapes run, so the last-row branch of View's separator is observed at Trash
// and at Billing.
func TestSettings_MenuUsesTheSelectListRenderingConventions(t *testing.T) {
	for _, withCloud := range []bool{false, true} {
		name := "without a cloud client"
		if withCloud {
			name = "with the Billing row"
		}
		t.Run(name, func(t *testing.T) {
			m := newMenuSettingsModel(t)
			if withCloud {
				m.SetCloudAccess(&fakeHomeCloudAccessClient{}, "")
			}
			sections := m.sections()

			// menuLines returns the plain-text view split into lines, and the
			// index of the row line for each section label. A row line is
			// matched on the bare label so a description or the action bar can
			// never be mistaken for one.
			menuLines := func(m SettingsModel) ([]string, map[string]int) {
				t.Helper()
				lines := strings.Split(trimLineRightSpace(stripANSI(m.View().Content)), "\n")
				at := map[string]int{}
				for i, l := range lines {
					bare := strings.TrimSpace(strings.ReplaceAll(l, cursorChevron, ""))
					for _, s := range sections {
						if bare == s.Label {
							at[s.Label] = i
						}
					}
				}
				for _, s := range sections {
					if _, ok := at[s.Label]; !ok {
						t.Fatalf("no row line for %q in:\n%s", s.Label, strings.Join(lines, "\n"))
					}
				}
				return lines, at
			}

			lines, at := menuLines(m)
			for i, s := range sections {
				li := at[s.Label]
				if got := strings.TrimSpace(lines[li+1]); !strings.Contains(got, s.Description) {
					t.Fatalf("line under %q = %q, want the description %q", s.Label, got, s.Description)
				}
				// Every entry but the last is followed by a blank separator.
				if i == len(sections)-1 {
					continue
				}
				if got := lines[li+2]; got != "" {
					t.Fatalf("line after %q's description = %q, want a blank separator", s.Label, got)
				}
			}

			// The chevron marks the cursor row and nothing else, and it
			// follows the cursor down the menu.
			first, second := sections[0].Label, sections[1].Label
			if !strings.Contains(lines[at[first]], cursorChevron) {
				t.Fatalf("%s row %q carries no %q cursor", first, lines[at[first]], cursorChevron)
			}
			if strings.Contains(lines[at[second]], cursorChevron) {
				t.Fatalf("%s row %q carries the cursor while it is on %s", second, lines[at[second]], first)
			}

			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			lines, at = menuLines(updated.(SettingsModel))
			if !strings.Contains(lines[at[second]], cursorChevron) {
				t.Fatalf("after down, %s row %q carries no cursor", second, lines[at[second]])
			}
			if strings.Contains(lines[at[first]], cursorChevron) {
				t.Fatalf("after down, %s row %q still carries the cursor", first, lines[at[first]])
			}
		})
	}
}

func TestSettings_EscCancels(t *testing.T) {
	m := newMenuSettingsModel(t)
	if m.Cancelled() {
		t.Fatal("precondition: a fresh settings model reports Cancelled()")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !updated.(SettingsModel).Cancelled() {
		t.Fatal("esc did not cancel the settings menu")
	}
}

// TestSettings_RowAndHotkeyParity is the regression guard BOS-511 exists for: a
// section must be reachable identically by enter-on-its-row and by its letter
// accelerator, so neither path can silently drift from the other.
func TestSettings_RowAndHotkeyParity(t *testing.T) {
	for _, want := range wantSections {
		if want.billing {
			continue // billing is an in-place action, covered by the billing tests
		}
		t.Run(want.label, func(t *testing.T) {
			base := newMenuSettingsModel(t)
			base.SetCloudAccess(&fakeHomeCloudAccessClient{}, "")

			idx := -1
			for i, s := range base.sections() {
				if s.Label == want.label {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Fatalf("section %q not found in the menu", want.label)
			}

			byCursor := base
			byCursor.cursor = idx
			_, enterCmd := byCursor.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			enterMsg, ok := runCmd(enterCmd).(switchViewMsg)
			if !ok {
				t.Fatalf("enter on %q produced %T, want switchViewMsg", want.label, runCmd(enterCmd))
			}

			_, keyCmd := base.Update(keyPress(rune(want.key[0])))
			keyMsg, ok := runCmd(keyCmd).(switchViewMsg)
			if !ok {
				t.Fatalf("key %q produced %T, want switchViewMsg", want.key, runCmd(keyCmd))
			}

			if enterMsg.view != want.target {
				t.Fatalf("enter on %q routed to %v, want %v", want.label, enterMsg.view, want.target)
			}
			if enterMsg.view != keyMsg.view || enterMsg.returnView != keyMsg.returnView {
				t.Fatalf("enter on %q gave %v/%v but key %q gave %v/%v — row and hotkey diverged",
					want.label, enterMsg.view, enterMsg.returnView, want.key, keyMsg.view, keyMsg.returnView)
			}
		})
	}
}

func TestSettings_HotkeysMoveCursorToActivatedSection(t *testing.T) {
	for _, want := range wantSections {
		t.Run(want.label, func(t *testing.T) {
			m := newMenuSettingsModel(t)
			if want.billing {
				m.SetCloudAccess(&fakeHomeCloudAccessClient{}, "")
			}

			updated, _ := m.Update(keyPress(rune(want.key[0])))
			m = updated.(SettingsModel)
			if got := m.selectedKey(); got != want.key {
				t.Fatalf("key %q left selected key %q, want %q", want.key, got, want.key)
			}
		})
	}
}

func TestSettings_RestoreSectionUsesKeysAndFallsBackToGeneral(t *testing.T) {
	fresh := newMenuSettingsModel(t)
	if got := fresh.selectedKey(); got != "g" {
		t.Fatalf("fresh Settings selected %q, want General", got)
	}

	withBilling := newMenuSettingsModel(t)
	withBilling.SetCloudAccess(&fakeHomeCloudAccessClient{}, "")
	withBilling.restoreSection("b")
	if got := withBilling.selectedKey(); got != "b" {
		t.Fatalf("visible Billing restored %q, want b", got)
	}

	withoutBilling := newMenuSettingsModel(t)
	withoutBilling.restoreSection("b")
	if got := withoutBilling.selectedKey(); got != "g" {
		t.Fatalf("hidden Billing restored %q, want General fallback", got)
	}

	withoutBilling.restoreSection("unknown")
	if got := withoutBilling.selectedKey(); got != "g" {
		t.Fatalf("unknown section restored %q, want General fallback", got)
	}
}

// TestSettings_GeneralCarriesNoReturnView pins the static-parent decision: the
// General list has no returnView slot, so the switch must leave it zero (app.go
// ignores the field for this view and routes esc straight back to Settings).
func TestSettings_GeneralCarriesNoReturnView(t *testing.T) {
	m := newMenuSettingsModel(t)
	_, cmd := m.Update(keyPress('g'))
	svm, ok := runCmd(cmd).(switchViewMsg)
	if !ok {
		t.Fatalf("key g produced %T, want switchViewMsg", runCmd(cmd))
	}
	if svm.view != ViewGeneralSettings {
		t.Fatalf("key g routed to %v, want ViewGeneralSettings", svm.view)
	}
	if svm.returnView != ViewHome {
		t.Fatalf("key g set returnView = %v, want the zero value (unused for this view)", svm.returnView)
	}
}

// TestViewGeneralSettingsIsHighestViewValue encodes the one half of the
// append-at-end convention that is mechanically checkable from inside the
// package: ViewGeneralSettings must remain the highest View value, so it was
// appended after every pre-existing constant rather than spliced in among
// them. That is exactly the BOS-511 acceptance criterion.
//
// It deliberately does NOT claim to catch a later mid-block insertion: such an
// insertion shifts ViewGeneralSettings by the same amount as every constant
// below it, so the relative ordering — and this test — still holds. Guarding
// that would mean pinning numeric values, which buys nothing here because View
// is a package-internal iota that is never persisted or serialized.
func TestViewGeneralSettingsIsHighestViewValue(t *testing.T) {
	others := map[string]View{
		"ViewHome":            ViewHome,
		"ViewNewSession":      ViewNewSession,
		"ViewAttach":          ViewAttach,
		"ViewChatPicker":      ViewChatPicker,
		"ViewRepoAdd":         ViewRepoAdd,
		"ViewRepoList":        ViewRepoList,
		"ViewRepoSettings":    ViewRepoSettings,
		"ViewTrash":           ViewTrash,
		"ViewSettings":        ViewSettings,
		"ViewSessionSettings": ViewSessionSettings,
		"ViewLogin":           ViewLogin,
		"ViewBugReport":       ViewBugReport,
		"ViewCron":            ViewCron,
		"ViewCronForm":        ViewCronForm,
		"ViewOnboarding":      ViewOnboarding,
		"ViewAccounts":        ViewAccounts,
		"ViewAccountEdit":     ViewAccountEdit,
		"ViewAccountRegister": ViewAccountRegister,
	}
	for name, v := range others {
		if ViewGeneralSettings <= v {
			t.Fatalf("ViewGeneralSettings (%d) is not above %s (%d) — new views must be appended at the END of the const block so existing iota values do not shift",
				ViewGeneralSettings, name, v)
		}
	}
}

// --- Billing ---

func TestSettings_BillingActionHiddenWithoutCloudClient(t *testing.T) {
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	if strings.Contains(m.View().Content, "[b]illing") {
		t.Fatalf("settings rendered [b]illing without a cloud client:\n%s", m.View().Content)
	}

	// Pressing b without a cloud client must be inert (no command, no crash):
	// there is no Billing section to match.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if cmd != nil {
		t.Fatal("key b: got a command without a cloud client, want nil")
	}
}

func TestSettings_BillingOpensPortalURL(t *testing.T) {
	var opened []string
	fake := &fakeHomeCloudAccessClient{portalURL: "https://billing.example.test/portal"}
	m, cleanup := newBillingSettingsModel(t, fake, "https://app.example.test/return", &opened)
	defer cleanup()

	if !strings.Contains(m.View().Content, "[b]illing") {
		t.Fatalf("settings did not render [b]illing with a cloud client:\n%s", m.View().Content)
	}

	m = pressBilling(t, m)

	if fake.portals != 1 {
		t.Fatalf("CreateBillingPortalSession calls = %d, want 1", fake.portals)
	}
	if len(opened) != 1 || opened[0] != "https://billing.example.test/portal" {
		t.Fatalf("browser opened with %v, want [https://billing.example.test/portal]", opened)
	}
	if !strings.Contains(m.View().Content, "Opened the billing portal") {
		t.Fatalf("settings missing success status:\n%s", m.View().Content)
	}
}

// TestSettings_BillingEnterOnRowOpensPortal covers the row half of the billing
// pair: the Billing section is an in-place action, so enter on its row must run
// the same portal flow the [b] accelerator does.
func TestSettings_BillingEnterOnRowOpensPortal(t *testing.T) {
	var opened []string
	fake := &fakeHomeCloudAccessClient{portalURL: "https://billing.example.test/portal"}
	m, cleanup := newBillingSettingsModel(t, fake, "", &opened)
	defer cleanup()

	sections := m.sections()
	m.cursor = len(sections) - 1
	if !sections[m.cursor].Billing {
		t.Fatalf("last section = %+v, want the Billing action", sections[m.cursor])
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(SettingsModel)
	if cmd == nil {
		t.Fatal("enter on the Billing row: got nil cmd, want the billing portal command")
	}
	msg := cmd()
	if _, ok := msg.(billingPortalMsg); !ok {
		t.Fatalf("billing command returned %T, want billingPortalMsg", msg)
	}
	updated, _ = m.Update(msg)
	m = updated.(SettingsModel)

	if fake.portals != 1 {
		t.Fatalf("CreateBillingPortalSession calls = %d, want 1", fake.portals)
	}
	if !strings.Contains(m.View().Content, "Opened the billing portal") {
		t.Fatalf("settings missing success status after enter on the Billing row:\n%s", m.View().Content)
	}
}

func TestSettings_BillingLocalDaemonShowsUnavailableMessage(t *testing.T) {
	var opened []string
	fake := &fakeHomeCloudAccessClient{
		portalErr: connect.NewError(connect.CodeUnimplemented,
			errors.New("cloud billing is only available on a local daemon")),
	}
	m, cleanup := newBillingSettingsModel(t, fake, "", &opened)
	defer cleanup()

	m = pressBilling(t, m)

	if fake.portals != 1 {
		t.Fatalf("CreateBillingPortalSession calls = %d, want 1", fake.portals)
	}
	if len(opened) != 0 {
		t.Fatalf("browser opened %v on local-daemon error, want none", opened)
	}
	if !strings.Contains(m.View().Content, "Billing isn't available for a local daemon") {
		t.Fatalf("settings missing local-daemon message:\n%s", m.View().Content)
	}
}

func TestSettings_BillingBrowserFailureSurfacesURL(t *testing.T) {
	withTempConfigHome(t)
	fake := &fakeHomeCloudAccessClient{portalURL: "https://billing.example.test/portal"}
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m.SetCloudAccess(fake, "")

	original := openBillingPortalURL
	openBillingPortalURL = func(string) error { return errors.New("no browser available") }
	defer func() { openBillingPortalURL = original }()

	m = pressBilling(t, m)

	if fake.portals != 1 {
		t.Fatalf("CreateBillingPortalSession calls = %d, want 1", fake.portals)
	}
	content := m.View().Content
	if !strings.Contains(content, "https://billing.example.test/portal") {
		t.Fatalf("browser-failure fallback did not surface the URL:\n%s", content)
	}
	if !strings.Contains(content, "Couldn't open a browser") {
		t.Fatalf("browser-failure fallback missing explanation:\n%s", content)
	}
}

func TestSettings_BillingInFlightGuardSkipsSecondRequest(t *testing.T) {
	var opened []string
	fake := &fakeHomeCloudAccessClient{portalURL: "https://billing.example.test/portal"}
	m, cleanup := newBillingSettingsModel(t, fake, "", &opened)
	defer cleanup()

	// First [b]: dispatches the RPC command and marks the request in flight.
	// We deliberately do NOT run the command yet, simulating a request that is
	// still outstanding.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	m = updated.(SettingsModel)
	if cmd == nil {
		t.Fatal("first b: got nil cmd, want billing portal command")
	}
	if !m.billingInFlight {
		t.Fatal("first b: billingInFlight = false, want true")
	}
	if m.billingStatus != billingOpeningStatus {
		t.Fatalf("first b: billingStatus = %q, want %q", m.billingStatus, billingOpeningStatus)
	}
	// Assert the literal copy too. Every other assertion here compares against
	// billingOpeningStatus, so emptying the constant would make them all
	// vacuous (strings.Contains(x, "") is always true) while View() renders no
	// status block at all — exactly the regression they exist to catch.
	if !strings.Contains(m.View().Content, "Opening the billing portal") {
		t.Fatalf("first b: the in-flight status is not on screen:\n%s", m.View().Content)
	}

	// Second [b] while the first is still in flight must be a no-op: no second
	// command, and the RPC fake must not have been invoked a second time. It
	// must NOT blank the status either — every keypress clears billingStatus
	// before dispatch, so an early return that did not restore it would leave
	// an outstanding request with no feedback at all.
	updated, cmd2 := m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	m = updated.(SettingsModel)
	if cmd2 != nil {
		t.Fatal("second b while in flight: got a command, want nil")
	}
	if m.billingStatus != billingOpeningStatus {
		t.Fatalf("second b while in flight: billingStatus = %q, want it still %q", m.billingStatus, billingOpeningStatus)
	}
	if !strings.Contains(m.View().Content, billingOpeningStatus) {
		t.Fatalf("second b while in flight: the status left the screen:\n%s", m.View().Content)
	}

	// Complete the first request and confirm exactly one RPC fired and the
	// in-flight flag resets so a later [b] works again.
	updated, _ = m.Update(cmd())
	m = updated.(SettingsModel)
	if fake.portals != 1 {
		t.Fatalf("CreateBillingPortalSession calls = %d, want 1", fake.portals)
	}
	if m.billingInFlight {
		t.Fatal("after completion: billingInFlight = true, want false")
	}

	// A later [b] after completion issues a fresh request.
	m = pressBilling(t, m)
	if fake.portals != 2 {
		t.Fatalf("CreateBillingPortalSession calls after re-press = %d, want 2", fake.portals)
	}
}

func TestSettings_BillingStatusClearedOnNextKey(t *testing.T) {
	var opened []string
	fake := &fakeHomeCloudAccessClient{portalURL: "https://billing.example.test/portal"}
	m, cleanup := newBillingSettingsModel(t, fake, "", &opened)
	defer cleanup()

	m = pressBilling(t, m)
	if !strings.Contains(m.View().Content, "Opened the billing portal") {
		t.Fatalf("precondition: billing status not shown after pressing b:\n%s", m.View().Content)
	}

	// An unrelated navigation keypress should clear the lingering status.
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = updated.(SettingsModel)
	if strings.Contains(m.View().Content, "Opened the billing portal") {
		t.Fatalf("billing status lingered after an unrelated keypress:\n%s", m.View().Content)
	}
}
