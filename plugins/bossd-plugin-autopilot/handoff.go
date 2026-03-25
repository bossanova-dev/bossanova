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

// synthesizeHandoff creates a minimal handoff file when the recovery agent
// succeeds but doesn't write one. The file contains enough context (plan
// reference, leg number, synthesized marker) for the resume step to pick up.
func synthesizeHandoff(dir, planPath string, leg int) (string, error) {
	if strings.Contains(dir, "..") {
		return "", fmt.Errorf("handoff directory must not contain '..': %s", dir)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create handoff directory: %w", err)
	}

	name := fmt.Sprintf("%s-synthesized-handoff-leg-%d.md", time.Now().Format("2006-01-02-1504"), leg)
	path := filepath.Join(dir, name)

	content := fmt.Sprintf(`# Synthesized Handoff — Leg %d

Synthesized: true

## Context

This handoff was synthesized by the orchestrator because the recovery agent
completed successfully but did not write a handoff file to disk.

## Plan

%s

## Status

The previous flight leg completed. Review recent git history and bd task
status for details on what was accomplished.
`, leg, planPath)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write synthesized handoff: %w", err)
	}

	return path, nil
}
