package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scanHandoffDir reads the handoff directory and returns the path to the
// newest file modified after `since`. Returns an empty string if no new
// files are found. The directory path must not contain "..".
func scanHandoffDir(dir string, since time.Time) (string, error) {
	if strings.Contains(dir, "..") {
		return "", fmt.Errorf("handoff directory must not contain '..': %s", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("handoff directory does not exist: %s", dir)
		}
		return "", fmt.Errorf("read handoff directory: %w", err)
	}

	var newestPath string
	var newestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if modTime.After(since) && modTime.After(newestTime) {
			newestPath = filepath.Join(dir, entry.Name())
			newestTime = modTime
		}
	}

	return newestPath, nil
}
