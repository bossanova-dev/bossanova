//go:build e2e

package main

import (
	"os"
	"strings"
	"unicode"

	"github.com/recurser/boss/internal/views"
)

// applyE2EHostAttachSeed puts the TUI into a --host remote context without a
// tunnel, so a proof scenario can capture the views that degrade under --host —
// today, the chat picker with no [t]erminal action — on a harness that has no
// second machine.
//
// It cannot stage the chat-session-gone screen, and no scenario should try: that
// screen needs a REACHABLE host whose tmux answers "no such session", while a
// seeded unreachable destination is deliberately classified as unreachable
// precisely so a live remote chat is never declared gone. Both directions of
// that decision are covered by Go tests instead.
//
// It sets the SAME switch the real --host startup sets (views.SetHostDestination
// in cmd/host.go), so what a scenario captures is the production remote context
// rather than a mock of it. It does NOT stand up a tunnel: every RPC still goes
// to the local mock daemon, which is what makes the seeded world render at all.
//
// BOSS_HOST_E2E_ATTACH_DESTINATION carries the ssh destination; an unset or
// explicitly falsey value means no seed. The production twin in
// hostattach_prod.go is a no-op that reads no environment at all, so a
// production boss cannot be talked into a remote context by an env var.
// It returns the destination it applied, or "" when it applied none, so the
// seed's two directions are assertable without exporting a read accessor from
// views purely for a test. The caller ignores the value.
func applyE2EHostAttachSeed() string {
	destination := resolveE2EHostAttachDestination()
	if destination == "" {
		return ""
	}
	views.SetHostDestination(destination)
	return destination
}

// resolveE2EHostAttachDestination maps BOSS_HOST_E2E_ATTACH_DESTINATION to the
// ssh destination the TUI should believe it is driving. The falsey set is
// enumerated explicitly for the same reason the reconnect seed enumerates it: an
// operator who exports the var as "0" plainly means off, and reading that as a
// destination would hijack an unrelated run into a remote context. Any other
// destination-shaped value is used verbatim (trimmed).
//
// Unlike every other BOSS_*_E2E_* value, this one does not merely flip on-screen
// text: it becomes argv for the ssh the attach path and the liveness probe run
// (`ssh -o BatchMode=yes <destination> tmux has-session …`). ProofEnvWhitelist
// validates env KEYS only, and scenario files are agent-authored, so a value
// like `-oProxyCommand=…` would be read by ssh as an option and execute an
// arbitrary command on the harness. Anything that is not destination-shaped is
// therefore refused outright rather than passed through — the seed exists to
// stage a screen, not to configure ssh.
func resolveE2EHostAttachDestination() string {
	value := strings.TrimSpace(os.Getenv("BOSS_HOST_E2E_ATTACH_DESTINATION"))
	switch strings.ToLower(value) {
	case "", "0", "false", "no", "off":
		return ""
	}
	if !destinationShaped(value) {
		return ""
	}
	return value
}

// destinationShaped reports whether value can only be read by ssh as a
// destination. A leading "-" makes ssh parse it as an option (the injection that
// matters), and whitespace or control bytes belong to neither a hostname nor a
// user@host — they only appear when something is being smuggled through.
func destinationShaped(value string) bool {
	if strings.HasPrefix(value, "-") {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
