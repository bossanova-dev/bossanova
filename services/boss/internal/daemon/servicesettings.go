package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/config"
)

// ServiceSettingsPathEnv is the variable that selects a daemon profile. Every
// boss process reads its settings file from it (config.Path), so the service
// definitions carry the same variable rather than a second spelling of it.
const ServiceSettingsPathEnv = "BOSS_SETTINGS_PATH"

// serviceSettingsHostileChars are the characters a settings path may not
// contain before it is baked into a service definition.
//
// plistHostileChars covers the plist and the watchdog argv: text/template
// performs no escaping, so `<`, `>`, `&` and the quotes corrupt the XML, and a
// newline injects content. The two extras are for the systemd units, where the
// value lands inside a QUOTED Environment= assignment: `\` is re-read as a
// C-style escape and `%` as a unit specifier, so neither survives the round
// trip intact. One set for every platform keeps a profile that installs on
// macOS installable on Linux too.
const serviceSettingsHostileChars = plistHostileChars + `\%`

// noticeServiceSettingsPathChanged reports a rewrite that moves the installed
// service to a different settings file. Package var, following the
// warnDaemonRefreshFailed idiom, so a test can observe it.
//
// It is a notice and not a refusal: an explicit BOSS_SETTINGS_PATH wins, and
// this line is the operator's only guard against having exported the wrong one.
var noticeServiceSettingsPathChanged = func(installed, next string) {
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: %s selects settings %s, but the installed service used %s; rewriting the service to use %s\n",
		ServiceSettingsPathEnv, DescribeServiceSettingsPath(next), DescribeServiceSettingsPath(installed),
		DescribeServiceSettingsPath(next))
}

// defaultSettingsPath is the settings file a process with no
// BOSS_SETTINGS_PATH reads.
func defaultSettingsPath() (string, error) {
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// serviceSettingsPath returns the settings path this process would bake into
// a service definition, or "" to bake nothing.
//
// "" is returned both when BOSS_SETTINGS_PATH is unset and when it names the
// OS default, so a default install renders byte-identically to one made before
// the variable was carried, and a shell that exports the default explicitly
// does not churn the unit.
//
// An error means the value cannot be baked: a relative path (config.Path's own
// rule and message) or a character a plist or systemd unit cannot carry.
func serviceSettingsPath() (string, error) {
	if os.Getenv(ServiceSettingsPathEnv) == "" {
		return "", nil
	}
	p, err := config.Path()
	if err != nil {
		return "", err
	}
	if err := validateServiceSettingsPath(p); err != nil {
		return "", err
	}
	if os.Getenv("XDG_CONFIG_HOME") != "" {
		// The default below would be derived from THIS shell's XDG override,
		// which no service definition carries, so the service could resolve a
		// different default. Bake the explicit value rather than risk dropping it.
		return p, nil
	}
	if def, err := defaultSettingsPath(); err == nil && filepath.Clean(def) == p {
		return "", nil
	}
	return p, nil
}

// ServiceSettingsPath is serviceSettingsPath for the CLI, which prints the
// value an install baked.
func ServiceSettingsPath() (string, error) {
	return serviceSettingsPath()
}

// DescribeServiceSettingsPath renders a baked settings path for an operator:
// the path itself, or "default" when nothing is baked.
func DescribeServiceSettingsPath(p string) string {
	if p == "" {
		return "default"
	}
	return p
}

// validateServiceSettingsPath rejects a value that is not absolute or that
// would corrupt the service definition it is interpolated into, naming the
// offending character.
func validateServiceSettingsPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s must be absolute: %q", ServiceSettingsPathEnv, p)
	}
	if i := strings.IndexAny(p, serviceSettingsHostileChars); i >= 0 {
		return fmt.Errorf("%s %q contains %q, which cannot be written into a service definition",
			ServiceSettingsPathEnv, p, p[i:i+1])
	}
	return nil
}

// rewriteServiceSettingsPath decides the settings path a REWRITE of an
// installed service bakes, given the value the installed definition carries
// ("" for the default).
//
// Rewrites (restart, the staged-plist refresh) are routinely run from a shell
// that never exported BOSS_SETTINGS_PATH, so an unset variable keeps what is
// installed — a baked profile is never silently dropped. A set variable wins,
// and a notice is printed when that changes the installed value.
//
// The preserved value is validated too: it was read back out of a file a human
// may have edited, and it is about to go through the same unescaped template.
func rewriteServiceSettingsPath(installed string) (string, error) {
	if os.Getenv(ServiceSettingsPathEnv) == "" {
		if installed == "" {
			return "", nil
		}
		if err := validateServiceSettingsPath(installed); err != nil {
			return "", fmt.Errorf("installed service settings path: %w", err)
		}
		return installed, nil
	}
	next, err := serviceSettingsPath()
	if err != nil {
		return "", err
	}
	if next != installed {
		noticeServiceSettingsPathChanged(installed, next)
	}
	return next, nil
}
