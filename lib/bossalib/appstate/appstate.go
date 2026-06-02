package appstate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const appName = "bossanova"

var (
	currentGOOS   = runtime.GOOS
	getenv        = os.Getenv
	userHomeDir   = os.UserHomeDir
	userConfigDir = os.UserConfigDir
	mkdirAll      = os.MkdirAll
)

// Dir returns the shared per-user state directory for local coordination files.
func Dir() (string, error) {
	base, err := baseDir(currentGOOS, getenv, userHomeDir, userConfigDir)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, appName)
	if err := mkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create app state directory %q: %w", dir, err)
	}
	return dir, nil
}

// Path returns a path inside Dir.
func Path(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid app state file name %q", name)
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func baseDir(goos string, getenv func(string) string, userHomeDir, userConfigDir func() (string, error)) (string, error) {
	var base string
	switch goos {
	case "darwin":
		home, err := userHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, "Library", "Application Support")
	case "windows":
		base = getenv("LOCALAPPDATA")
		if base == "" {
			var err error
			base, err = userConfigDir()
			if err != nil || base == "" {
				return "", fmt.Errorf("resolve user config directory: %w", err)
			}
		}
	default:
		base = getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := userHomeDir()
			if err != nil || home == "" {
				return "", fmt.Errorf("resolve home directory: %w", err)
			}
			base = filepath.Join(home, ".local", "state")
		}
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("invalid app state base path %q: must be absolute", base)
	}
	return base, nil
}
