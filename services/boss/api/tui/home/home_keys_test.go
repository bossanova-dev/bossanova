package home

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// renameKeyHome is a three-session board with its rows already built, so the
// table cursor has somewhere to move to.
func renameKeyHome(t *testing.T) Model {
	t.Helper()
	h := NewModel(nil, context.Background(), nil)
	h.Loading = false
	h.RepoCount = 1
	h.Width = 120
	h.Height = 30
	h.Sessions = []*pb.Session{
		{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
		{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
		{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
	}
	h.BuildTableRows()
	// One row per session, or the cursor arithmetic below means something else.
	if got := len(h.Table.Rows()); got != len(h.Sessions) {
		t.Fatalf("fixture built %d rows for %d sessions; the cursor assertions assume one row each", got, len(h.Sessions))
	}
	if h.Table.Cursor() != 0 {
		t.Fatalf("fixture starts at cursor %d, want 0", h.Table.Cursor())
	}
	return h
}

// bindRIntoTableNavigation makes "r" a real table-navigation key for this model
// only. bubbles v2.1.1's default keymap binds no "r" (table.go:67-100, narrowed
// by bossKeyMap in tablehelpers.go), so without this the guard has nothing to
// guard against and every "the cursor did not move" assertion below would pass
// whether or not handleActionKey consumed the key. "j" is kept alongside it so
// the same model can still prove that handleKey does reach the table.
func bindRIntoTableNavigation(h Model) Model {
	h.Table.KeyMap.LineDown = key.NewBinding(key.WithKeys("j", "r"))
	return h
}

func homeFromKey(t *testing.T, model tea.Model) Model {
	t.Helper()
	h, ok := model.(Model)
	if !ok {
		t.Fatalf("handler returned %T, want views.Model", model)
	}
	return h
}

// TestHomeConsumesTheHiddenRenameKeyBeforeTheTable is the fall-through trap for
// BOS-837: "r" must be consumed by handleActionKey and must never reach
// handleTableKey, so a user who has bound "r" to navigation cannot have the
// hidden shortcut silently stolen from under the rename — or, worse, have both
// fire on one keystroke.
//
// The trap only bites because the test manufactures the collision it asserts
// on: see bindRIntoTableNavigation. The first two subtests are the controls
// that keep the third honest — one proves the rebinding is live, the other
// proves handleKey does forward ordinary keys to the table in this fixture.
//
// Falsification: delete the `case "r":` arm from handleActionKey
// (home_keys.go:143-149) and this test fails — "r is consumed before the table
// sees it" finds the cursor on row 1, and "…even when no session resolves"
// finds the table's rows wiped by the rebuild inside handleTableKey. This was
// performed once and the arm restored.
func TestHomeConsumesTheHiddenRenameKeyBeforeTheTable(t *testing.T) {
	t.Run("control: r really is table navigation in this fixture", func(t *testing.T) {
		h := bindRIntoTableNavigation(renameKeyHome(t))

		got := homeFromKey(t, mustModel(h.handleTableKey(keyPress('r'))))

		if got.Table.Cursor() != 1 {
			t.Fatalf("cursor = %d after r reached the table, want 1; the rebinding is not live, so the trap below would pass vacuously", got.Table.Cursor())
		}
	})

	t.Run("control: handleKey does forward navigation to the table", func(t *testing.T) {
		h := bindRIntoTableNavigation(renameKeyHome(t))

		got := homeFromKey(t, mustModel(h.HandleKey(keyPress('j'))))

		if got.Table.Cursor() != 1 {
			t.Fatalf("cursor = %d after j, want 1; handleKey is not reaching the table at all, so the trap below would pass vacuously", got.Table.Cursor())
		}
	})

	t.Run("r is consumed before the table sees it", func(t *testing.T) {
		h := bindRIntoTableNavigation(renameKeyHome(t))

		got := homeFromKey(t, mustModel(h.HandleKey(keyPress('r'))))

		if got.Table.Cursor() != 0 {
			t.Fatalf("cursor = %d after r, want 0; the key fell through to the table's navigation binding", got.Table.Cursor())
		}
		if !got.Rename.Active() {
			t.Fatal("r did not open the rename editor")
		}
		if id := got.Rename.SessionID(); id != "sess-1" {
			t.Fatalf("rename opened on %q, want the selected session sess-1", id)
		}
		if v := got.Rename.Value(); v != "Add dark mode" {
			t.Fatalf("rename pre-filled with %q, want the selected session's title", v)
		}
	})

	t.Run("r is consumed even when no session resolves", func(t *testing.T) {
		h := bindRIntoTableNavigation(renameKeyHome(t))
		// Rows stay built while the sessions behind them go away, so
		// sessionIndexForTableCursor (home_sessions.go:239-252) finds nothing
		// under the cursor. This is the path handleRenameStartKey no-ops on —
		// and the one where forgetting handled=true would leak the key.
		h.Sessions = nil
		if h.SelectedSession() != nil {
			t.Fatal("fixture still resolves a session; this subtest needs the no-selection state")
		}

		model, cmd := h.HandleKey(keyPress('r'))
		got := homeFromKey(t, model)

		if got.Rename.Active() {
			t.Fatal("rename opened with no session selected")
		}
		if cmd != nil {
			t.Fatal("a no-op rename scheduled a command")
		}
		if got.Table.Cursor() != 0 {
			t.Fatalf("cursor = %d after r, want 0; the key fell through to the table", got.Table.Cursor())
		}
		// handleTableKey ends in buildTableRows, which drops every row when
		// there are no sessions. Surviving rows are proof it never ran.
		if got := len(got.Table.Rows()); got != 3 {
			t.Fatalf("table holds %d rows, want the 3 it started with; handleTableKey rebuilt them, so r reached the table", got)
		}
	})

	t.Run("r is consumed while the logout confirmation is up", func(t *testing.T) {
		h := bindRIntoTableNavigation(renameKeyHome(t))
		h.Confirm = ui.NewConfirmPrompt("Log out?", func() tea.Msg { return nil })

		got := homeFromKey(t, mustModel(h.HandleKey(keyPress('r'))))

		if got.Rename.Active() {
			t.Fatal("r opened a rename underneath the logout confirmation")
		}
		if got.Table.Cursor() != 0 {
			t.Fatalf("cursor = %d, want 0; the confirmation must swallow every key", got.Table.Cursor())
		}
	})

	t.Run("r is consumed on the empty board", func(t *testing.T) {
		h := bindRIntoTableNavigation(renameKeyHome(t))
		h.RepoCount = 0
		h.Sessions = nil
		h.BuildTableRows()

		model, cmd := h.HandleKey(keyPress('r'))
		got := homeFromKey(t, model)

		if got.Rename.Active() {
			t.Fatal("r opened a rename on a board with no repositories")
		}
		if cmd != nil {
			t.Fatal("r scheduled a command on a board with no repositories")
		}
	})
}

// TestHomeKeysAreUnchangedWhenNoRenameIsActive is the regression half of
// BOS-837: adding a swallow-everything editor to handleKey is exactly the kind
// of change that quietly costs an existing binding, so every key Home already
// answered is re-asserted here against the real bossKeyMap.
func TestHomeKeysAreUnchangedWhenNoRenameIsActive(t *testing.T) {
	t.Run("navigation", func(t *testing.T) {
		h := renameKeyHome(t)

		h = homeFromKey(t, mustModel(h.HandleKey(keyPress('j'))))
		if h.Table.Cursor() != 1 {
			t.Fatalf("cursor = %d after j, want 1", h.Table.Cursor())
		}
		h = homeFromKey(t, mustModel(h.HandleKey(keyPress('k'))))
		if h.Table.Cursor() != 0 {
			t.Fatalf("cursor = %d after k, want 0", h.Table.Cursor())
		}
		h = homeFromKey(t, mustModel(h.HandleKey(keyPress('G'))))
		if h.Table.Cursor() != 2 {
			t.Fatalf("cursor = %d after G, want the last row 2", h.Table.Cursor())
		}
		h = homeFromKey(t, mustModel(h.HandleKey(specialKeyPress(tea.KeyHome))))
		if h.Table.Cursor() != 0 {
			t.Fatalf("cursor = %d after home, want 0", h.Table.Cursor())
		}
		h = homeFromKey(t, mustModel(h.HandleKey(specialKeyPress(tea.KeyEnd))))
		if h.Table.Cursor() != 2 {
			t.Fatalf("cursor = %d after end, want the last row 2", h.Table.Cursor())
		}
	})

	t.Run("actions", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			key  tea.KeyMsg
			want ui.SwitchViewMsg
		}{
			{"n opens the new-session wizard", keyPress('n'), ui.SwitchViewMsg{View: ui.ViewNewSession}},
			{"s opens settings", keyPress('s'), ui.SwitchViewMsg{View: ui.ViewSettings}},
			{"l opens login", keyPress('l'), ui.SwitchViewMsg{View: ui.ViewLogin}},
			{"enter opens the chat picker", specialKeyPress(tea.KeyEnter), ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "sess-1"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := renameKeyHome(t)
				h.authMgr = &auth.Manager{}

				_, cmd := h.HandleKey(tc.key)

				if cmd == nil {
					t.Fatalf("%s scheduled no command", tc.name)
				}
				got, ok := cmd().(ui.SwitchViewMsg)
				if !ok {
					t.Fatalf("%s produced %T, want switchViewMsg", tc.name, cmd())
				}
				if got.View != tc.want.View || got.SessionID != tc.want.SessionID {
					t.Fatalf("%s switched to %+v, want %+v", tc.name, got, tc.want)
				}
			})
		}
	})

	t.Run("q still quits", func(t *testing.T) {
		h := renameKeyHome(t)

		_, cmd := h.HandleKey(keyPress('q'))

		if cmd == nil {
			t.Fatal("q scheduled no command")
		}
		if msg := cmd(); !isQuitMsg(msg) {
			t.Fatalf("q produced %T, want tea.QuitMsg", msg)
		}
	})
}

// TestHomeRenameSwallowsQuit is the unit-level half of the q hazard: handleKey
// tests the rename before its bare `q => tea.Quit` arm, so "q" is an ordinary
// character in a title. The evidence that the *binary* survives it is the
// integration test in services/boss/e2e — a model that returns no
// quit command cannot show that a real boss process stayed alive.
func TestHomeRenameSwallowsQuit(t *testing.T) {
	h := renameKeyHome(t)
	h = homeFromKey(t, mustModel(h.HandleKey(keyPress('r'))))

	model, cmd := h.HandleKey(keyPress('q'))
	got := homeFromKey(t, model)

	if cmd != nil {
		if msg := cmd(); isQuitMsg(msg) {
			t.Fatal("q quit boss while renaming; a title containing q would kill the TUI")
		}
	}
	if !got.Rename.Active() {
		t.Fatal("q closed the rename editor")
	}
	if v := got.Rename.Value(); v != "Add dark modeq" {
		t.Fatalf("rename value = %q, want the q typed into the title", v)
	}
}

func isQuitMsg(msg tea.Msg) bool {
	_, ok := msg.(tea.QuitMsg)
	return ok
}

// mustModel adapts the (tea.Model, tea.Cmd) handlers for the assertions that
// only care about the model.
func mustModel(model tea.Model, _ tea.Cmd) tea.Model { return model }

// TestHomeRenameClosesWhenThePollHidesItsScreen pairs handleKey's precedence
// with View's branching. handleKey routes every key into an active prompt no
// matter which branch is rendering, but only renderSessionTable draws the
// rename footer — so a poll that switches Home to the daemon-error or
// empty-state screen while the editor is open would leave an invisible prompt
// swallowing keys, including the [q] both of those screens advertise. The user
// would be stuck on a screen whose own instructions do nothing.
//
// Falsification: delete the `h = h.cancelRenameIfHidden()` calls from
// handleSessionListError and applySessionList and both subtests fail on "q was
// swallowed". Performed once and the calls restored.
func TestHomeRenameClosesWhenThePollHidesItsScreen(t *testing.T) {
	openRename := func(t *testing.T) Model {
		t.Helper()
		h := renameKeyHome(t)
		h = homeFromKey(t, mustModel(h.HandleKey(keyPress('r'))))
		if !h.Rename.Active() {
			t.Fatal("fixture failed to open the rename editor")
		}
		return h
	}
	// quitsOnQ is the property that matters: not merely that the prompt closed,
	// but that the key the visible screen advertises works again.
	quitsOnQ := func(t *testing.T, h Model) {
		t.Helper()
		if h.Rename.Active() {
			t.Fatal("the rename editor survived onto a screen that does not draw it")
		}
		_, cmd := h.HandleKey(keyPress('q'))
		if cmd == nil || !isQuitMsg(cmd()) {
			t.Fatal("q was swallowed by the hidden rename editor; the screen's own [q] hint is dead")
		}
	}

	t.Run("the daemon-error screen takes over", func(t *testing.T) {
		h := openRename(t)
		// One failure short of the threshold keeps the table on screen, so the
		// editor is still visible and must stay open.
		for range pollFailureThreshold - 1 {
			h = homeFromKey(t, mustModel(h.Update(SessionListMsg{Err: errors.New("daemon unavailable")})))
		}
		if !h.Rename.Active() {
			t.Fatal("a debounced poll failure closed the editor while the board was still rendered")
		}
		h = homeFromKey(t, mustModel(h.Update(SessionListMsg{Err: errors.New("daemon unavailable")})))
		if h.Err == nil {
			t.Fatal("fixture never reached the daemon-error screen")
		}
		quitsOnQ(t, h)
	})

	t.Run("the board empties out", func(t *testing.T) {
		h := openRename(t)
		h = homeFromKey(t, mustModel(h.Update(SessionListMsg{Sessions: []*pb.Session{}})))
		if len(h.Sessions) != 0 {
			t.Fatalf("fixture still holds %d sessions; the empty-state branch was never reached", len(h.Sessions))
		}
		quitsOnQ(t, h)
	})
}

// TestHomeHiddenTrackerAndGitHubKeys pins Home's two unadvertised shortcuts:
// t opens the tracker issue and g opens the PR, and neither appears in the
// footer, whatever attention shape the row has.
func TestHomeHiddenTrackerAndGitHubKeys(t *testing.T) {
	var opened []string
	original := OpenURLFunc
	OpenURLFunc = func(url string) error { opened = append(opened, url); return nil }
	t.Cleanup(func() { OpenURLFunc = original })
	const prURL, issueURL = "https://github.com/org/repo/pull/42", "https://linear.app/team/issue/BOS-1"
	mergeable := true
	inputs := map[string]attention.Input{
		"fix":         {Fix: &attention.FixFact{AlertKey: "alert", Sentence: "broken"}},
		"question":    {QuestionText: "which one?"},
		"review":      {ParkCodes: []string{"human-review:ui"}},
		"blocked":     {ParkCodes: []string{"ledger-open"}},
		"merge":       {HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable, Verified: true},
		"unverified":  {HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable},
		"in progress": {ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
	}
	for kind, in := range inputs {
		in.HasTrackerLink = true
		s := &pb.Session{Id: "s", Title: "session", PrNumber: proto.Int32(42), PrUrl: proto.String(prURL), TrackerUrl: proto.String(issueURL), Attention: attention.Compute(in)}
		home := func() Model {
			h := attentionHome(&attentionStub{}, s)
			h.authMgr, h.LoggedIn = &auth.Manager{}, true
			return h
		}

		footer := ansi.Strip(home().RenderSessionTableFooter())
		if strings.Contains(footer, "[t]") || strings.Contains(footer, "[g]") {
			t.Errorf("%s: footer advertises a hidden key: %q", kind, footer)
		}
		if !strings.Contains(footer, "[l]ogout") {
			t.Errorf("%s: footer lost [l]ogout: %q", kind, footer)
		}

		for key, want := range map[rune]string{'t': issueURL, 'g': prURL} {
			opened = nil
			model, cmd := home().Update(keyPress(key))
			if cmd == nil {
				t.Fatalf("%s: %c scheduled nothing", kind, key)
			}
			model, _ = model.Update(cmd())
			if !slices.Equal(opened, []string{want}) {
				t.Errorf("%s: %c opened %v, want %s", kind, key, opened, want)
			}
			if got := model.(Model); got.StatusErr() {
				t.Errorf("%s: %c reported an error: %s", kind, key, got.Status)
			}
		}
	}

	t.Run("g opens the PR of a row with no attention", func(t *testing.T) {
		opened = nil
		h := renameKeyHome(t)
		h.Sessions[0].PrUrl = proto.String(prURL)
		_, cmd := h.HandleKey(keyPress('g'))
		if cmd == nil {
			t.Fatal("g scheduled nothing on a row with a PR")
		}
		cmd()
		if !slices.Equal(opened, []string{prURL}) {
			t.Fatalf("g opened %v, want %s", opened, prURL)
		}
	})

	t.Run("control: the real table keymap binds g to go-to-top", func(t *testing.T) {
		h := renameKeyHome(t)
		h.Table.SetCursor(2)
		got := homeFromKey(t, mustModel(h.handleTableKey(keyPress('g'))))
		if got.Table.Cursor() != 0 {
			t.Fatalf("cursor = %d after g reached the table, want 0; without this binding the no-op case below passes vacuously", got.Table.Cursor())
		}
	})

	t.Run("g on a row without a PR does nothing and never reaches the table", func(t *testing.T) {
		opened = nil
		h := renameKeyHome(t)
		h.Table.SetCursor(2)
		model, cmd := h.HandleKey(keyPress('g'))
		got := homeFromKey(t, model)
		if cmd != nil {
			t.Fatal("g scheduled a command on a row without a PR")
		}
		if got.Table.Cursor() != 2 {
			t.Fatalf("cursor = %d after g, want 2; g fell through to the table's go-to-top", got.Table.Cursor())
		}
		if len(opened) != 0 || got.Status != "" {
			t.Fatalf("g opened %v with status %q on a row without a PR", opened, got.Status)
		}
	})
}

func TestHomeEnterSettingUpSession(t *testing.T) {
	h := renameKeyHome(t)
	h.Sessions[0].DisplaySettingUp = true
	_, cmd := h.handleSelectKey()
	if cmd == nil {
		t.Fatal("no setup navigation")
	}
	got := cmd().(ui.SwitchViewMsg)
	if got.View != ui.ViewNewSession || got.SessionID != "sess-1" {
		t.Fatalf("navigation=%+v", got)
	}
}
