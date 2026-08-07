//go:build e2e

package main

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
)

// e2eHostReconnectDefaultPolls is how many 1s RunDaemonWait probes report the
// tunnel as still down before the seed lets it recover. Long enough that a
// proof waitFor reliably catches the reconnecting screen even when the bridge
// is slow to send its first op, short enough that the recovery scene is not a
// stall.
const e2eHostReconnectDefaultPolls = 3

// e2eHostReconnectReasonLimit caps the seeded failure reason. It is production's
// own cap (host.go hostTunnelReasonLimit) rather than a number of its own, so a
// seeded frame can never stage a screen a live tunnel could not produce.
const e2eHostReconnectReasonLimit = hostTunnelReasonLimit

// runE2EHostReconnectSeed stages the --host tunnel-dropped screen before the
// TUI dials its client, so a proof scenario can capture the reconnecting state
// (and its unaided recovery) on a harness that has no network and never runs
// --host. It calls the production runHostReconnectWait, so what the scenario
// captures is the real screen, not a mock of it.
//
// BOSS_HOST_E2E_RECONNECT carries the ssh destination to display; an unset or
// explicitly falsey value means no seed. BOSS_HOST_E2E_RECONNECT_REASON carries
// the supervisor's classified failure the screen reports (BOS-724), which the
// harness cannot produce for real because no test here ever runs an ssh child.
// The production twin in hostwait_prod.go returns nil unconditionally and reads
// no environment at all, so this whole path is a compile-time no-op outside an
// e2e-tagged build.
//
// The detail is built by the production hostReconnectDetail, so a scenario
// captures the same formatting a real drop produces — including the fixed
// fallback sentence when no reason is seeded.
func runE2EHostReconnectSeed() error {
	destination := resolveE2EHostReconnectDestination()
	if destination == "" {
		return nil
	}
	// RunDaemonWait's ticks are delivered from tea.Tick goroutines, so the
	// countdown is atomic rather than a plain int: -race must stay quiet.
	var remaining atomic.Int64
	remaining.Store(int64(resolveE2EHostReconnectPolls()))
	healthy := func() bool { return remaining.Add(-1) < 0 }
	return runHostReconnectWait(destination, hostReconnectDetail(resolveE2EHostReconnectReason()), healthy)
}

// resolveE2EHostReconnectDestination maps BOSS_HOST_E2E_RECONNECT to the ssh
// destination the wait screen names. An operator who exports the var as "0" or
// "false" plainly means off, and a seed that read that as on would hijack an
// unrelated run with a full-screen blocking wait — so the falsey set is
// enumerated explicitly. Any other non-empty value is used verbatim (trimmed),
// because it is a destination string, not a flag.
func resolveE2EHostReconnectDestination() string {
	value := strings.TrimSpace(os.Getenv("BOSS_HOST_E2E_RECONNECT"))
	switch strings.ToLower(value) {
	case "", "0", "false", "no", "off":
		return ""
	default:
		return value
	}
}

// resolveE2EHostReconnectPolls maps BOSS_HOST_E2E_RECONNECT_POLLS to the number
// of failing probes before recovery. A malformed or negative value falls back to
// the default rather than failing the run: this knob only tunes how long a proof
// scene lingers, and a boot abort over it would be a worse outcome than a scene
// of the standard length.
func resolveE2EHostReconnectPolls() int {
	raw := strings.TrimSpace(os.Getenv("BOSS_HOST_E2E_RECONNECT_POLLS"))
	if raw == "" {
		return e2eHostReconnectDefaultPolls
	}
	polls, err := strconv.Atoi(raw)
	if err != nil || polls < 0 {
		return e2eHostReconnectDefaultPolls
	}
	return polls
}

// resolveE2EHostReconnectReason maps BOSS_HOST_E2E_RECONNECT_REASON to the
// supervisor diagnosis the reconnecting screen reports. Empty means no reason
// was recorded, which is a real production state (the first failing RPC can
// beat the ssh child's exit), and the screen falls back to its fixed sentence.
//
// The falsey set is enumerated for the same reason the destination enumerates
// it: an operator who exports the var as "0" plainly means off, and printing
// "Last tunnel failure: 0" would be nonsense on screen.
//
// Unlike the destination this value never becomes argv — it is rendered text
// only. What it can still do is corrupt the frame the scenario captures: an
// escape or control byte would move the cursor or recolour the terminal, and an
// overlong value would push the action bar out of view. Both are refused rather
// than sanitized, because a seed exists to stage a screen and a value that
// cannot be one is a mistake worth ignoring, not repairing.
func resolveE2EHostReconnectReason() string {
	value := strings.TrimSpace(os.Getenv("BOSS_HOST_E2E_RECONNECT_REASON"))
	switch strings.ToLower(value) {
	case "", "0", "false", "no", "off":
		return ""
	}
	if len(value) > e2eHostReconnectReasonLimit {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return value
}
