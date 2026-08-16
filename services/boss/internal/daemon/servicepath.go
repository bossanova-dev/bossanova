package daemon

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/config"
)

// userHomeDir is indirected so the ~ expansion below is testable without
// depending on the developer's real home directory.
var userHomeDir = os.UserHomeDir

// loadServiceSettings reads the settings that feed the rendered service PATH.
// It is a package var so tests can inject settings without writing a real
// settings.json, following the runLaunchctl / findDaemonProcess idiom.
var loadServiceSettings = config.Load

// servicePathHostileChars are the characters an entry may not contain before it
// reaches a service template. This is load-bearing, not hygiene: both templates
// interpolate the joined PATH with text/template, which performs NO escaping.
//
//   - `<`, `>`, `&`, `"` corrupt the plist XML.
//   - a newline or carriage return injects arbitrary directives into the
//     systemd unit, which is a sharper failure than a malformed file.
//   - `:` is the PATH separator itself, so an entry containing one is either a
//     mis-entered list or an attempt to smuggle extra entries past this filter.
//   - `\` would be re-interpreted as a C-style escape inside the QUOTED systemd
//     `Environment=` value, so a literal backslash cannot survive the round
//     trip intact.
//
// A space or tab is deliberately NOT here: `/opt/my tools/bin` is a legal Unix
// directory. It is unquoted systemd `Environment=` that cannot carry one, which
// unitTemplate solves by quoting the value rather than by dropping the entry.
const servicePathHostileChars = "<>&\"\n\r:\\"

// pathExtras returns the sanitized, expanded, de-duplicated directories that
// daemon_path_extra prepends to the service PATH, in the order declared.
//
// Every rejection is silent by design: a bad entry costs the operator that one
// directory, never the daemon's ability to start. `boss daemon doctor` reports
// the effective PATH so a dropped entry is still discoverable.
func pathExtras(settings config.Settings) []string {
	extras := make([]string, 0, len(settings.DaemonPathExtra))
	seen := make(map[string]struct{}, len(settings.DaemonPathExtra))

	for _, raw := range settings.DaemonPathExtra {
		entry, ok := normalizePathEntry(raw)
		if !ok {
			continue
		}
		if _, duplicate := seen[entry]; duplicate {
			continue
		}
		seen[entry] = struct{}{}
		extras = append(extras, entry)
	}

	return extras
}

// normalizePathEntry expands a leading `~/`, cleans the result, and reports
// whether the entry is usable in a service file at all.
func normalizePathEntry(raw string) (string, bool) {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return "", false
	}
	if strings.ContainsAny(entry, servicePathHostileChars) {
		return "", false
	}

	// `~` alone is a home directory, not a bin directory; only `~/...` expands.
	// Treating a bare `~` as a PATH entry would silently put the whole home
	// directory on the daemon's PATH.
	if entry == "~" {
		return "", false
	}
	if strings.HasPrefix(entry, "~/") {
		home, err := userHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		entry = filepath.Join(home, strings.TrimPrefix(entry, "~/"))
	}

	// `~someone/bin` is not expanded: resolving another user's home is a
	// different lookup, and leaving the literal on the PATH would be a silently
	// broken entry. Rejecting it is the honest outcome.
	if !filepath.IsAbs(entry) {
		return "", false
	}

	return filepath.Clean(entry), true
}

// joinServicePath renders the final PATH string: extras first in their declared
// order, then the platform baseline, with the first occurrence of any duplicate
// winning so a configured entry keeps its front position.
//
// It re-applies the hostile-character filter to EVERY entry, not just the
// configured extras. The systemd baseline is the PATH this process inherited,
// so on that platform a baseline entry is not a compile-time literal and would
// otherwise reach the unit template unchecked.
func joinServicePath(extras, baseline []string) string {
	entries := make([]string, 0, len(extras)+len(baseline))
	seen := make(map[string]struct{}, len(extras)+len(baseline))

	for _, entry := range append(append([]string{}, extras...), baseline...) {
		if entry == "" || strings.ContainsAny(entry, servicePathHostileChars) {
			continue
		}
		if _, duplicate := seen[entry]; duplicate {
			continue
		}
		seen[entry] = struct{}{}
		entries = append(entries, entry)
	}

	return strings.Join(entries, ":")
}

// ServiceEnvPath returns the PATH the generated bossd service file will set.
// It is exported for `boss daemon doctor`, which reports the effective service
// PATH rather than the caller's own — resolving node under an interactive
// shell is exactly the check that passes while the daemon is broken.
func ServiceEnvPath() string {
	return serviceEnvPath()
}

// serviceEnvSettings loads the settings behind the service PATH, falling back
// to defaults when they cannot be read. An unreadable settings file must never
// collapse the PATH to empty: the baseline is what keeps the daemon able to run
// git.
func serviceEnvSettings() config.Settings {
	settings, err := loadServiceSettings()
	if err != nil {
		return config.Settings{}
	}
	return settings
}

// LookPathIn reports the first directory in a PATH-formatted string that holds
// an executable file named name. It deliberately does not consult the caller's
// own PATH: the question it answers is what the daemon will find.
func LookPathIn(pathValue, name string) (string, bool) {
	for _, dir := range strings.Split(pathValue, ":") {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate, true
	}
	return "", false
}
