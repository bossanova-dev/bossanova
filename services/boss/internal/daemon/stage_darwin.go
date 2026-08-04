//go:build darwin

package daemon

import (
	"fmt"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
)

// EnsureStaged copies bossd to the stable per-user app-data path when needed.
// macOS TCC grants follow the executable's resolved real path, so a versioned
// package-manager path (or a symlink to one) loses its grant after an upgrade.
// Keeping a real file at one stable path while copying the same signed bossd
// preserves both that path and the binary's signing identity across upgrades.
func EnsureStaged(sourcePath string) (string, error) {
	if err := validatePath(sourcePath); err != nil {
		return "", err
	}

	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("get default app data dir: %w", err)
	}
	stagedPath := daemonbin.StagedPath(appDataDir)
	if sourcePath == stagedPath {
		return stagedPath, nil
	}

	needsStage, _, err := daemonbin.NeedsStage(sourcePath, stagedPath)
	if err != nil {
		return "", fmt.Errorf("check staged bossd: %w", err)
	}
	if needsStage {
		if err := daemonbin.Stage(sourcePath, stagedPath); err != nil {
			return "", fmt.Errorf("stage bossd: %w", err)
		}
	}
	return stagedPath, nil
}
