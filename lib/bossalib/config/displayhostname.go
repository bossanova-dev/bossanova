package config

import (
	"strings"
	"sync"
)

// displayHostnameSuffixes are the trailing domain suffixes stripped from a
// machine hostname before an operator reads it: macOS's DHCP ".lan" and Bonjour
// ".local". See CONCEPTS.md, "Daemon display name", for why neither belongs in
// a name a human reads.
var displayHostnameSuffixes = []string{".lan", ".local"}

// DefaultDisplayHostname derives the default name a daemon should self-report
// for display, given the raw machine hostname. Precedence:
//
//  1. the platform's operator-facing computer name, where the host keeps one
//     separately from its OS hostname (the macOS ComputerName), once
//     it is non-empty after trimming;
//  2. otherwise the passed hostname with at most one trailing ".lan"/".local"
//     stripped, matched case-insensitively;
//  3. otherwise the passed hostname, byte for byte.
//
// It is PRESENTATION only. Callers must keep passing the raw OS hostname — never
// this result — to daemon-ID resolution, or renaming a machine would re-key the
// daemon, rotate its persisted UUID and invalidate its stream tokens.
//
// The result is never empty for a non-empty input: bosso rejects a registration
// whose hostname is blank, so a hostname that is nothing but a suffix (".lan")
// comes back unchanged rather than stripped away. An empty input returns empty
// only where the platform offers no computer name of its own — every !darwin
// build, and a darwin host that reports no ComputerName. On a darwin host that
// does report one, the probe takes precedence over the (empty) hostname and
// that name is returned instead. Callers must therefore keep their "machine
// hostname unavailable" branch for the empty result, but must not assume the
// empty result is the only possible answer to an empty input.
//
// Compose it with DaemonDisplayName, which owns the daemon_name override:
//
//	config.DaemonDisplayName(settings, config.DefaultDisplayHostname(raw))
func DefaultDisplayHostname(hostname string) string {
	return defaultDisplayHostnameWith(hostname, computerName)
}

// defaultDisplayHostnameWith is DefaultDisplayHostname with the platform probe
// injected. The seam exists so the precedence, the trim and the suffix strip are
// testable on every platform without depending on the host's real ComputerName —
// the same reason bossd's resolveDaemonIdentityWith injects its id resolver.
//
// A probe that reports ok but yields only whitespace is not a name, and falls
// through to the strip rather than advertising a blank.
func defaultDisplayHostnameWith(hostname string, probe func() (string, bool)) string {
	if name, ok := probe(); ok {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			return trimmed
		}
	}
	return stripDisplayHostnameSuffix(hostname)
}

// stripDisplayHostnameSuffix removes at most one trailing ".lan"/".local" from
// hostname, matched case-insensitively, and only where a non-empty remainder is
// left behind. The remainder is returned byte for byte: the case-insensitive
// match governs the suffix alone, never the name the operator reads.
func stripDisplayHostnameSuffix(hostname string) string {
	for _, suffix := range displayHostnameSuffixes {
		// "<=" rather than "<" is what keeps R2: a hostname that is exactly the
		// suffix has no remainder to show, so it is left alone instead of being
		// emptied out into a registration bosso would reject.
		if len(hostname) <= len(suffix) {
			continue
		}
		if strings.EqualFold(hostname[len(hostname)-len(suffix):], suffix) {
			return hostname[:len(hostname)-len(suffix)]
		}
	}
	return hostname
}

// memoizeProbe caches a computer-name probe for the life of the process. The
// answer is process-lifetime stable, and one caller (the boss TUI's General
// Settings construction) runs inside Bubble Tea's Update on the goroutine that
// renders every frame — so a probe that forks must fork at most once.
func memoizeProbe(probe func() (string, bool)) func() (string, bool) {
	return sync.OnceValues(probe)
}
