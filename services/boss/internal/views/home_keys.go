// Home's key handling: the precedence chain from the logout confirmation down
// to table navigation, and the per-action handlers it dispatches to. Split out
// of home.go (BOS-526) so the file that owns HomeModel is not also the file
// every keybinding change has to touch — home.go sees ~76 commits a quarter and
// keybindings are the most common reason to open a view.

package views

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
)

// authChangeQueue preserves the order in which local auth transitions finish
// when their best-effort daemon notifications run asynchronously. A stalled
// logout must reach bossd before a later login, or the stale logout can clear
// the daemon's newly refreshed credentials.
type authChangeQueue struct {
	mu                  sync.Mutex
	tail                <-chan struct{}
	notificationTimeout time.Duration
}

const defaultAuthChangeNotificationTimeout = 5 * time.Second

func newAuthChangeQueue() *authChangeQueue {
	done := make(chan struct{})
	close(done)
	return &authChangeQueue{
		tail:                done,
		notificationTimeout: defaultAuthChangeNotificationTimeout,
	}
}

func (q *authChangeQueue) notify(ctx context.Context, c client.BossClient, action string) tea.Cmd {
	if q == nil {
		return func() tea.Msg {
			notifyAuthChange(ctx, c, action, defaultAuthChangeNotificationTimeout)
			return nil
		}
	}

	q.mu.Lock()
	previous := q.tail
	done := make(chan struct{})
	q.tail = done
	timeout := q.notificationTimeout
	q.mu.Unlock()

	return func() tea.Msg {
		defer close(done)
		<-previous
		notifyAuthChange(ctx, c, action, timeout)
		return nil
	}
}

// notifyAuthChange nudges the daemon and deliberately drops both return values.
//
// The daemon's post-login verdict is rendered by `boss login`, which owns the
// terminal at the moment of the login and can print a warning the operator will
// actually read. This path is the TUI's own logout/login bookkeeping, running as
// a background tea.Cmd with no output surface of its own; surfacing the verdict
// here would mean routing it into the view's message loop, which is out of scope
// for BOS-945. Dropping it keeps today's behaviour rather than half-reporting it.
func notifyAuthChange(ctx context.Context, c client.BossClient, action string, timeout time.Duration) {
	notifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = c.NotifyAuthChange(notifyCtx, action)
}

// handleKey walks the same precedence the inline switch had: the logout
// confirmation swallows everything, then quit, then the upgrade/restart
// modals, then the action keys, and finally table navigation. Each helper
// reports whether it consumed the key so the fall-through order is preserved.
//
// The rule for the handlers dispatched from here: one that always consumes its
// message returns tea.Model, because handleKey tail-calls it; one that can
// DECLINE returns HomeModel plus handled, and the caller threads the model back
// into h. Returning tea.Model from a declining handler would let a mutation on
// the fall-through path vanish silently, which is why cmd and handled are
// declared up front and assigned with plain "=" — a short declaration inside an
// if-statement would scope a fresh h to that block and reintroduce exactly that
// bug with no test able to see it. The leaf handlers below handleActionKey
// (handleNewSessionKey, handleLoginKey, handleSelectKey) always consume, but
// return HomeModel too so handleActionKey can pass it back up concretely.
//
// The declining handlers' cmd is deliberately nil: declining means "not my
// key", so there is nothing to schedule and the next stage's cmd is the only
// one. A declining handler that ever needs to schedule work must batch it here
// rather than rely on this.
func (h HomeModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if h.confirm.active {
		return h.handleConfirmKey(msg)
	}
	// The rename editor swallows everything, and must be tested BEFORE the quit
	// arm below: "q" is an ordinary letter in a session title, and a rename
	// prompt that exits the whole TUI on it would be unusable.
	if h.rename.Active() {
		return h.handleRenameKey(msg)
	}
	if msg.String() == "q" {
		return h, tea.Quit
	}
	if h.upgrading || h.restarting {
		return h, nil
	}
	var cmd tea.Cmd
	var handled bool
	h, cmd, handled = h.handleUpgradeKey(msg)
	if handled {
		return h, cmd
	}
	h, cmd, handled = h.handleActionKey(msg)
	if handled {
		return h, cmd
	}
	return h.handleTableKey(msg)
}

func (h HomeModel) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	confirmed := msg.String() == "y" || msg.String() == "enter"
	var cmd tea.Cmd
	h.confirm, cmd = h.confirm.update(msg)
	if confirmed && cmd != nil && h.loggedIn {
		h.logoutPending = true
	}
	return h, cmd
}

// handleActionKey routes the top-level action keys. handled=false means the key
// is navigation and belongs to the table, and leaves the model untouched.
func (h HomeModel) handleActionKey(msg tea.KeyMsg) (HomeModel, tea.Cmd, bool) {
	switch msg.String() {
	case "n":
		model, cmd := h.handleNewSessionKey()
		return model, cmd, true
	case "s":
		return h, func() tea.Msg { return switchViewMsg{view: ViewSettings} }, true
	case "l":
		model, cmd := h.handleLoginKey()
		return model, cmd, true
	case "enter":
		model, cmd := h.handleSelectKey()
		return model, cmd, true
	case "r":
		// Hidden shortcut (BOS-837): deliberately absent from the action bar,
		// like the chat picker's own [r]. handled is true on every path,
		// including the no-session-selected one — "r" must never reach the
		// table, where a user rebinding could turn it into navigation.
		model, cmd := h.handleRenameStartKey()
		return model, cmd, true
	}
	return h, nil, false
}

// handleRenameStartKey opens the inline title editor on the selected session.
// With nothing selected it is a no-op that still counts as handled.
func (h HomeModel) handleRenameStartKey() (HomeModel, tea.Cmd) {
	sess := h.selectedSession()
	if sess == nil {
		return h, nil
	}
	var cmd tea.Cmd
	h.rename, cmd = newRenamePrompt(sess.GetId(), sess.GetTitle())
	// Drop the previous rename's outcome. Nothing else clears the status line,
	// and a stale "Rename failed" sitting above a freshly opened editor reads
	// as a complaint about the edit in progress.
	h.status, h.statusErr = "", false
	// The prompt replaces the action bar, which is one line shorter than the
	// editor, so the table has to give a row back or the board overflows. The
	// cleared status may free another; tableHeight reads both.
	h.table.SetHeight(h.tableHeight())
	return h, cmd
}

// handleRenameKey owns the two keys that end the edit; everything else is text.
func (h HomeModel) handleRenameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return h.commitRename()
	case "esc":
		return h.cancelRename()
	}
	var cmd tea.Cmd
	h.rename, cmd = h.rename.Update(msg)
	return h, cmd
}

// commitRename issues the write for the session the prompt was opened on. An
// empty title is refused in place — the prompt stays open with the typed text
// intact and no RPC is sent, because a blank title would leave the row with
// nothing to identify it by.
func (h HomeModel) commitRename() (tea.Model, tea.Cmd) {
	title := h.rename.Value()
	if title == "" {
		h.rename = h.rename.withEmptyTitleError()
		return h, nil
	}
	sessionID := h.rename.SessionID()
	h.rename = renamePrompt{}
	h.table.SetHeight(h.tableHeight())
	return h, renameSessionCmd(h.client, h.ctx, sessionID, title)
}

// cancelRename closes the editor and discards the edit.
func (h HomeModel) cancelRename() (tea.Model, tea.Cmd) {
	h.rename = renamePrompt{}
	h.table.SetHeight(h.tableHeight())
	return h, nil
}

// cancelRenameIfHidden closes the editor when a poll has moved Home onto a
// screen that does not draw it. handleKey routes every key into an active
// prompt regardless of which View branch is rendering, but only
// renderSessionTable draws the footer — so an editor left open behind the
// daemon-error or empty-state screen would keep swallowing keys, including the
// [q] those screens advertise, with nothing on screen to explain why. The
// conditions below mirror View's branch order exactly; they must be kept in
// step with it.
func (h HomeModel) cancelRenameIfHidden() HomeModel {
	if !h.rename.Active() {
		return h
	}
	if h.err == nil && !h.loading && len(h.sessions) > 0 {
		return h
	}
	h.rename = renamePrompt{}
	h.table.SetHeight(h.tableHeight())
	return h
}

func (h HomeModel) handleNewSessionKey() (HomeModel, tea.Cmd) {
	if h.repoCount == 0 {
		return h, nil
	}
	return h, func() tea.Msg { return switchViewMsg{view: ViewNewSession} }
}

func (h HomeModel) handleLoginKey() (HomeModel, tea.Cmd) {
	if h.authMgr == nil {
		return h, nil
	}
	if h.logoutPending {
		return h, nil
	}
	if h.loggedIn {
		authMgr := h.authMgr
		c := h.client
		ctx := h.ctx
		label := "Log out?"
		if h.loggedInEmail != "" {
			label = fmt.Sprintf("Log out %s?", h.loggedInEmail)
		}
		h.confirm = newConfirmPrompt(label, func() tea.Msg {
			var err error
			if authMgr != nil {
				err = authMgr.Logout(ctx)
			}
			if err != nil || c == nil {
				return logoutMsg{err: err}
			}
			return logoutMsg{
				notify: h.authChanges.notify(ctx, c, "logout"),
			}
		})
		return h, nil
	}
	return h, func() tea.Msg { return switchViewMsg{view: ViewLogin} }
}

func (h HomeModel) handleSelectKey() (HomeModel, tea.Cmd) {
	if h.repoCount == 0 {
		// New user with no repos: guide them into adding their first
		// repository. firstRepo=true makes the add wizard return to the
		// home empty state on cancel rather than the repo list.
		return h, func() tea.Msg { return switchViewMsg{view: ViewRepoAdd, firstRepo: true} }
	}
	if sess := h.selectedSession(); sess != nil {
		return h, func() tea.Msg {
			return switchViewMsg{view: ViewChatPicker, sessionID: sess.Id}
		}
	}
	return h, nil
}

func (h HomeModel) handleTableKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Forward navigation keys to the table.
	var cmd tea.Cmd
	previousCursor := h.table.Cursor()
	h.table, cmd = h.table.Update(msg)
	h.normalizeTableCursor(previousCursor)
	// Rebuild so the selection (blue) styling follows the new cursor; this
	// also re-runs updateCursorColumn internally to keep the chevron in
	// sync. updateCursorColumn alone would only move the chevron, leaving
	// repo/name/pr stale on error/attention rows (BOS-103).
	h.buildTableRows()
	return h, cmd
}
