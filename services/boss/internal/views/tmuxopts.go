package views

import (
	"os/exec"
	"strings"
)

// tmuxNotificationOptions are the tmux session options boss enables so OS
// notifications work when running inside tmux:
//   - allow-passthrough lets the OSC notification escape reach the outer
//     terminal (tmux drops it otherwise);
//   - focus-events lets boss learn when its pane regains focus, so clicking the
//     notification (which focuses the terminal) can jump to the waiting chat.
//
// Both default off, and neither has a pane-scoped form — enabling them affects
// the whole tmux session, so we only change ones that were off and undo them on
// exit.
var tmuxNotificationOptions = []string{"allow-passthrough", "focus-events"}

// tmuxCommand runs a tmux subcommand and returns its trimmed stdout. A var so
// tests can intercept it without a real tmux server.
var tmuxCommand = func(args ...string) (string, error) {
	// #nosec G204 -- fixed "tmux" binary; args are literal option names, no user input
	// owner=@recurser review-by=2027-01-22 issue=BOS-459
	out, err := exec.Command("tmux", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// EnableTmuxNotificationOptions turns on the tmux session options boss needs for
// desktop notifications (and focus-driven navigation) when running inside tmux,
// and returns a function that restores each option it changed. It is a no-op
// with a nil-safe restore when not inside tmux or when notifications are
// disabled. Options already enabled are left untouched (and not restored).
//
// Enabling allow-passthrough is a deliberate, reversible change to the user's
// tmux session: it is scoped to the current session, only applied when it was
// off, and unset again on exit.
func EnableTmuxNotificationOptions(notificationsEnabled bool) func() {
	return enableTmuxNotificationOptions(currentNotifyEnv().inTmux(), notificationsEnabled)
}

func enableTmuxNotificationOptions(inTmux, notificationsEnabled bool) func() {
	noop := func() {}
	if !inTmux || !notificationsEnabled {
		return noop
	}

	var changed []string
	for _, opt := range tmuxNotificationOptions {
		// -A resolves inherited/global values, so "on" here means effectively on
		// already — leave it alone and record nothing to restore.
		value, err := tmuxCommand("show-options", "-Av", opt)
		if err != nil {
			continue
		}
		if value == "on" {
			continue
		}
		if _, err := tmuxCommand("set-option", opt, "on"); err != nil {
			continue
		}
		changed = append(changed, opt)
	}
	if len(changed) == 0 {
		return noop
	}

	return func() {
		// Unset (rather than force "off") so each option falls back to whatever
		// the user's global/default was — the realistic prior state, since we
		// only ever touch options that were effectively off.
		for _, opt := range changed {
			_, _ = tmuxCommand("set-option", "-u", opt)
		}
	}
}
