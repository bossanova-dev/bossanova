// Home's key handling: the precedence chain from the logout confirmation down
// to table navigation, and the per-action handlers it dispatches to. Split out
// of home.go (BOS-526) so the file that owns HomeModel is not also the file
// every keybinding change has to touch — home.go sees ~76 commits a quarter and
// keybindings are the most common reason to open a view.

package views

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

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
	if confirmed && cmd != nil {
		h.loggedIn = false
		h.loggedInEmail = ""
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
	}
	return h, nil, false
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
			if err == nil {
				_ = c.NotifyAuthChange(ctx, "logout")
			}
			return logoutMsg{err: err}
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
