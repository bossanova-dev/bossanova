// Package config manages global Bossanova settings stored as a JSON file
// in the user's config directory.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

// DiscoverPlugins scans for plugin binaries relative to the running binary's
// location. It checks ../libexec/plugins/ first (Homebrew layout, resolving
// symlinks), then falls back to the binary's own directory (dev mode where
// all binaries live in bin/). Returns an empty slice if no plugins are found.
func DiscoverPlugins() []PluginConfig {
	return discoverPluginsFrom("")
}

// discoverPluginsFrom is the testable core of DiscoverPlugins. When binDir is
// empty it uses os.Executable() to locate the binary directory.
func discoverPluginsFrom(binDir string) []PluginConfig {
	if binDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(exe)
		if err != nil {
			return nil
		}
		binDir = filepath.Dir(resolved)
	}

	// Try Homebrew layout first: ../libexec/plugins/
	libexecDir := filepath.Clean(filepath.Join(binDir, "..", "libexec", "plugins"))
	if plugins := scanForPlugins(libexecDir); len(plugins) > 0 {
		return plugins
	}

	// Fall back to same directory as binary (dev mode).
	return scanForPlugins(binDir)
}

// platformSuffixes lists OS/arch suffixes to skip during plugin discovery
// (cross-compiled binaries in dev mode).
var platformSuffixes = []string{
	"-darwin-arm64", "-darwin-amd64",
	"-linux-arm64", "-linux-amd64",
}

// scanForPlugins scans a directory for any executable matching the
// bossd-plugin-* prefix and returns a PluginConfig for each one found.
// Cross-compiled binaries with platform suffixes are skipped.
func scanForPlugins(dir string) []PluginConfig {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var plugins []PluginConfig
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, pluginPrefix) {
			continue
		}
		if hasPlatformSuffix(name) {
			continue
		}
		plugins = append(plugins, PluginConfig{
			Name:    name[len(pluginPrefix):],
			Path:    filepath.Join(dir, name),
			Enabled: true,
		})
	}
	return plugins
}

func hasPlatformSuffix(name string) bool {
	for _, s := range platformSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Settings holds global Bossanova configuration.
type Settings struct {
	WorktreeBaseDir     string         `json:"worktree_base_dir"`
	DefaultAgent        string         `json:"default_agent,omitempty"`
	SkillsDeclined      bool           `json:"skills_declined,omitempty"`
	PollIntervalSeconds int            `json:"poll_interval_seconds,omitempty"`
	Plugins             []PluginConfig `json:"plugins,omitempty"`
	Repair              RepairConfig   `json:"repair,omitzero"`
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

// Path returns the default settings file path.
// On macOS: ~/Library/Application Support/bossanova/settings.json
// On Linux: ~/.config/bossanova/settings.json
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bossanova", "settings.json"), nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
