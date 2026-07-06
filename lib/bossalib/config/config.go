// Package config manages global Bossanova settings stored as a JSON file
// in the user's config directory.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/buildinfo"
)

const settingsPathEnv = "BOSS_SETTINGS_PATH"

// refuseDefaultEnv, when set to "1", marks the process as running under test so
// SaveTo refuses to overwrite the developer's real settings.json. The test
// harnesses set it in the boss subprocess (where testing.Testing() is false) so
// an E2E test that forgets to point BOSS_SETTINGS_PATH at a temp file fails
// loudly instead of clobbering the real file. It has no effect on shipped
// binaries (unset). Reads are never blocked — only writes to the real default.
// Intended only for test harnesses: do not set it in a real shell, or the
// shipped binary will refuse to save its settings.
const refuseDefaultEnv = "BOSS_REFUSE_DEFAULT_SETTINGS"

// realDefaultPath is the OS default settings path computed once at package init,
// before any t.Setenv can redirect HOME. It is the path the under-test guard
// refuses. Empty if it cannot be resolved (the guard then no-ops).
var realDefaultPath = func() string {
	if dir, err := DefaultAppDataDir(); err == nil {
		return filepath.Join(dir, "settings.json")
	}
	return ""
}()

// initEnvSettingsPath is BOSS_SETTINGS_PATH as it was at process start, captured
// once at package init before any t.Setenv can change it. For an in-process test
// binary launched from a developer shell (e.g. via direnv) this is the
// developer's real settings.json, so the under-test guard refuses writes to it —
// catching tests that redirect HOME but forget BOSS_SETTINGS_PATH (which
// config.Path() consults before HOME). Empty when the env var is unset (CI) or
// relative, in which case the guard no-ops. It is NOT refused for the subprocess
// sentinel: a test harness deliberately points BOSS_SETTINGS_PATH at its own temp
// file before spawning, so there this path IS the legitimate write target.
var initEnvSettingsPath = resolveInitEnvSettingsPath()

// resolveInitEnvSettingsPath reads BOSS_SETTINGS_PATH and returns its cleaned
// absolute value, or "" when the var is unset or relative. It is split out of
// the package-init var above so the env-gating logic can be unit-tested with
// t.Setenv without re-running package initialization.
func resolveInitEnvSettingsPath() string {
	if p := os.Getenv(settingsPathEnv); p != "" && filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return ""
}

// guardRealDefaultWrite returns an error when a test would overwrite a real
// settings.json. It is the pure, unit-testable core of the write guard: reads
// are never blocked, only a write whose resolved path equals a real settings
// location while running under test. Tests must isolate via BOSS_SETTINGS_PATH (a
// temp file) or a redirected HOME so the resolved path matches neither location.
//
// Two locations are guarded:
//   - realDefault: the OS-default path (HOME-derived). Refused for both the
//     in-process test binary and the subprocess sentinel.
//   - envPath: BOSS_SETTINGS_PATH captured at process start. Refused only for the
//     in-process test binary (inProcessTest); the subprocess sentinel points
//     BOSS_SETTINGS_PATH at its own temp file, so there envPath is legitimate.
func guardRealDefaultWrite(path string, inProcessTest, sentinelSet bool, realDefault, envPath string) error {
	clean := filepath.Clean(path)
	if (inProcessTest || sentinelSet) && realDefault != "" && clean == realDefault {
		return fmt.Errorf(
			"refusing to overwrite the real settings path %q under test: set %s to an absolute temp path or redirect HOME to a temp dir",
			realDefault, settingsPathEnv)
	}
	if inProcessTest && envPath != "" && clean == envPath {
		return fmt.Errorf(
			"refusing to overwrite the developer's %s path %q under test: point %s at an absolute temp file (a redirected HOME alone is not enough — %s takes precedence)",
			settingsPathEnv, envPath, settingsPathEnv, settingsPathEnv)
	}
	return nil
}

// PluginConfig describes a single plugin to load.
type PluginConfig struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Enabled bool              `json:"enabled"`
	Version string            `json:"version,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
}

// RepairSkills maps repair workflow operations to skill names.
type RepairSkills struct {
	Repair string `json:"repair,omitempty"`
}

// RepairConfig holds configuration for the repair plugin.
type RepairConfig struct {
	Skills                     RepairSkills `json:"skills,omitzero"`
	CooldownMinutes            int          `json:"cooldown_minutes,omitempty"`
	PollIntervalSeconds        int          `json:"poll_interval_seconds,omitempty"`
	SweepIntervalMinutes       int          `json:"sweep_interval_minutes,omitempty"`
	IdleRepairThresholdMinutes int          `json:"idle_repair_threshold_minutes,omitempty"`
}

// CooldownDuration returns the configured cooldown or the default of 1 minute.
func (c RepairConfig) CooldownDuration() time.Duration {
	if c.CooldownMinutes > 0 {
		return time.Duration(c.CooldownMinutes) * time.Minute
	}
	return 1 * time.Minute
}

// PollInterval returns the configured poll interval or the default of 5 seconds.
func (c RepairConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return 5 * time.Second
}

// IdleRepairThreshold returns the configured idle threshold or the default of 5 minutes.
// When a session has a live chat but its most recent output is older than this
// threshold, the repair plugin treats the chat as idle and proceeds with repair.
func (c RepairConfig) IdleRepairThreshold() time.Duration {
	if c.IdleRepairThresholdMinutes > 0 {
		return time.Duration(c.IdleRepairThresholdMinutes) * time.Minute
	}
	return 5 * time.Minute
}

// SkillName returns the configured repair skill name or the default.
func (c RepairConfig) SkillName() string {
	if c.Skills.Repair != "" {
		return c.Skills.Repair
	}
	return "boss-repair"
}

// RotationConfig holds account-rotation policy knobs.
type RotationConfig struct {
	DefaultCooldownMinutes int `json:"default_cooldown_minutes,omitempty"`
	// Enabled gates auto-rotation of headless runs. nil = unset = enabled;
	// unlike the int-based knobs below, a default-true bool can't use the
	// "if x > 0" idiom, so this is a *bool.
	Enabled                  *bool `json:"enabled,omitempty"`
	MaxRotationsPerRun       int   `json:"max_rotations_per_run,omitempty"`
	ParkSweepIntervalSeconds int   `json:"park_sweep_interval_seconds,omitempty"`

	// AutoRotateChats is the global scope for automatic rotation of interactive
	// tmux chats on a CHAT_STATUS_LIMITED transition (Epic 4.3, BOS-175). nil
	// means ON (decision D4: fully automatic by default). Distinct from Enabled,
	// which gates headless-run rotation (BOS-174).
	AutoRotateChats *bool `json:"auto_rotate_chats,omitempty"`
	// AutoRotateChatsPerRepo overrides the global interactive-chat scope per repo
	// ID (both directions). The global kill-switch/settings UX is BOS-176.
	AutoRotateChatsPerRepo map[string]bool `json:"auto_rotate_chats_per_repo,omitempty"`
	// ChatRotateMinIntervalMinutes rate-limits automatic rotation attempts per
	// chat (belt-and-braces against banner-flap loops). 0 = default (10m).
	ChatRotateMinIntervalMinutes int `json:"chat_rotate_min_interval_minutes,omitempty"`
}

// AutoRotateChatsEnabled resolves the interactive-chat auto-rotate scope for a
// repo: per-repo override → global → default ON (decision D4).
func (c RotationConfig) AutoRotateChatsEnabled(repoID string) bool {
	if v, ok := c.AutoRotateChatsPerRepo[repoID]; ok {
		return v
	}
	if c.AutoRotateChats != nil {
		return *c.AutoRotateChats
	}
	return true
}

// ChatRotateMinInterval returns the per-chat automatic-rotation rate limit, or
// the default of 10 minutes when unset.
func (c RotationConfig) ChatRotateMinInterval() time.Duration {
	if c.ChatRotateMinIntervalMinutes > 0 {
		return time.Duration(c.ChatRotateMinIntervalMinutes) * time.Minute
	}
	return 10 * time.Minute
}

// DefaultCooldown returns the configured default cooldown applied to a
// usage-limited account when the signal carries no reset time, or 60 minutes
// when unset.
func (c RotationConfig) DefaultCooldown() time.Duration {
	if c.DefaultCooldownMinutes > 0 {
		return time.Duration(c.DefaultCooldownMinutes) * time.Minute
	}
	return 60 * time.Minute
}

// RotationEnabled returns whether auto-rotation is enabled. Unset (nil)
// defaults to true.
func (c RotationConfig) RotationEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// MaxRotations returns the configured cap on rotations per headless run, or
// the default of 3 when unset.
func (c RotationConfig) MaxRotations() int {
	if c.MaxRotationsPerRun > 0 {
		return c.MaxRotationsPerRun
	}
	return 3
}

// ParkSweepInterval returns the configured interval between sweeps of parked
// sessions awaiting rotation, or the default of 60 seconds when unset.
func (c RotationConfig) ParkSweepInterval() time.Duration {
	if c.ParkSweepIntervalSeconds > 0 {
		return time.Duration(c.ParkSweepIntervalSeconds) * time.Second
	}
	return 60 * time.Second
}

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
	allowCWDWalk     bool // dev-mode walk up from CWD looking for bin/
}

// activePolicy derives the discovery policy from the build type. Path
// hardening always applies; checksum enforcement and the CWD walk are gated on
// release vs dev. See docs/plans/BOS-27-*.md.
func activePolicy() discoveryPolicy {
	release := buildinfo.IsReleaseBuild()
	return discoveryPolicy{
		requireSafePerms: true,
		verifyChecksums:  release,
		allowCWDWalk:     !release,
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
//  2. (dev builds only) a bin/ directory found by walking up from CWD;
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
// auto-discovery. bossd-plugin-stub-runner is a deterministic AgentRunnerService
// used only by E2E tests (it launches no real agent subprocess) and is
// NO_DISTRIBUTE. The E2E harness loads it via an explicit plugins config entry;
// auto-discovering it would surface a non-functional "stub" agent in real and
// dev daemons' agent pickers.
var nonDiscoverablePlugins = []string{"bossd-plugin-stub-runner"}

func isNonDiscoverablePlugin(name string) bool {
	return slices.Contains(nonDiscoverablePlugins, name)
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
		plugins = append(plugins, PluginConfig{Name: shortName, Path: path, Enabled: true})
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
	if !policy.verifyChecksums {
		return cfgs, nil
	}
	// Each plugin directory carries its own plugins.sum; load each at most once.
	type manifest struct {
		sums map[string]string
		err  error
	}
	manifests := make(map[string]manifest)
	accepted := make([]PluginConfig, 0, len(cfgs))
	var rejections []PluginRejection
	for _, c := range cfgs {
		if policy.requireSafePerms {
			if ok, reason := isTrustedPath(c.Path); !ok {
				rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path, Reason: reason})
				continue
			}
		}
		dir := filepath.Dir(c.Path)
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

// Settings holds global Bossanova configuration.
type Settings struct {
	WorktreeBaseDir                string            `json:"worktree_base_dir"`
	AppDataDir                     string            `json:"app_data_dir,omitempty"`
	SocketPath                     string            `json:"socket_path,omitempty"`
	DefaultAgent                   string            `json:"default_agent,omitempty"`
	InstalledAt                    time.Time         `json:"installed_at,omitzero"`
	BossCloudGuestOfferHidden      bool              `json:"boss_cloud_guest_offer_hidden,omitempty"`
	BossCloudValueDeliveredAt      time.Time         `json:"boss_cloud_value_delivered_at,omitzero"`
	SkillsDeclined                 bool              `json:"skills_declined,omitempty"`
	SkillsDeclinedByAgent          map[string]bool   `json:"skills_declined_by_agent,omitempty"`
	SkillsDeclinedManifestByAgent  map[string]string `json:"skills_declined_manifest_by_agent,omitempty"`
	SkillsInstalledManifestByAgent map[string]string `json:"skills_installed_manifest_by_agent,omitempty"`
	PollIntervalSeconds            int               `json:"poll_interval_seconds,omitempty"`
	EventTracingEnabled            bool              `json:"event_tracing_enabled,omitempty"`
	ErrorTrackingEnabled           bool              `json:"error_tracking_enabled,omitempty"`
	PostHogProjectToken            string            `json:"posthog_project_token,omitempty"`
	PostHogHost                    string            `json:"posthog_host,omitempty"`
	Plugins                        []PluginConfig    `json:"plugins,omitempty"`
	Repair                         RepairConfig      `json:"repair,omitzero"`
	Rotation                       RotationConfig    `json:"rotation,omitzero"`
	ProvidersAcknowledged          bool              `json:"providers_acknowledged,omitempty"`
	KnownAgentProviders            []string          `json:"known_agent_providers,omitempty"`
	// LoginShell is the user's interactive shell ($SHELL), captured by the TUI
	// (which runs in that shell) so the launchd daemon — which has no $SHELL —
	// can launch agents through it for per-project tool resolution.
	LoginShell string `json:"login_shell,omitempty"`
}

// PluginConfigBool reads a boolean-valued entry from a named plugin's
// Config map. Returns false when the plugin isn't configured, the key is
// absent, or the value isn't "true".
func PluginConfigBool(s *Settings, pluginName, key string) bool {
	if s == nil {
		return false
	}
	for _, p := range s.Plugins {
		if p.Name == pluginName {
			return p.Config[key] == "true"
		}
	}
	return false
}

// SetPluginConfigBool writes a boolean-valued entry into a named plugin's
// Config map. When value is true it stores "true"; when false it removes
// the key entirely so the JSON stays clean. If the named plugin isn't yet
// in s.Plugins, an entry is appended so the toggle isn't silently lost
// before `boss config init` runs (init later fills in Path/Enabled, while
// the Config map is preserved by name).
func SetPluginConfigBool(s *Settings, pluginName, key string, value bool) {
	if s == nil {
		return
	}
	for i := range s.Plugins {
		if s.Plugins[i].Name != pluginName {
			continue
		}
		if value {
			if s.Plugins[i].Config == nil {
				s.Plugins[i].Config = map[string]string{}
			}
			s.Plugins[i].Config[key] = "true"
		} else if s.Plugins[i].Config != nil {
			delete(s.Plugins[i].Config, key)
		}
		return
	}
	if !value {
		// Removing a key from a not-yet-configured plugin is a no-op.
		return
	}
	s.Plugins = append(s.Plugins, PluginConfig{
		Name:   pluginName,
		Config: map[string]string{key: "true"},
	})
}

// PluginConfigString returns the string value at plugin.Config[key] or "".
func PluginConfigString(s *Settings, pluginName, key string) string {
	if s == nil {
		return ""
	}
	for _, p := range s.Plugins {
		if p.Name == pluginName {
			return p.Config[key]
		}
	}
	return ""
}

// SetPluginConfigString writes a string entry into the named plugin's Config
// map. Empty value deletes the key (matches SetPluginConfigBool behaviour).
// If the named plugin isn't yet in s.Plugins, an entry is appended so the
// setting isn't silently lost before `boss config init` runs.
func SetPluginConfigString(s *Settings, pluginName, key, value string) {
	if s == nil {
		return
	}
	for i := range s.Plugins {
		if s.Plugins[i].Name != pluginName {
			continue
		}
		if value == "" {
			if s.Plugins[i].Config != nil {
				delete(s.Plugins[i].Config, key)
			}
			return
		}
		if s.Plugins[i].Config == nil {
			s.Plugins[i].Config = map[string]string{}
		}
		s.Plugins[i].Config[key] = value
		return
	}
	if value == "" {
		// Removing a key from a not-yet-configured plugin is a no-op.
		return
	}
	s.Plugins = append(s.Plugins, PluginConfig{
		Name:   pluginName,
		Config: map[string]string{key: value},
	})
}

// SetPluginConfigEnum writes a string-valued setting after validating that
// value appears in allowed. Returns an error without mutating s when the
// value is rejected.
func SetPluginConfigEnum(s *Settings, pluginName, key, value string, allowed []string) error {
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("value %q not in allowed list %v for %s.%s", value, allowed, pluginName, key)
	}
	SetPluginConfigString(s, pluginName, key, value)
	return nil
}

// DisplayPollInterval returns the interval for polling PR display status.
// Defaults to 2 minutes if not configured.
func (s Settings) DisplayPollInterval() time.Duration {
	if s.PollIntervalSeconds > 0 {
		return time.Duration(s.PollIntervalSeconds) * time.Second
	}
	return 2 * time.Minute
}

// DefaultSettings returns settings with sensible defaults.
func DefaultSettings() Settings {
	home, _ := os.UserHomeDir()
	return Settings{
		WorktreeBaseDir: filepath.Join(home, ".bossanova", "worktrees"),
		DefaultAgent:    "claude",
	}
}

// EnsureInstalledAt returns settings with InstalledAt initialized.
// It reports whether the caller should persist the returned settings.
func (s Settings) EnsureInstalledAt(now time.Time) (Settings, bool) {
	if !s.InstalledAt.IsZero() {
		return s, false
	}
	s.InstalledAt = now.UTC()
	return s, true
}

// EnsureBossCloudValueDeliveredAt returns settings with BossCloudValueDeliveredAt
// initialized to now (UTC). It reports whether the caller should persist the
// returned settings. If BossCloudValueDeliveredAt is already set, it is left
// unchanged and the method returns false.
func (s Settings) EnsureBossCloudValueDeliveredAt(now time.Time) (Settings, bool) {
	if !s.BossCloudValueDeliveredAt.IsZero() {
		return s, false
	}
	s.BossCloudValueDeliveredAt = now.UTC()
	return s, true
}

// DefaultAppDataDir returns the platform-default directory for Bossanova's
// per-user state. The settings file, daemon database, lock, socket, and the
// plugin directory all live here when no app_data_dir/socket_path override is
// configured, so boss and bossd agree on one location on every platform.
//
//	macOS: ~/Library/Application Support/bossanova
//	Linux: $XDG_CONFIG_HOME/bossanova (defaults to ~/.config/bossanova)
//
// It does not create the directory; callers that need it on disk MkdirAll it
// themselves.
func DefaultAppDataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bossanova"), nil
}

// Path returns the default settings file path.
// If BOSS_SETTINGS_PATH is set, it must be absolute and points directly to the
// settings file to load. Otherwise platform defaults apply.
// On macOS: ~/Library/Application Support/bossanova/settings.json
// On Linux: ~/.config/bossanova/settings.json
func Path() (string, error) {
	if p := os.Getenv(settingsPathEnv); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be absolute: %q", settingsPathEnv, p)
		}
		return filepath.Clean(p), nil
	}

	dir, err := DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ConfiguredAppDataDir returns the configured local runtime data directory.
// The boolean is false when app_data_dir is unset and callers should keep their
// existing platform default.
func ConfiguredAppDataDir(s Settings) (string, bool, error) {
	if s.AppDataDir == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(s.AppDataDir) {
		return "", false, fmt.Errorf("app_data_dir must be absolute: %q", s.AppDataDir)
	}
	dir := filepath.Clean(s.AppDataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("create app_data_dir %q: %w", dir, err)
	}
	return dir, true, nil
}

// ConfiguredSocketPath returns the configured Unix socket path. socket_path
// wins when set. If only app_data_dir is set, the socket defaults to
// app_data_dir/bossd.sock. The boolean is false when neither setting applies.
func ConfiguredSocketPath(s Settings) (string, bool, error) {
	if s.SocketPath != "" {
		if !filepath.IsAbs(s.SocketPath) {
			return "", false, fmt.Errorf("socket_path must be absolute: %q", s.SocketPath)
		}
		return filepath.Clean(s.SocketPath), true, nil
	}

	dir, ok, err := ConfiguredAppDataDir(s)
	if err != nil || !ok {
		return "", ok, err
	}
	return filepath.Join(dir, "bossd.sock"), true, nil
}

// Load reads settings from the default path, returning defaults if the file is missing.
// It ensures the worktree base directory exists.
func Load() (Settings, error) {
	p, err := Path()
	if err != nil {
		return DefaultSettings(), err
	}
	s, err := LoadFrom(p)
	if s.WorktreeBaseDir != "" {
		_ = os.MkdirAll(s.WorktreeBaseDir, 0o755)
	}
	return s, err
}

// LoadFrom reads settings from a specific path, returning defaults if the file is missing.
func LoadFrom(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultSettings(), nil
		}
		return DefaultSettings(), err
	}

	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultSettings(), err
	}
	// Backfill DefaultAgent if the file explicitly sets it to "" — that's the
	// only case this branch covers, since files that omit the key entirely
	// already inherit "claude" from the DefaultSettings() seed used as the
	// unmarshal target above. Downstream code can't tolerate an empty agent
	// name, so normalise both shapes to "claude".
	if s.DefaultAgent == "" {
		s.DefaultAgent = "claude"
	}
	return s, nil
}

// Save writes settings to the default path.
func Save(s Settings) error {
	p, err := Path()
	if err != nil {
		return err
	}
	return SaveTo(p, s)
}

// SaveTo writes settings to a specific path, creating parent directories as needed.
func SaveTo(path string, s Settings) error {
	if err := guardRealDefaultWrite(
		path,
		testing.Testing(),
		os.Getenv(refuseDefaultEnv) == "1",
		realDefaultPath,
		initEnvSettingsPath,
	); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	_ = syncDir(dir)
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	return nil
}
