// Package config manages global Bossanova settings stored as a JSON file
// in the user's config directory.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings holds global Bossanova configuration.
type Settings struct {
	DangerouslySkipPermissions bool   `json:"dangerously_skip_permissions"`
	WorktreeBaseDir            string `json:"worktree_base_dir"`
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
