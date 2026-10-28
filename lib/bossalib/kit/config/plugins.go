package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/recurser/bossalib/kit/buildinfo"
)

const pluginPrefix = "bossd-plugin-"

// DedupPluginConfigs returns cfgs with duplicate entries removed, keeping the
// first occurrence of each name. The second return value reports whether any
// duplicates were dropped, which callers can use to decide whether to persist
// the cleaned-up slice back to disk.
//
// Duplicates cause a second plugin subprocess to be launched with its own
// in-memory state, breaking per-session dedup in plugins like repair (each
// instance independently fires NotifyStatusChange → CreateWorkflow, yielding
// parallel chats).
func DedupPluginConfigs(cfgs []PluginConfig) ([]PluginConfig, bool) {
	if len(cfgs) <= 1 {
		return cfgs, false
	}
	seen := make(map[string]struct{}, len(cfgs))
	out := make([]PluginConfig, 0, len(cfgs))
	dropped := false
	for _, c := range cfgs {
		if _, ok := seen[c.Name]; ok {
			dropped = true
			continue
		}
		seen[c.Name] = struct{}{}
		out = append(out, c)
	}
	return out, dropped
}

// MergeDiscoveredPlugins appends any discovered plugin whose name is not
// already present in existing, returning the merged slice and the names that
// were added. Existing entries always win: their path, enabled flag, and config
// are preserved untouched, so a user who customized a plugin's path or disabled
// it keeps that choice even when discovery finds the binary on disk.
//
// This is what lets a freshly-built plugin binary (e.g. a new bossd-plugin-*
// added since settings.json was last written) load on the next daemon start
// without a hand-edit — and self-heals a config that a clobbering save stripped
// the entry from. The input slices are not mutated.
func MergeDiscoveredPlugins(existing, discovered []PluginConfig) ([]PluginConfig, []string) {
	have := make(map[string]struct{}, len(existing))
	for _, p := range existing {
		have[p.Name] = struct{}{}
	}

	merged := append([]PluginConfig(nil), existing...)
	var added []string
	for _, d := range discovered {
		if _, ok := have[d.Name]; ok {
			continue
		}
		have[d.Name] = struct{}{}
		merged = append(merged, d)
		added = append(added, d.Name)
	}
	return merged, added
}

// PluginRejection records a bossd-plugin-* binary that was found on disk but
// refused before exec, with the reason (untrusted perms, checksum mismatch,
// missing manifest). bossd surfaces these as security-level errors.
type PluginRejection struct {
	Name   string
	Path   string
	Reason string
}

// discoveryPolicy controls how strictly plugin discovery vets binaries.
type discoveryPolicy struct {
	requireSafePerms bool // reject group/world-writable or wrong-owner dirs+files
	verifyChecksums  bool // enforce plugins.sum (release builds)
	allowCWDWalk     bool // dev-mode, opt-in walk up from CWD looking for bin/
}

// devPluginsFromCWDEnv is the explicit opt-in for the dev-build plugin walk up
// from the working directory (BOS-1563). Only the exact value "1" enables it.
const devPluginsFromCWDEnv = "BOSS_DEV_PLUGINS_FROM_CWD"

// activePolicy derives the discovery policy from the build type. Path
// hardening always applies; checksum enforcement is gated on release vs dev.
// See BOS-27.
//
// The CWD walk additionally needs BOSS_DEV_PLUGINS_FROM_CWD=1 (BOS-1563): a dev
// boss/bossd started inside an untrusted checkout would otherwise exec that
// checkout's bin/bossd-plugin-*, and the path-hardening check passes because
// the cloning user owns the files. ./bin/bossd still finds its sibling plugins
// through the binary-relative locations without the opt-in.
func activePolicy() discoveryPolicy {
	release := buildinfo.IsReleaseBuild()
	return discoveryPolicy{
		requireSafePerms: true,
		verifyChecksums:  release,
		allowCWDWalk:     !release && os.Getenv(devPluginsFromCWDEnv) == "1",
	}
}

// DiscoverPlugins scans for plugin binaries in precedence order (see
// DiscoverPluginsVerified) and returns only the binaries that passed
// verification. Rejected binaries are dropped silently here; callers that need
// to surface them (bossd) use DiscoverPluginsVerified.
func DiscoverPlugins() []PluginConfig {
	plugins, _ := DiscoverPluginsVerified()
	return plugins
}

// DiscoverPluginsVerified scans for plugin binaries in precedence order:
//  1. ../libexec/plugins/ relative to the running binary (Homebrew layout),
//     then the binary's own directory (dev mode);
//  2. (dev builds with BOSS_DEV_PLUGINS_FROM_CWD=1 only) a bin/ directory
//     found by walking up from CWD;
//  3. the per-user plugin dir used by upgrades.
//
// Every candidate is vetted by the active discoveryPolicy. Returns accepted
// plugins and the rejected ones (with reasons).
func DiscoverPluginsVerified() ([]PluginConfig, []PluginRejection) {
	policy := activePolicy()
	if plugins, rej := discoverPluginsFrom("", policy); len(plugins) > 0 || len(rej) > 0 {
		return plugins, rej
	}
	if policy.allowCWDWalk {
		if plugins, rej := discoverDevPluginsFromCWD(policy); len(plugins) > 0 || len(rej) > 0 {
			return plugins, rej
		}
	}
	dir, err := UserPluginDir()
	if err != nil {
		return nil, nil
	}
	return scanForPlugins(dir, policy)
}

// osExecutable indirects os.Executable so the binDir=="" fallback (which
// locates plugins relative to the running binary) is unit-testable.
var osExecutable = os.Executable

func discoverPluginsFrom(binDir string, policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	if binDir == "" {
		exe, err := osExecutable()
		if err != nil {
			return nil, nil
		}
		resolved, err := filepath.EvalSymlinks(exe)
		if err != nil {
			return nil, nil
		}
		binDir = filepath.Dir(resolved)
	}
	libexecDir := filepath.Clean(filepath.Join(binDir, "..", "libexec", "plugins"))
	if plugins, rej := scanForPlugins(libexecDir, policy); len(plugins) > 0 || len(rej) > 0 {
		return plugins, rej
	}
	return scanForPlugins(binDir, policy)
}

func discoverDevPluginsFromCWD(policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, nil
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if plugins, rej := scanForPlugins(filepath.Join(dir, "bin"), policy); len(plugins) > 0 || len(rej) > 0 {
			return plugins, rej
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
	}
}

// platformSuffixes lists OS/arch suffixes to skip during plugin discovery
// (cross-compiled binaries in dev mode).
var platformSuffixes = []string{
	"-darwin-arm64", "-darwin-amd64",
	"-linux-arm64", "-linux-amd64",
}

// nonDiscoverablePlugins lists plugin binaries that must never be picked up by
// auto-discovery.
//
// bossd-plugin-stub-runner is a deterministic AgentRunnerService used only by
// E2E tests (it launches no real agent subprocess) and is NO_DISTRIBUTE. The
// E2E harness loads it via an explicit plugins config entry; auto-discovering
// it would surface a non-functional "stub" agent in real and dev daemons'
// agent pickers.
//
// bossd-plugin-opencode was excluded through the BOS-433..436 epic slices while
// its run/launch RPCs returned codes.Unimplemented; BOS-437 wired and
// live-validated the launch path (StartAgentRun/StopRun/IsRunning/ExitStatus drive a
// real opencode session), so the binary is now a functional agent runner and is
// discovered like claude/codex. It is intentionally NO LONGER listed here.
var nonDiscoverablePlugins = []string{
	"bossd-plugin-stub-runner",
}

func isNonDiscoverablePlugin(name string) bool {
	return slices.Contains(nonDiscoverablePlugins, name)
}

// experimentalPlugins lists plugin binaries that ship installed but stay OFF
// until the user names them in the `experimental_plugins` settings key.
//
// This is a GATE, not a filter, and the distinction from nonDiscoverablePlugins
// above is the whole point. A non-discoverable plugin is removed from the
// config outright and has no opt-in path; an experimental plugin stays
// discoverable, listable, and path-healed while it is off, so a user can turn
// it on without hand-authoring a plugins[] entry. Re-listing a plugin here in
// nonDiscoverablePlugins instead would make it unreachable — and for
// bossd-plugin-opencode specifically it would revert BOS-437 and fail
// TestFilterNonDiscoverablePluginsKeepsOpencode.
//
// bossd-plugin-opencode is installed for every user (infra/install.sh,
// infra/homebrew/bossanova.rb) and was auto-enabled by discovery, so a
// very-alpha agent runner loaded on every daemon unasked (BOS-1145).
var experimentalPlugins = []string{
	"bossd-plugin-opencode",
}

// IsExperimentalPlugin reports whether shortName names an experimental plugin
// (see experimentalPlugins). It accepts the un-prefixed PluginConfig.Name
// spelling — the canonical one, and what `plugins[].name` and
// `experimental_plugins` both use — and also tolerates the full
// "bossd-plugin-" binary spelling, because settings.json has no validation
// feedback: an opt-in written the other way would otherwise fail silently and
// look exactly like a broken gate.
func IsExperimentalPlugin(shortName string) bool {
	if shortName == "" {
		return false
	}
	return slices.Contains(experimentalPlugins, pluginPrefix+strings.TrimPrefix(shortName, pluginPrefix))
}

// ApplyExperimentalPluginGate returns cfgs with every experimentalPlugins
// member's Enabled flag set from optIn (the `experimental_plugins` settings
// key), plus the names of members it turned OFF that were previously on.
//
// For a registry member the gate is AUTHORITATIVE IN BOTH DIRECTIONS: listed in
// optIn means Enabled:true, absent means Enabled:false, regardless of the
// persisted plugins[].enabled value. Overriding the persisted flag is what
// repairs the existing installed base, whose settings.json already carries
// {"name":"opencode","enabled":true} from a daemon that auto-enabled it — a
// discovery-default change alone can never reach those users. The consequence,
// which the settings reference documents, is that plugins[].enabled is inert
// for a registry member.
//
// Plugins outside the registry are never touched in either direction, so an
// ordinary user enable/disable is preserved. An optIn entry naming a plugin
// that is not present (or not a registry member) is ignored; no entry is ever
// invented. The input slice is not mutated, matching MergeDiscoveredPlugins and
// HealPluginPaths.
func ApplyExperimentalPluginGate(cfgs []PluginConfig, optIn []string) ([]PluginConfig, []string) {
	out := make([]PluginConfig, 0, len(cfgs))
	var disabled []string
	for _, c := range cfgs {
		if !IsExperimentalPlugin(c.Name) {
			out = append(out, c)
			continue
		}
		wantEnabled := slices.ContainsFunc(optIn, func(name string) bool {
			return strings.TrimPrefix(name, pluginPrefix) == strings.TrimPrefix(c.Name, pluginPrefix)
		})
		if c.Enabled && !wantEnabled {
			disabled = append(disabled, c.Name)
		}
		c.Enabled = wantEnabled
		out = append(out, c)
	}
	return out, disabled
}

// PluginEnabledForSettings reports a plugin's EFFECTIVE enabled state — the
// value the daemon's plugin host actually acts on — for a settings consumer
// that has one plugin name rather than the whole slice the gate operates on.
//
// For a registry member experimental_plugins is authoritative, because
// ApplyExperimentalPluginGate overrides plugins[].enabled in both directions
// and the daemon deliberately does not persist the result. Reading
// plugins[].enabled directly therefore hides a plugin the user opted into and
// the daemon has loaded, which is what kept an opted-in OpenCode out of the
// agent pickers. The opt-in comparison reuses the gate's trim-both-sides form
// so the tolerated "bossd-plugin-opencode" spelling behaves identically here.
//
// A plugin with no entry at all is not enabled, matching the callers this
// replaces.
func PluginEnabledForSettings(s Settings, name string) bool {
	if IsExperimentalPlugin(name) {
		return slices.ContainsFunc(s.ExperimentalPlugins, func(optIn string) bool {
			return strings.TrimPrefix(optIn, pluginPrefix) == strings.TrimPrefix(name, pluginPrefix)
		})
	}
	for _, p := range s.Plugins {
		if p.Name == name {
			return p.Enabled
		}
	}
	return false
}

// FilterNonDiscoverablePlugins drops any entry that names a non-discoverable
// plugin (see nonDiscoverablePlugins), returning the filtered slice and the
// names that were removed. PluginConfig.Name holds the binary name without the
// "bossd-plugin-" prefix, so it is re-prefixed before comparison.
//
// Auto-discovery (scanForPlugins) already skips these binaries, but a config
// persisted by an older daemon — built before the binary was marked
// non-discoverable — can still carry a stale entry. Filtering the persisted
// list at load keeps such an entry from loading and surfacing a non-functional
// agent (e.g. the E2E-only "stub-runner") in the picker. The explicit --plugins
// path (E2E) intentionally bypasses this so tests can still load the stub.
func FilterNonDiscoverablePlugins(cfgs []PluginConfig) ([]PluginConfig, []string) {
	out := make([]PluginConfig, 0, len(cfgs))
	var dropped []string
	for _, c := range cfgs {
		if isNonDiscoverablePlugin(pluginPrefix + c.Name) {
			dropped = append(dropped, c.Name)
			continue
		}
		out = append(out, c)
	}
	return out, dropped
}

// scanForPlugins scans dir for bossd-plugin-* executables, applying the policy.
// Cross-compiled binaries with platform suffixes are skipped (not rejections).
func scanForPlugins(dir string, policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}

	// Directory-level perms gate: an untrusted dir taints every binary in it.
	if policy.requireSafePerms {
		if ok, reason := isTrustedPath(dir); !ok {
			var rej []PluginRejection
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasPrefix(name, pluginPrefix) || hasPlatformSuffix(name) {
					continue
				}
				if isNonDiscoverablePlugin(name) {
					continue
				}
				if !isExecutableFile(filepath.Join(dir, name)) {
					continue
				}
				rej = append(rej, PluginRejection{
					Name:   name[len(pluginPrefix):],
					Path:   filepath.Join(dir, name),
					Reason: "untrusted plugin directory: " + reason,
				})
			}
			return nil, rej
		}
	}

	// Load the manifest once if checksum verification is on. A missing/invalid
	// manifest on a release build means we reject every binary (fail closed).
	var sums map[string]string
	var sumErr error
	if policy.verifyChecksums {
		sums, sumErr = loadPluginSums(dir)
	}

	var plugins []PluginConfig
	var rejections []PluginRejection
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, pluginPrefix) {
			continue
		}
		if isNonDiscoverablePlugin(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if !isExecutableFile(path) {
			continue
		}
		if hasPlatformSuffix(name) {
			continue
		}
		shortName := name[len(pluginPrefix):]

		if policy.requireSafePerms {
			if ok, reason := isTrustedPath(path); !ok {
				rejections = append(rejections, PluginRejection{Name: shortName, Path: path, Reason: reason})
				continue
			}
		}
		if policy.verifyChecksums {
			if sumErr != nil {
				rejections = append(rejections, PluginRejection{Name: shortName, Path: path,
					Reason: "checksum manifest unavailable: " + sumErr.Error()})
				continue
			}
			if ok, reason := verifyPluginChecksum(path, sums); !ok {
				rejections = append(rejections, PluginRejection{Name: shortName, Path: path, Reason: reason})
				continue
			}
		}
		// Experimental plugins are discovered but arrive disabled, so the
		// persisted settings.json agrees with the gate bossd applies at load
		// rather than showing a misleading "enabled": true (BOS-1145).
		plugins = append(plugins, PluginConfig{Name: shortName, Path: path, Enabled: !IsExperimentalPlugin(shortName)})
	}
	return plugins, rejections
}

// VerifyConfiguredPlugins re-applies the active discovery policy's safety checks
// to an explicit plugin list — entries that came from settings.Plugins (e.g.
// persisted by `boss config init --plugin-dir`, which the official installer
// runs) or a hand-edited config rather than from a fresh auto-discovery scan.
// Auto-discovery (scanForPlugins) vets every binary it returns, but configured
// entries are exec'd by their stored path without re-checking, so on a release
// build a plugin binary swapped after `config init` would otherwise bypass the
// plugins.sum manifest the installer shipped beside the binaries. This verifies
// each configured binary against the manifest in its own directory and returns
// the accepted entries plus any rejections (so callers can fail closed). On dev
// builds (checksum enforcement off) the list is returned unchanged.
func VerifyConfiguredPlugins(cfgs []PluginConfig) ([]PluginConfig, []PluginRejection) {
	return verifyConfiguredPlugins(cfgs, activePolicy())
}

func verifyConfiguredPlugins(cfgs []PluginConfig, policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	// Each plugin directory carries its own plugins.sum; load each at most once.
	type manifest struct {
		sums map[string]string
		err  error
	}
	manifests := make(map[string]manifest)
	accepted := make([]PluginConfig, 0, len(cfgs))
	var rejections []PluginRejection
	for _, c := range cfgs {
		dir := filepath.Dir(c.Path)
		if policy.requireSafePerms {
			if ok, reason := isTrustedPath(dir); !ok {
				rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path,
					Reason: "untrusted plugin directory: " + reason})
				continue
			}
			if ok, reason := isTrustedPath(c.Path); !ok {
				rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path, Reason: reason})
				continue
			}
		}
		if !policy.verifyChecksums {
			accepted = append(accepted, c)
			continue
		}
		m, ok := manifests[dir]
		if !ok {
			m.sums, m.err = loadPluginSums(dir)
			manifests[dir] = m
		}
		if m.err != nil {
			rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path,
				Reason: "checksum manifest unavailable: " + m.err.Error()})
			continue
		}
		if ok, reason := verifyPluginChecksum(c.Path, m.sums); !ok {
			rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path, Reason: reason})
			continue
		}
		accepted = append(accepted, c)
	}
	return accepted, rejections
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode := info.Mode()
	return mode.IsRegular() && mode.Perm()&0o111 != 0
}

func hasPlatformSuffix(name string) bool {
	for _, s := range platformSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// UserPluginDir returns the per-user plugin directory shared by boss upgrade
// and bossd plugin auto-discovery.
func UserPluginDir() (string, error) {
	settings, err := loadWithoutSideEffects()
	if err != nil {
		return "", err
	}
	if dir, ok, err := ConfiguredAppDataDir(settings); err != nil {
		return "", err
	} else if ok {
		return filepath.Join(dir, "plugins"), nil
	}

	dir, err := DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins"), nil
}

func loadWithoutSideEffects() (Settings, error) {
	p, err := Path()
	if err != nil {
		return DefaultSettings(), err
	}
	return LoadFrom(p)
}
