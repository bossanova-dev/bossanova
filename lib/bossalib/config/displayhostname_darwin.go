//go:build darwin

package config

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// computerNameProbeTimeout bounds the scutil subprocess so a wedged
// SystemConfiguration answer degrades to the suffix-stripping fallback instead
// of hanging. It bounds one stall, it does not prevent it — memoisation below is
// what keeps that bound from being paid again. Two seconds matches
// lib/bossalib/termnorm's terminfo probe, this repo's precedent for a bounded
// constant-argv probe.
const computerNameProbeTimeout = 2 * time.Second

// computerName reports the operator-facing machine name macOS keeps in
// SystemConfiguration — see CONCEPTS.md, "Daemon display name".
//
// The fork is memoised, so a repeated caller pays the up-to-2s bound at most
// once per process rather than on every call; see memoizeProbe.
var computerName = memoizeProbe(probeComputerName)

// probeComputerName runs the actual probe. It shells out to
// `scutil --get ComputerName` rather than calling SCDynamicStoreCopyComputerName
// because that CoreFoundation binding needs cgo, which this library avoids, and
// the backing preferences plist is a private format. Every argv word is a string
// literal, so there is no injection surface and gosec G204 does not fire.
//
// Any failure at all — scutil absent, non-zero exit, the timeout firing, or a
// whitespace-only answer — reports ok == false so the caller falls back to the
// hostname it already had.
func probeComputerName() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), computerNameProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "scutil", "--get", "ComputerName").Output()
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", false
	}
	return name, true
}
