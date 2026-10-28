package accountwiring

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReapOrphanedUsageProbeHomes removes old probe directories directly under dir.
// Symlinks and unrelated entries are preserved, including nested probe directories.
func ReapOrphanedUsageProbeHomes(dir string, olderThan time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read usage probe directory: %w", err)
	}
	count := 0
	var reapErr error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "boss-usage-probe-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			reapErr = errors.Join(reapErr, fmt.Errorf("stat usage probe %q: %w", entry.Name(), err))
			continue
		}
		if !info.ModTime().Before(now.Add(-olderThan)) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			reapErr = errors.Join(reapErr, fmt.Errorf("remove usage probe %q: %w", entry.Name(), err))
			continue
		}
		count++
	}
	return count, reapErr
}
