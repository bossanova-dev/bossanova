package home

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// homeStatusTTL is how long a finished action's outcome stays on Home's status
// line. It matches the window the cron and accounts lists give their own
// outcome lines.
const HomeStatusTTL = 5 * time.Second

// HomeStatusTick schedules the expiry. Tests replace it to fire at once.
var HomeStatusTick = tea.Tick

// homeStatusKind decides how long a line on Home's status line stays.
type homeStatusKind uint8

const (
	// statusOutcome reports something that has finished, such as "Merged PR
	// #12". It clears itself after homeStatusTTL.
	statusOutcome homeStatusKind = iota
	// statusOngoing describes something still under way, such as a merge whose
	// answer has not come back. It stays until a newer status replaces it.
	statusOngoing
	// statusFailure reports what went wrong. It has no timer: it stays until
	// the operator presses a key other than navigation, so a failure that lands
	// while they are away, or while they hold an arrow key through a merge, is
	// still there when they look.
	statusFailure
)

// homeStatusExpiredMsg fires homeStatusTTL after an outcome is shown. Both
// fields must still match for it to clear the line: seq so an older outcome's
// timer cannot clear a newer status, homeGeneration so a timer from a Home that
// has since been rebuilt cannot clear the rebuilt one's.
type StatusExpiredMsg struct {
	HomeGeneration uint64
	seq            uint64
}

// setStatus replaces the status line and returns the timer that clears an
// outcome. Only another failure replaces a failure the operator has not
// dismissed: an outcome from another row would hide it and then time out, and
// an ongoing line would hide it until something replaced that.
func (h *Model) setStatus(text string, kind homeStatusKind) tea.Cmd {
	if kind != statusFailure && h.StatusErr() {
		return nil
	}
	h.writeStatus(text, kind)
	return h.statusExpiry()
}

// clearStatus empties the status line, a failure included. Callers clear on a
// deliberate act by the operator.
func (h *Model) clearStatus() {
	h.writeStatus("", statusOutcome)
}

// writeStatus is the one place the status line changes, so every change bumps
// statusSeq and no older timer can clear it.
func (h *Model) writeStatus(text string, kind homeStatusKind) {
	h.statusSeq++
	h.Status, h.StatusKind = text, kind
}

func (h Model) StatusErr() bool {
	return h.Status != "" && h.StatusKind == statusFailure
}

// statusExpiry is the timer for the current status, or nil when it does not
// expire. Init calls it too, for an outcome carried into a rebuilt Home.
func (h Model) statusExpiry() tea.Cmd {
	if h.Status == "" || h.StatusKind != statusOutcome {
		return nil
	}
	msg := StatusExpiredMsg{HomeGeneration: h.Generation, seq: h.statusSeq}
	return HomeStatusTick(HomeStatusTTL, func(time.Time) tea.Msg { return msg })
}

func (h Model) handleStatusExpired(msg StatusExpiredMsg) (tea.Model, tea.Cmd) {
	if msg.HomeGeneration != h.Generation || msg.seq != h.statusSeq {
		return h, nil
	}
	h.clearStatus()
	h.Table.SetHeight(h.TableHeight())
	return h, nil
}

// keyOnlyNavigates reports whether a key only moves the cursor, so it leaves a
// failure on screen. Home stays usable while a merge runs, and a held key
// repeats faster than a frame lasts: clearing on it wiped a merge failure
// before it was ever drawn. The keys are the table's own keymap, so they track
// what the table pages on, less g, which handleActionKey takes to open the
// selected row's PR.
func (h Model) keyOnlyNavigates(msg tea.KeyMsg) bool {
	if msg.String() == "g" {
		return false
	}
	for _, bindings := range h.Table.KeyMap.FullHelp() {
		if key.Matches(msg, bindings...) {
			return true
		}
	}
	return false
}

// clearFailureOnKey drops a failure once the operator presses a key other than
// navigation. That key is a deliberate act, typed with the failure on screen.
// A key typed into the search box or the rename editor belongs to that field,
// and a failure landing mid-word would otherwise go with the next letter.
func (h *Model) clearFailureOnKey(msg tea.KeyMsg) {
	if _, press := msg.(tea.KeyPressMsg); !press || !h.StatusErr() {
		return
	}
	if h.Filter.Active() || h.Rename.Active() || h.keyOnlyNavigates(msg) {
		return
	}
	h.clearStatus()
	h.restoreMergeStatus()
	h.Table.SetHeight(h.TableHeight())
}

// restoreMergeStatus shows the line of a merge that is still unresolved. The
// failure held that line back, and without it a row reading merging would sit
// beside an empty status line.
func (h *Model) restoreMergeStatus() {
	for _, s := range h.Sessions {
		if line := h.MergesInFlight[s.GetId()].OngoingStatus(); line != "" {
			h.writeStatus(line, statusOngoing)
			return
		}
	}
}
