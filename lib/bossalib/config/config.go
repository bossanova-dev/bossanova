// Package config manages global Bossanova settings stored as a JSON file
// in the user's config directory.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// AutopilotSkills maps workflow steps to skill names.
type AutopilotSkills struct {
	Plan      string `json:"plan,omitempty"`
	Implement string `json:"implement,omitempty"`
	Handoff   string `json:"handoff,omitempty"`
	Resume    string `json:"resume,omitempty"`
	Verify    string `json:"verify,omitempty"`
	Land      string `json:"land,omitempty"`
}

// AutopilotConfig holds configuration for the autopilot plugin.
type AutopilotConfig struct {
	Skills              AutopilotSkills `json:"skills,omitzero"`
	HandoffDir          string          `json:"handoff_dir,omitempty"`
	PollIntervalSeconds int             `json:"poll_interval_seconds,omitempty"`
	MaxFlightLegs       int             `json:"max_flight_legs,omitempty"`
	ConfirmLand         bool            `json:"confirm_land,omitempty"`
}

// RepairSkills maps repair workflow operations to skill names.
type RepairSkills struct {
	Repair string `json:"repair,omitempty"`
}

// RepairConfig holds configuration for the repair plugin.
type RepairConfig struct {
	Skills               RepairSkills `json:"skills,omitzero"`
	CooldownMinutes      int          `json:"cooldown_minutes,omitempty"`
	PollIntervalSeconds  int          `json:"poll_interval_seconds,omitempty"`
	SweepIntervalMinutes int          `json:"sweep_interval_minutes,omitempty"`
}

var defaultSkills = map[string]string{
	"plan":      "boss-create-tasks",
	"implement": "boss-implement",
	"handoff":   "boss-handoff",
	"resume":    "boss-resume",
	"verify":    "boss-verify",
	"land":      "boss-finalize",
}

// HandoffDirectory returns the configured handoff directory or the default.
func (c AutopilotConfig) HandoffDirectory() string {
	if c.HandoffDir != "" {
		return c.HandoffDir
	}
	return "docs/handoffs"
}

// PollInterval returns the configured poll interval or the default of 5 seconds.
func (c AutopilotConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return 5 * time.Second
}

// MaxLegs returns the configured max flight legs or the default of 20.
func (c AutopilotConfig) MaxLegs() int {
	if c.MaxFlightLegs > 0 {
		return c.MaxFlightLegs
	}
	return 20
}

// SkillName returns the skill name for a given workflow step,
// using the configured override or the default.
func (c AutopilotConfig) SkillName(step string) string {
	switch step {
	case "plan":
		if c.Skills.Plan != "" {
			return c.Skills.Plan
		}
	case "implement":
		if c.Skills.Implement != "" {
			return c.Skills.Implement
		}
	case "handoff":
		if c.Skills.Handoff != "" {
			return c.Skills.Handoff
		}
	case "resume":
		if c.Skills.Resume != "" {
			return c.Skills.Resume
		}
	case "verify":
		if c.Skills.Verify != "" {
			return c.Skills.Verify
		}
	case "land":
		if c.Skills.Land != "" {
			return c.Skills.Land
		}
	}
	return defaultSkills[step]
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

// SkillName returns the configured repair skill name or the default.
func (c RepairConfig) SkillName() string {
	if c.Skills.Repair != "" {
		return c.Skills.Repair
	}
	return "boss-repair"
}

// knownPlugins lists plugin binary names to scan for during auto-discovery.
var knownPlugins = []string{
	"bossd-plugin-autopilot",
	"bossd-plugin-dependabot",
	"bossd-plugin-repair",
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

// scanForPlugins checks a directory for known plugin binaries and returns
// a PluginConfig for each one found.
func scanForPlugins(dir string) []PluginConfig {
	var plugins []PluginConfig
	for _, name := range knownPlugins {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			pluginName := name[len("bossd-plugin-"):]
			plugins = append(plugins, PluginConfig{
				Name:    pluginName,
				Path:    path,
				Enabled: true,
			})
		}
	}
	return plugins
}

// Settings holds global Bossanova configuration.
type Settings struct {
	DangerouslySkipPermissions bool            `json:"dangerously_skip_permissions"`
	WorktreeBaseDir            string          `json:"worktree_base_dir"`
	SkillsDeclined             bool            `json:"skills_declined,omitempty"`
	PollIntervalSeconds        int             `json:"poll_interval_seconds,omitempty"`
	Plugins                    []PluginConfig  `json:"plugins,omitempty"`
	Autopilot                  AutopilotConfig `json:"autopilot,omitzero"`
	Repair                     RepairConfig    `json:"repair,omitzero"`
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
		DangerouslySkipPermissions: false,
		WorktreeBaseDir:            filepath.Join(home, ".bossanova", "worktrees"),
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
