// Home's key handling: the precedence chain from the logout confirmation down
// to table navigation, and the per-action handlers it dispatches to. Split out
// of home.go (BOS-526) so the file that owns Model is not also the file
// every keybinding change has to touch — home.go sees ~76 commits a quarter and
// keybindings are the most common reason to open a view.

package home

import (
	"fmt"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// handleKey walks the same precedence the inline switch had: the logout
// confirmation swallows everything, then quit, then the upgrade/restart
// modals, then the action keys, and finally table navigation. Each helper
// reports whether it consumed the key so the fall-through order is preserved.
//
// The rule for the handlers dispatched from here: one that always consumes its
// message returns tea.Model, because handleKey tail-calls it; one that can
// DECLINE returns Model plus handled, and the caller threads the model back
// into h. Returning tea.Model from a declining handler would let a mutation on
// the fall-through path vanish silently, which is why cmd and handled are
// declared up front and assigned with plain "=" — a short declaration inside an
// if-statement would scope a fresh h to that block and reintroduce exactly that
// bug with no test able to see it. The leaf handlers below handleActionKey
// (handleNewSessionKey, handleLoginKey, handleSelectKey) always consume, but
// return Model too so handleActionKey can pass it back up concretely.
//
// The declining handlers' cmd is deliberately nil: declining means "not my
// key", so there is nothing to schedule and the next stage's cmd is the only
// one. A declining handler that ever needs to schedule work must batch it here
// rather than rely on this.
func (h Model) HandleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	h.clearFailureOnKey(msg)
	if h.AttentionAction.Active() {
		var cmd tea.Cmd
		h.AttentionAction, cmd = h.AttentionAction.Update(msg)
		if h.AttentionAction.MergeInFlight() {
			// A confirmed merge runs on without the operator: its row reads
			// merging and Home stays usable, as on the details view.
			h.MarkMerging(h.AttentionAction)
			h.setStatus(h.AttentionAction.Progress, statusOngoing)
			h.AttentionAction = ui.AttentionActionState{}
		}
		h.Table.SetHeight(h.TableHeight())
		return h, cmd
	}
	if h.Confirm.Active {
		return h.handleConfirmKey(msg)
	}
	// The rename editor swallows everything, and must be tested BEFORE the quit
	// arm below: "q" is an ordinary letter in a session title, and a rename
	// prompt that exits the whole TUI on it would be unusable.
	if h.Rename.Active() {
		return h.handleRenameKey(msg)
	}
	if h.Filter.Active() {
		return h.handleFilterKey(msg)
	}
	if msg.String() == "q" {
		return h, tea.Quit
	}
	if h.Upgrading || h.Restarting {
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

func (h Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	confirmed := msg.String() == "y" || msg.String() == "enter"
	var cmd tea.Cmd
	h.Confirm, cmd = h.Confirm.Update(msg)
	if confirmed && cmd != nil && h.LoggedIn {
		h.LogoutPending = true
	}
	return h, cmd
}

// handleActionKey routes the top-level action keys. handled=false means the key
// is navigation and belongs to the table, and leaves the model untouched.
func (h Model) handleActionKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	var cmd tea.Cmd
	var handled bool
	if sel := h.SelectedSession(); !h.isMerging(sel.GetId()) {
		h.AttentionAction, cmd, handled = h.AttentionAction.Start(h.Ctx, h.Client, sel, msg.String(), false, OpenURLFunc)
	}
	if handled {
		h.clearStatus()
		h.Table.SetHeight(h.TableHeight())
		return h, cmd, true
	}
	switch msg.String() {
	case "/":
		return h.startFilter()
	case "c":
		// Clears the ticket notice. Not "d": the upgrade banner's [d]ismiss
		// runs first, in handleUpgradeKey, so with both shown "d" would snooze
		// the upgrade instead. With no notice it is a no-op.
		model, cmd := h.dismissTicketNotices()
		return model, cmd, true
	case "esc":
		if h.Filter.Applied() {
			h.Filter.Reset()
			h.rebuildFilteredBoard()
			return h, nil, true
		}
		return h, nil, false
	case "n":
		model, cmd := h.handleNewSessionKey()
		return model, cmd, true
	case "s":
		return h, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewSettings} }, true
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
	case "g":
		// Hidden shortcut, following [r]: open the selected row's PR. A row
		// whose attention offers GITHUB never gets here, because start above
		// already opened the same URL. handled is true on every path: the
		// table's default keymap binds g to go-to-top, and a key that opens
		// a PR on one row must not jump the cursor on the next. home still
		// goes to the top.
		return h, h.openSelectedPRCmd(), true
	case "alt+up":
		// Hidden shortcut (BOS-1231), following the [r] precedent above: a real
		// binding deliberately absent from the action bar. handled is true on
		// every path — no session selected, and the boundary no-op included —
		// because the table's own keymap owns the unmodified arrows, and a user
		// who rebinds navigation onto the chord must not be able to turn a
		// reorder back into a cursor move.
		model, cmd := h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_UP)
		return model, cmd, true
	case "alt+down":
		model, cmd := h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_DOWN)
		return model, cmd, true
	}
	return h, nil, false
}

// openSelectedPRCmd opens the selected session's PR on GitHub, through the
// same command as the GITHUB attention action. It is nil when the row has no
// PR.
func (h Model) openSelectedPRCmd() tea.Cmd {
	sess := h.SelectedSession()
	if sess.GetPrUrl() == "" {
		return nil
	}
	return attentionActionCmd(h.Ctx, h.Client, proto.CloneOf(sess), pb.AttentionAction_ATTENTION_ACTION_GITHUB)
}

// handleRenameStartKey opens the inline title editor on the selected session.
// With nothing selected it is a no-op that still counts as handled.
func (h Model) handleRenameStartKey() (Model, tea.Cmd) {
	sess := h.SelectedSession()
	if sess == nil {
		return h, nil
	}
	var cmd tea.Cmd
	h.Rename, cmd = ui.NewRenamePrompt(sess.GetId(), sess.GetTitle())
	// Drop the previous rename's outcome: a "Renamed session to …" still on
	// screen above a freshly opened editor reads as the edit already landing.
	h.clearStatus()
	// The prompt replaces the action bar, which is one line shorter than the
	// editor, so the table has to give a row back or the board overflows. The
	// cleared status may free another; tableHeight reads both.
	h.Table.SetHeight(h.TableHeight())
	return h, cmd
}

// handleRenameKey owns the two keys that end the edit; everything else is text.
func (h Model) handleRenameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return h.CommitRename()
	case "esc":
		return h.cancelRename()
	}
	var cmd tea.Cmd
	h.Rename, cmd = h.Rename.Update(msg)
	return h, cmd
}

// commitRename issues the write for the session the prompt was opened on. An
// empty title is refused in place — the prompt stays open with the typed text
// intact and no RPC is sent, because a blank title would leave the row with
// nothing to identify it by.
func (h Model) CommitRename() (tea.Model, tea.Cmd) {
	title := h.Rename.Value()
	if title == "" {
		h.Rename = h.Rename.WithEmptyTitleError()
		return h, nil
	}
	sessionID := h.Rename.SessionID()
	h.Rename = ui.RenamePrompt{}
	h.Table.SetHeight(h.TableHeight())
	return h, renameSessionCmd(h.Client, h.Ctx, sessionID, title)
}

// cancelRename closes the editor and discards the edit.
func (h Model) cancelRename() (tea.Model, tea.Cmd) {
	h.Rename = ui.RenamePrompt{}
	h.Table.SetHeight(h.TableHeight())
	return h, nil
}

// dismissInputsIfHidden closes the editor when a poll has moved Home onto a
// screen that does not draw it. handleKey routes every key into an active
// prompt regardless of which View branch is rendering, but only
// renderSessionTable draws the footer — so an editor left open behind the
// daemon-error or empty-state screen would keep swallowing keys, including the
// [q] those screens advertise, with nothing on screen to explain why. The
// conditions below mirror View's branch order exactly; they must be kept in
// step with it.
func (h Model) dismissInputsIfHidden() Model {
	if h.Err == nil && !h.Loading && len(h.Sessions) > 0 {
		return h
	}
	h.Rename = ui.RenamePrompt{}
	h.Filter.Reset()
	h.Table.SetHeight(h.TableHeight())
	return h
}

func (h Model) handleNewSessionKey() (Model, tea.Cmd) {
	if h.RepoCount == 0 {
		return h, nil
	}
	return h, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewNewSession} }
}

func (h Model) handleLoginKey() (Model, tea.Cmd) {
	if h.authMgr == nil {
		return h, nil
	}
	if h.LogoutPending {
		return h, nil
	}
	if h.LoggedIn {
		authMgr := h.authMgr
		c := h.Client
		ctx := h.Ctx
		label := "Log out?"
		if h.LoggedInEmail != "" {
			label = fmt.Sprintf("Log out %s?", h.LoggedInEmail)
		}
		h.Confirm = ui.NewConfirmPrompt(label, func() tea.Msg {
			var err error
			if authMgr != nil {
				err = authMgr.Logout(ctx)
			}
			if err != nil || c == nil {
				return LogoutMsg{Err: err}
			}
			return LogoutMsg{
				notify: h.AuthChanges.Notify(ctx, c, "logout"),
			}
		})
		return h, nil
	}
	return h, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewLogin} }
}

func (h Model) handleSelectKey() (Model, tea.Cmd) {
	if h.RepoCount == 0 {
		// New user with no repos: guide them into adding their first
		// repository. firstRepo=true makes the add wizard return to the
		// home empty state on cancel rather than the repo list.
		return h, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewRepoAdd, FirstRepo: true} }
	}
	if sess := h.SelectedSession(); sess != nil {
		if sess.GetDisplaySettingUp() {
			return h, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewNewSession, SessionID: sess.Id} }
		}
		return h, func() tea.Msg {
			return ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: sess.Id, ChatID: sess.GetAttention().GetTargetChatId()}
		}
	}
	return h, nil
}

func (h Model) handleTableKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Forward navigation keys to the table.
	var cmd tea.Cmd
	previousCursor := h.Table.Cursor()
	h.Table, cmd = h.Table.Update(msg)
	h.normalizeTableCursor(previousCursor)
	// Rebuild so the selection (blue) styling follows the new cursor; this
	// also re-runs updateCursorColumn internally to keep the chevron in
	// sync. updateCursorColumn alone would only move the chevron, leaving
	// repo/name/pr stale on error/attention rows (BOS-103).
	h.BuildTableRows()
	return h, cmd
}
