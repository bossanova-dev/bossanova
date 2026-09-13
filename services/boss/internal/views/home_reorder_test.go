// BOS-1231: the hidden alt+up / alt+down reorder chords on the home session
// list. The discriminating rule these tests exist to pin is that a key press
// either MOVES the selected session or it NAVIGATES — never both, and never the
// wrong one. The table's own keymap owns the unmodified arrows, so every
// assertion below is written against the id sequence and the selected session
// id rather than a row index.

package views

import (
	"context"
	"errors"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// moveStubClient records every reorder the chords issue. It layers on
// stubSessionSettingsClient, whose other methods panic, so an unexpected RPC
// from Home fails loudly rather than being quietly absorbed.
type moveStubClient struct {
	*stubSessionSettingsClient
	reqs  []*pb.MoveSessionRequest
	moved bool
	err   error
}

var _ client.BossClient = (*moveStubClient)(nil)

func (c *moveStubClient) MoveSession(_ context.Context, req *pb.MoveSessionRequest) (*pb.Session, bool, error) {
	c.reqs = append(c.reqs, req)
	if c.err != nil {
		return nil, false, c.err
	}
	return &pb.Session{Id: req.GetId()}, c.moved, nil
}

func newMoveStubClient() *moveStubClient {
	return &moveStubClient{stubSessionSettingsClient: &stubSessionSettingsClient{}, moved: true}
}

// reorderHome is a three-session board on which the FIRST session renders a
// waiting sub-row. The sub-row is the point of the fixture, not decoration: it
// makes the table's row indices diverge from the session indices (rows are
// sess-1, sess-1's waiting hint, sess-2, sess-3), so an implementation that
// moved or re-pinned the cursor by row number lands on the wrong session and
// the cursor assertions below fail.
func reorderHome(t *testing.T) (HomeModel, *moveStubClient) {
	t.Helper()
	stub := newMoveStubClient()
	h := NewHomeModel(stub, context.Background(), nil)
	h.loading = false
	h.repoCount = 1
	h.width = 120
	h.height = 30
	h.sessions = []*pb.Session{
		{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
		{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
		{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
	}
	h.daemonWaitingReasons = map[string]string{
		"sess-1": "awaiting checks_passed_ready on acme/widget#123",
	}
	h.buildTableRows()
	if got, want := len(h.table.Rows()), len(h.sessions)+1; got != want {
		t.Fatalf("fixture built %d rows for %d sessions, want %d; the sub-row that makes "+
			"row indices diverge from session indices is missing, so the cursor assertions "+
			"would pass against a row-index implementation", got, len(h.sessions), want)
	}
	return h, stub
}

// sessionIDs is what every reorder assertion is written against: the rendered
// session order, which is the artifact the ticket is about.
func sessionIDs(h HomeModel) []string { return sessionIDOrder(h.sessions) }

func assertSessionIDs(t *testing.T, h HomeModel, want ...string) {
	t.Helper()
	got := sessionIDs(h)
	if len(got) != len(want) {
		t.Fatalf("session order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("session order = %v, want %v", got, want)
		}
	}
}

// selectSession puts the cursor on a session by id, so no test has to compute a
// row index by hand — the arithmetic under test is exactly what would be
// duplicated by doing so.
func selectSession(t *testing.T, h HomeModel, id string) HomeModel {
	t.Helper()
	row, ok := h.tableCursorForSessionID(id)
	if !ok {
		t.Fatalf("fixture has no session %q to select", id)
	}
	h.table.SetCursor(row)
	updateCursorColumn(&h.table)
	if got := h.selectedSessionID(); got != id {
		t.Fatalf("selecting %q put the cursor on %q", id, got)
	}
	return h
}

// altKeyPress builds the chord the driver's "alt+up"/"alt+down" bytes decode
// to. Asserted against KeyPressMsg.String(), which is what handleActionKey
// switches on.
func altKeyPress(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModAlt}
}

func TestAltKeyPressProducesTheChordHandleActionKeySwitchesOn(t *testing.T) {
	// Control for every test below: if the fixture's key message did not
	// stringify to the chord, the binding would never be reached and the
	// reorder assertions would be testing nothing.
	if got := altKeyPress(tea.KeyUp).String(); got != "alt+up" {
		t.Fatalf("altKeyPress(up).String() = %q, want alt+up", got)
	}
	if got := altKeyPress(tea.KeyDown).String(); got != "alt+down" {
		t.Fatalf("altKeyPress(down).String() = %q, want alt+down", got)
	}
}

// TestHomeAltUpMovesTheSelectedSessionEarlier is AC1.
func TestHomeAltUpMovesTheSelectedSessionEarlier(t *testing.T) {
	h, stub := reorderHome(t)
	h = selectSession(t, h, "sess-2")

	model, cmd := h.handleKey(altKeyPress(tea.KeyUp))
	h = homeFromKey(t, model)

	assertSessionIDs(t, h, "sess-2", "sess-1", "sess-3")
	if cmd == nil {
		t.Fatal("alt+up scheduled no move command; the RPC must run as a tea.Cmd, not inline in Update")
	}
	msg, ok := cmd().(sessionMovedMsg)
	if !ok {
		t.Fatalf("the move produced %T, want sessionMovedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("move reported %v", msg.err)
	}
	if len(stub.reqs) != 1 {
		t.Fatalf("alt+up issued %d MoveSession calls, want exactly 1", len(stub.reqs))
	}
	if got := stub.reqs[0].GetId(); got != "sess-2" {
		t.Fatalf("MoveSession moved %q, want the selected session sess-2", got)
	}
	if got := stub.reqs[0].GetDirection(); got != pb.MoveDirection_MOVE_DIRECTION_UP {
		t.Fatalf("MoveSession direction = %v, want UP", got)
	}
	if stub.reqs[0].RepoId != nil {
		t.Fatalf("MoveSession scoped to repo %q; Home renders the unfiltered cross-repo list, "+
			"so scoping it would make the daemon reason about different neighbours than the screen shows",
			stub.reqs[0].GetRepoId())
	}
}

// TestHomeAltDownMovesTheSelectedSessionLater is AC2.
func TestHomeAltDownMovesTheSelectedSessionLater(t *testing.T) {
	h, stub := reorderHome(t)
	h = selectSession(t, h, "sess-2")

	model, cmd := h.handleKey(altKeyPress(tea.KeyDown))
	h = homeFromKey(t, model)

	assertSessionIDs(t, h, "sess-1", "sess-3", "sess-2")
	if cmd == nil {
		t.Fatal("alt+down scheduled no move command")
	}
	if _, ok := cmd().(sessionMovedMsg); !ok {
		t.Fatalf("the move produced %T, want sessionMovedMsg", cmd())
	}
	if len(stub.reqs) != 1 {
		t.Fatalf("alt+down issued %d MoveSession calls, want exactly 1", len(stub.reqs))
	}
	if got := stub.reqs[0].GetDirection(); got != pb.MoveDirection_MOVE_DIRECTION_DOWN {
		t.Fatalf("MoveSession direction = %v, want DOWN", got)
	}
}

// TestHomeBareNavigationKeysNeverReorder is AC3: the unmodified arrows and the
// vim keys must keep doing exactly what they do today. The cursor assertion is
// the control — without it a binding that swallowed navigation entirely would
// also leave the id sequence unchanged and pass vacuously.
func TestHomeBareNavigationKeysNeverReorder(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
		want string // session under the cursor afterwards
	}{
		{"down", specialKeyPress(tea.KeyDown), "sess-3"},
		{"j", keyPress('j'), "sess-3"},
		{"up", specialKeyPress(tea.KeyUp), "sess-1"},
		{"k", keyPress('k'), "sess-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, stub := reorderHome(t)
			h = selectSession(t, h, "sess-2")

			h = homeFromKey(t, mustModel(h.handleKey(tc.key)))

			assertSessionIDs(t, h, "sess-1", "sess-2", "sess-3")
			if got := h.selectedSessionID(); got != tc.want {
				t.Fatalf("%s left the cursor on %q, want %q; the key no longer navigates", tc.name, got, tc.want)
			}
			if len(stub.reqs) != 0 {
				t.Fatalf("%s issued %d MoveSession calls; a bare navigation key must never reorder", tc.name, len(stub.reqs))
			}
		})
	}
}

// bindAltArrowsIntoTableNavigation makes the chords real table-navigation keys
// for this model only — the technique home_keys_test.go uses for "r". bubbles'
// default keymap binds no alt+arrow, so without this the guard has nothing to
// guard against and "the order did not change" would pass whether or not
// handleActionKey consumed the chord.
func bindAltArrowsIntoTableNavigation(h HomeModel) HomeModel {
	h.table.KeyMap.LineDown = key.NewBinding(key.WithKeys("j", "alt+up", "alt+down"))
	return h
}

// TestHomeConsumesTheReorderChordsBeforeTheTable is AC4, and the fall-through
// trap: the chord must be consumed by handleActionKey and must never reach
// handleTableKey, where a user's rebinding could turn a reorder back into
// navigation — or, worse, fire both on one keystroke.
//
// Falsification: delete the `case "alt+up":` / `case "alt+down":` arms from
// handleActionKey and the third and fourth subtests fail. Performed once and
// the arms restored.
func TestHomeConsumesTheReorderChordsBeforeTheTable(t *testing.T) {
	t.Run("control: the chord really is table navigation in this fixture", func(t *testing.T) {
		h, _ := reorderHome(t)
		h = bindAltArrowsIntoTableNavigation(h)

		got := homeFromKey(t, mustModel(h.handleTableKey(altKeyPress(tea.KeyUp))))

		if got.table.Cursor() == 0 {
			t.Fatal("the cursor did not move when the chord reached the table; the rebinding is not live, so the trap below would pass vacuously")
		}
	})

	t.Run("control: handleKey does forward navigation to the table", func(t *testing.T) {
		h, _ := reorderHome(t)
		h = bindAltArrowsIntoTableNavigation(h)

		got := homeFromKey(t, mustModel(h.handleKey(keyPress('j'))))

		if got.table.Cursor() == 0 {
			t.Fatal("handleKey is not reaching the table at all, so the trap below would pass vacuously")
		}
	})

	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
		want []string
	}{
		{"alt+up is consumed before the table sees it", altKeyPress(tea.KeyUp), []string{"sess-2", "sess-1", "sess-3"}},
		{"alt+down is consumed before the table sees it", altKeyPress(tea.KeyDown), []string{"sess-1", "sess-3", "sess-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := reorderHome(t)
			h = bindAltArrowsIntoTableNavigation(h)
			h = selectSession(t, h, "sess-2")

			got := homeFromKey(t, mustModel(h.handleKey(tc.key)))

			assertSessionIDs(t, got, tc.want...)
			if id := got.selectedSessionID(); id != "sess-2" {
				t.Fatalf("cursor landed on %q, want the moved session sess-2; the chord fell through "+
					"to the table's navigation binding", id)
			}
		})
	}

	t.Run("the chord is consumed even when no session resolves", func(t *testing.T) {
		h, stub := reorderHome(t)
		h = bindAltArrowsIntoTableNavigation(h)
		// Rows stay built while the sessions behind them go away, so
		// sessionIndexForTableCursor finds nothing under the cursor. This is
		// the path moveSelectedSession no-ops on — and the one where forgetting
		// handled=true would leak the chord to the table.
		rowsBefore := len(h.table.Rows())
		h.sessions = nil
		if h.selectedSession() != nil {
			t.Fatal("fixture still resolves a session; this subtest needs the no-selection state")
		}

		model, cmd := h.handleKey(altKeyPress(tea.KeyUp))
		got := homeFromKey(t, model)

		if cmd != nil {
			t.Fatal("a no-op reorder scheduled a command")
		}
		if len(stub.reqs) != 0 {
			t.Fatal("a no-op reorder issued a MoveSession RPC")
		}
		// handleTableKey ends in buildTableRows, which drops every row when
		// there are no sessions. Surviving rows are proof it never ran.
		if n := len(got.table.Rows()); n != rowsBefore {
			t.Fatalf("table holds %d rows, want the %d it started with; handleTableKey rebuilt them, "+
				"so the chord reached the table", n, rowsBefore)
		}
	})

	t.Run("the chord is consumed while the logout confirmation is up", func(t *testing.T) {
		h, stub := reorderHome(t)
		h = selectSession(t, h, "sess-2")
		h.confirm = newConfirmPrompt("Log out?", func() tea.Msg { return nil })

		got := homeFromKey(t, mustModel(h.handleKey(altKeyPress(tea.KeyUp))))

		assertSessionIDs(t, got, "sess-1", "sess-2", "sess-3")
		if len(stub.reqs) != 0 {
			t.Fatal("the chord reordered underneath the logout confirmation")
		}
	})

	t.Run("the chord is typed into the rename editor rather than reordering", func(t *testing.T) {
		h, stub := reorderHome(t)
		h = selectSession(t, h, "sess-2")
		h = homeFromKey(t, mustModel(h.handleKey(keyPress('r'))))
		if !h.rename.Active() {
			t.Fatal("fixture failed to open the rename editor")
		}

		got := homeFromKey(t, mustModel(h.handleKey(altKeyPress(tea.KeyUp))))

		assertSessionIDs(t, got, "sess-1", "sess-2", "sess-3")
		if len(stub.reqs) != 0 {
			t.Fatal("the chord reordered the board while a rename was open")
		}
	})
}

// TestHomeReorderKeepsTheCursorOnTheMovedSession is AC5. The assertion is on
// the SELECTED SESSION ID; the accompanying row-number check is what proves the
// fixture's sub-row makes the two diverge, so an implementation that pinned the
// cursor by row index could not pass this by accident.
func TestHomeReorderKeepsTheCursorOnTheMovedSession(t *testing.T) {
	h, _ := reorderHome(t)
	h = selectSession(t, h, "sess-2")
	rowBefore := h.table.Cursor()

	h = homeFromKey(t, mustModel(h.handleKey(altKeyPress(tea.KeyUp))))

	if got := h.selectedSessionID(); got != "sess-2" {
		t.Fatalf("cursor landed on %q after the move, want the session that moved (sess-2)", got)
	}
	rowAfter := h.table.Cursor()
	if rowAfter == rowBefore {
		t.Fatalf("the moved session is still on row %d, so this test would also pass against an "+
			"implementation that never re-pinned the cursor; the fixture's sub-row is supposed to "+
			"make the row index change", rowBefore)
	}
	// And the row the cursor now sits on really is that session's primary row,
	// not a sub-row that merely normalizes back to it.
	if want, ok := h.tableCursorForSessionID("sess-2"); !ok || want != rowAfter {
		t.Fatalf("cursor row = %d, want sess-2's primary row %d", rowAfter, want)
	}
}

// TestHomeReorderAtTheBoundaryIsANoop is AC6: the first session cannot move up
// and the last cannot move down. Neither the order nor the cursor changes, no
// RPC is sent, and nothing is reported on the status line — a held-down key
// must not start failing.
func TestHomeReorderAtTheBoundaryIsANoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		onID string
		key  tea.KeyMsg
	}{
		{"alt+up on the first session", "sess-1", altKeyPress(tea.KeyUp)},
		{"alt+down on the last session", "sess-3", altKeyPress(tea.KeyDown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, stub := reorderHome(t)
			h = selectSession(t, h, tc.onID)

			model, cmd := h.handleKey(tc.key)
			got := homeFromKey(t, model)

			assertSessionIDs(t, got, "sess-1", "sess-2", "sess-3")
			if id := got.selectedSessionID(); id != tc.onID {
				t.Fatalf("cursor moved to %q, want %q; a boundary move must not move the cursor either", id, tc.onID)
			}
			if cmd != nil {
				t.Fatalf("a boundary move scheduled %T; the local list already says it is a no-op, "+
					"so a held-down key must not issue a round trip per repeat", cmd())
			}
			if len(stub.reqs) != 0 {
				t.Fatalf("a boundary move issued %d MoveSession calls", len(stub.reqs))
			}
			if got.status != "" {
				t.Fatalf("a boundary move reported %q on the status line; it is a successful no-op, not an error", got.status)
			}
			if got.statusErr {
				t.Fatal("a boundary move set the failure colour on the status line")
			}
		})
	}
}

// TestHomeReorderSurvivesThePoll pins the optimistic override. The list is
// polled every ~2s and reassigned wholesale, so without the override the moved
// row snaps back to the daemon's order for up to a full interval and then jumps
// again when the write lands.
func TestHomeReorderSurvivesThePoll(t *testing.T) {
	serverOrder := func() []*pb.Session {
		return []*pb.Session{
			{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
			{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
			{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
		}
	}

	t.Run("a poll carrying the pre-move order does not undo the move", func(t *testing.T) {
		h, _ := reorderHome(t)
		h = selectSession(t, h, "sess-2")
		h = homeFromKey(t, mustModel(h.handleKey(altKeyPress(tea.KeyUp))))

		// The move RPC has not resolved yet: this poll was already in flight.
		h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: serverOrder()})))

		assertSessionIDs(t, h, "sess-2", "sess-1", "sess-3")
		if id := h.selectedSessionID(); id != "sess-2" {
			t.Fatalf("the poll moved the cursor to %q, want sess-2", id)
		}
	})

	t.Run("the override is dropped once the daemon serves the same order", func(t *testing.T) {
		h, _ := reorderHome(t)
		h = selectSession(t, h, "sess-2")
		model, cmd := h.handleKey(altKeyPress(tea.KeyUp))
		h = homeFromKey(t, model)
		h = homeFromKey(t, mustModel(h.Update(cmd())))
		if h.moveInFlight != 0 {
			t.Fatalf("moveInFlight = %d after the RPC resolved, want 0", h.moveInFlight)
		}

		moved := serverOrder()
		moved[0], moved[1] = moved[1], moved[0]
		h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: moved})))

		assertSessionIDs(t, h, "sess-2", "sess-1", "sess-3")
		if h.moveOverrideOrder != nil {
			t.Fatalf("moveOverrideOrder = %v after the daemon served the same order; the override "+
				"must be dropped as soon as the server's own answer supersedes it", h.moveOverrideOrder)
		}
	})

	t.Run("a session the daemon added keeps the position the daemon gave it", func(t *testing.T) {
		h, _ := reorderHome(t)
		h = selectSession(t, h, "sess-2")
		h = homeFromKey(t, mustModel(h.handleKey(altKeyPress(tea.KeyUp))))

		incoming := append([]*pb.Session{{Id: "sess-new", Title: "Quick chat", RepoDisplayName: "bossanova"}}, serverOrder()...)
		h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: incoming})))

		// The override permutes only the slots its own ids occupy, so the new
		// session stays in the slot the daemon chose for it.
		assertSessionIDs(t, h, "sess-new", "sess-2", "sess-1", "sess-3")
	})

	t.Run("a failed move drops the override and says so", func(t *testing.T) {
		h, stub := reorderHome(t)
		stub.err = errors.New("daemon unavailable")
		h = selectSession(t, h, "sess-2")
		model, cmd := h.handleKey(altKeyPress(tea.KeyUp))
		h = homeFromKey(t, model)

		h = homeFromKey(t, mustModel(h.Update(cmd())))

		if h.moveOverrideOrder != nil {
			t.Fatalf("moveOverrideOrder = %v after a failed move, want nil", h.moveOverrideOrder)
		}
		if !h.statusErr {
			t.Fatal("a failed move was not coloured as a failure on the status line")
		}
		if h.status == "" {
			t.Fatal("a failed move reported nothing on the status line")
		}
		// The next poll is the authority, and it restores the daemon's order.
		h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: serverOrder()})))
		assertSessionIDs(t, h, "sess-1", "sess-2", "sess-3")
	})

	t.Run("a successful no-op from the daemon drops the override without an error", func(t *testing.T) {
		h, stub := reorderHome(t)
		stub.moved = false
		h = selectSession(t, h, "sess-2")
		model, cmd := h.handleKey(altKeyPress(tea.KeyUp))
		h = homeFromKey(t, model)

		h = homeFromKey(t, mustModel(h.Update(cmd())))

		if h.moveOverrideOrder != nil {
			t.Fatalf("moveOverrideOrder = %v after is_moved=false, want nil", h.moveOverrideOrder)
		}
		if h.status != "" || h.statusErr {
			t.Fatalf("is_moved=false surfaced %q as an error; it is a successful boundary no-op", h.status)
		}
	})
}

// TestHomeReorderYieldsToAMultiPositionDaemonMove is the regression for the
// stuck-override defect. The daemon's move is NOT an adjacent swap: on a board
// where nothing has been reordered yet, moving a row up past an UNRANKED
// neighbour makes it join the (empty) ranked block, and every ranked row sorts
// above every unranked one — so the THIRD row rises to the TOP, not by one. See
// db.ComputeListRankMove's `rankedCount == 0 -> singleRank(moved.ID,
// ListRankGap)` branch, and "the FALL IS MULTI-POSITION by design" for the
// mirror case on the way down. The daemon still answers is_moved=true.
//
// Painting an adjacent swap for the one frame before the poll lands is fine.
// What must not happen is the OVERRIDE outliving it: released only when the
// incoming order already equals the local swap, it can never be released after
// a multi-position move, so the board re-imposes a wrong order on every
// subsequent poll — forever, and masking every later reorder from any source.
func TestHomeReorderYieldsToAMultiPositionDaemonMove(t *testing.T) {
	daemonOrder := func() []*pb.Session {
		// sess-3 rose to the top, which is what the real daemon does here.
		return []*pb.Session{
			{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
			{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
			{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
		}
	}

	h, _ := reorderHome(t)
	h = selectSession(t, h, "sess-3")

	model, cmd := h.handleKey(altKeyPress(tea.KeyUp))
	h = homeFromKey(t, model)
	// Control: the optimistic frame really is the adjacent swap, so the
	// assertions below are about the override and not about a TUI that happened
	// to guess the daemon's answer.
	assertSessionIDs(t, h, "sess-1", "sess-3", "sess-2")

	// The RPC resolves with is_moved=true: the daemon DID move the session,
	// just not to the position the chord guessed.
	h = homeFromKey(t, mustModel(h.Update(cmd())))
	if h.moveInFlight != 0 {
		t.Fatalf("moveInFlight = %d after the RPC resolved, want 0", h.moveInFlight)
	}

	h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: daemonOrder()})))

	assertSessionIDs(t, h, "sess-3", "sess-1", "sess-2")
	if h.moveOverrideOrder != nil {
		t.Fatalf("moveOverrideOrder = %v once nothing was outstanding; an override released only "+
			"on exact agreement can never be released after a multi-position move, so it would "+
			"re-impose this stale order on every future poll", h.moveOverrideOrder)
	}

	// And it stays released, so a later reorder from any source is not masked.
	later := daemonOrder()
	later[1], later[2] = later[2], later[1]
	h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: later})))
	assertSessionIDs(t, h, "sess-3", "sess-2", "sess-1")
}

// TestHomeReorderKeepsTheOverrideWhileAnotherMoveIsOutstanding pins the guard
// on the path that CLEARS the override after a SUCCESSFUL reply. Each press
// overwrites the override with the newest full order, so a boundary no-op
// belonging to an EARLIER press must not throw away the override that describes
// a later one still queued behind it. Without the guard the next poll, which
// predates that outstanding write, snaps the row back: exactly the flicker the
// override exists to prevent.
func TestHomeReorderKeepsTheOverrideWhileAnotherMoveIsOutstanding(t *testing.T) {
	preMoveOrder := func() []*pb.Session {
		return []*pb.Session{
			{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
			{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
			{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
		}
	}

	// twoFastChords presses alt+up twice on the last session and returns the
	// model plus the FIRST press's unresolved command. The second press is still
	// outstanding behind it, and the override describes ITS result.
	twoFastChords := func(t *testing.T) (HomeModel, *moveStubClient, tea.Cmd) {
		t.Helper()
		h, stub := reorderHome(t)
		h = selectSession(t, h, "sess-3")
		model, first := h.handleKey(altKeyPress(tea.KeyUp))
		h = homeFromKey(t, model)
		model, _ = h.handleKey(altKeyPress(tea.KeyUp))
		h = homeFromKey(t, model)
		assertSessionIDs(t, h, "sess-3", "sess-1", "sess-2")
		if h.moveInFlight != 1 || len(h.movePending) != 1 {
			t.Fatalf("moveInFlight = %d with %d queued after two chords, want 1 and 1; this test "+
				"needs a second outstanding move for the first reply to be able to discard its "+
				"override", h.moveInFlight, len(h.movePending))
		}
		return h, stub, first
	}

	t.Run("a boundary no-op does not discard a queued move's override", func(t *testing.T) {
		h, stub, first := twoFastChords(t)
		stub.moved = false

		h = homeFromKey(t, mustModel(h.Update(first())))

		if h.moveOverrideOrder == nil {
			t.Fatal("a boundary no-op from an earlier press discarded the override that still " +
				"describes the queued second press")
		}
		// The poll that predates the outstanding write must not win.
		h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: preMoveOrder()})))
		assertSessionIDs(t, h, "sess-3", "sess-1", "sess-2")
	})

	t.Run("a failure drops the queue along with the override", func(t *testing.T) {
		h, stub, first := twoFastChords(t)
		stub.err = errors.New("daemon unavailable")

		model, next := h.Update(first())
		h = homeFromKey(t, model)

		if !h.statusErr || h.status == "" {
			t.Fatal("a failed move must still be reported on the status line")
		}
		if next != nil {
			t.Fatal("a failed move released the queued chord anyway; every queued move was " +
				"computed against a local order the daemon never accepted")
		}
		if len(h.movePending) != 0 {
			t.Fatalf("%d moves left queued after a failure, want none", len(h.movePending))
		}
		if h.moveOverrideOrder != nil {
			t.Fatalf("moveOverrideOrder = %v after a failure emptied the queue, want nil; the "+
				"next poll is the authority", h.moveOverrideOrder)
		}
		if len(stub.reqs) != 1 {
			t.Fatalf("%d MoveSession calls were issued, want only the one that failed", len(stub.reqs))
		}
	})
}

// TestHomeReorderSerializesMoveRequests is the regression for out-of-order
// moves. Each press used to return its own tea.Cmd, and bubbletea runs commands
// concurrently — so alt+down then alt+up, pressed faster than the round trip,
// could reach the daemon in either order. Up-first makes the first request a
// boundary no-op and the later Down then moves the session anyway, leaving a
// persisted order that contradicts the keypress sequence. One request at a time,
// released by the previous reply, is what makes the daemon see what was typed.
func TestHomeReorderSerializesMoveRequests(t *testing.T) {
	h, stub := reorderHome(t)
	h = selectSession(t, h, "sess-1")

	model, first := h.handleKey(altKeyPress(tea.KeyDown))
	h = homeFromKey(t, model)
	model, second := h.handleKey(altKeyPress(tea.KeyUp))
	h = homeFromKey(t, model)

	if first == nil {
		t.Fatal("the first chord scheduled no command")
	}
	if second != nil {
		t.Fatal("the second chord dispatched its own command while the first was still " +
			"outstanding; bubbletea runs commands concurrently, so the daemon can serve them " +
			"in either order and the persisted order can contradict the keypress sequence")
	}
	// Both presses are still reflected locally: serializing the RPC must not
	// make the board stop responding to the keystroke.
	assertSessionIDs(t, h, "sess-1", "sess-2", "sess-3")
	if id := h.selectedSessionID(); id != "sess-1" {
		t.Fatalf("cursor landed on %q, want the moved session sess-1", id)
	}

	model, next := h.Update(first())
	h = homeFromKey(t, model)
	if next == nil {
		t.Fatal("the first reply released no command; the queued second move would never be sent")
	}
	// Feed the released command's reply back in so the queued second move is
	// dispatched. The resulting model is not read again; the assertions below are
	// on the stub's recorded requests.
	_ = homeFromKey(t, mustModel(h.Update(next())))

	if len(stub.reqs) != 2 {
		t.Fatalf("the two chords issued %d MoveSession calls, want exactly 2", len(stub.reqs))
	}
	if got := stub.reqs[0].GetDirection(); got != pb.MoveDirection_MOVE_DIRECTION_DOWN {
		t.Fatalf("the first request went %v, want the first keypress's DOWN", got)
	}
	if got := stub.reqs[1].GetDirection(); got != pb.MoveDirection_MOVE_DIRECTION_UP {
		t.Fatalf("the second request went %v, want the second keypress's UP", got)
	}
}

// TestHomeReorderHoldsTheOverrideWhileAMoveIsQueued is the other half of
// serialization: a queued move has not reached the daemon at all, so a poll is
// necessarily older than its write. Releasing the override while work is still
// queued would snap the row back for the rest of the queue.
func TestHomeReorderHoldsTheOverrideWhileAMoveIsQueued(t *testing.T) {
	h, _ := reorderHome(t)
	h = selectSession(t, h, "sess-3")

	model, first := h.handleKey(altKeyPress(tea.KeyUp))
	h = homeFromKey(t, model)
	model, _ = h.handleKey(altKeyPress(tea.KeyUp))
	h = homeFromKey(t, model)
	assertSessionIDs(t, h, "sess-3", "sess-1", "sess-2")

	// The first move resolves; the second is dispatched but not yet answered.
	model, next := h.Update(first())
	h = homeFromKey(t, model)
	if next == nil {
		t.Fatal("the queued second move was never dispatched, so this test would not be " +
			"exercising the held override")
	}

	h = homeFromKey(t, mustModel(h.Update(sessionListMsg{sessions: []*pb.Session{
		{Id: "sess-1", Title: "Add dark mode", RepoDisplayName: "bossanova"},
		{Id: "sess-3", Title: "Add rate limiting", RepoDisplayName: "bossanova"},
		{Id: "sess-2", Title: "Fix login bug", RepoDisplayName: "bossanova"},
	}})))
	assertSessionIDs(t, h, "sess-3", "sess-1", "sess-2")
}

// refusingMoveStubClient answers the capability check the way RemoteClient does:
// MoveSession is not routed through the orchestrator yet (BOS-1232).
type refusingMoveStubClient struct{ *moveStubClient }

func (c *refusingMoveStubClient) CanMoveSession() bool { return false }

// TestHomeReorderRefusesBeforePaintingWhenTheClientCannotMove pins the remote
// path. RemoteClient.MoveSession is a hard Unimplemented, so an optimistic swap
// would reorder the board, report a failure, and revert on the next poll. The
// chord is still CONSUMED — it must not fall through to table navigation — it
// just says why instead of lying first.
func TestHomeReorderRefusesBeforePaintingWhenTheClientCannotMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"alt+up", altKeyPress(tea.KeyUp)},
		{"alt+down", altKeyPress(tea.KeyDown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, stub := reorderHome(t)
			h.client = &refusingMoveStubClient{moveStubClient: stub}
			h = selectSession(t, h, "sess-2")

			model, cmd := h.handleKey(tc.key)
			got := homeFromKey(t, model)

			assertSessionIDs(t, got, "sess-1", "sess-2", "sess-3")
			if cmd != nil {
				t.Fatalf("the chord scheduled %T against a client that cannot serve MoveSession", cmd())
			}
			if len(stub.reqs) != 0 {
				t.Fatalf("the chord issued %d MoveSession calls against a client that refuses them", len(stub.reqs))
			}
			if got.moveOverrideOrder != nil {
				t.Fatalf("moveOverrideOrder = %v; nothing was reordered, so nothing needs defending", got.moveOverrideOrder)
			}
			if got.status == "" || !got.statusErr {
				t.Fatalf("the refusal reported %q (err=%v); the user is owed one message saying why",
					got.status, got.statusErr)
			}
			if id := got.selectedSessionID(); id != "sess-2" {
				t.Fatalf("cursor landed on %q, want sess-2; the refused chord must not navigate either", id)
			}
		})
	}
}

// TestSessionReorderCapabilityMatchesTheClients is the control for the test
// above: the seam only defends anything if the client that actually refuses
// implements it, and only stays out of the way if the local one does not.
func TestSessionReorderCapabilityMatchesTheClients(t *testing.T) {
	remote := (*client.RemoteClient)(nil)
	capable, ok := any(remote).(sessionReorderCapable)
	if !ok {
		t.Fatal("*client.RemoteClient no longer implements sessionReorderCapable, so the TUI would " +
			"paint an optimistic reorder and only then hit MoveSession's hard Unimplemented")
	}
	if capable.CanMoveSession() {
		t.Fatal("*client.RemoteClient claims it can move sessions, but MoveSession returns Unimplemented")
	}
	if !sessionReorderAvailable((*client.LocalClient)(nil)) {
		t.Fatal("the chord is refused against a local daemon, which is the only place it works")
	}
}
