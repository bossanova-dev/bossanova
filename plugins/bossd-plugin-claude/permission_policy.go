package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	unattendedPolicySettingsOpt = "unattended_policy_settings_path"
	policySettingsFilePrefix    = "claude-policy-"
	policySettingsFileSuffix    = ".json"
	fallbackPolicyDirPrefix     = "bossd-claude-policy-"
	stalePolicySettingsAge      = 24 * time.Hour
)

// policySettingsDefaultDir resolves the default private cache directory used
// when logPath is empty or sits inside the worktree. Tests override this in
// TestMain so no test touches the user's real cache directory.
var policySettingsDefaultDir = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bossanova", "claude-policy-settings"), nil
}

// writtenPolicySettings records a rendered flag-settings file and whether its
// parent directory is a per-launch fallback directory owned by this launch.
type writtenPolicySettings struct {
	path     string
	ownedDir bool
}

func (w writtenPolicySettings) cleanup() {
	if w.path == "" {
		return
	}
	if w.ownedDir {
		_ = os.RemoveAll(filepath.Dir(w.path))
		return
	}
	_ = os.Remove(w.path)
}

func (w *writtenPolicySettings) cleanupOnError(errp *error) {
	if *errp != nil {
		w.cleanup()
	}
}

type claudePolicySettings struct {
	Permissions claudePolicyPermissions `json:"permissions"`
}

type claudePolicyPermissions struct {
	Allow []string `json:"allow"`
}

// unattendedPolicyArgs returns the Claude CLI flags for a rendered unattended
// permission policy settings file.
func unattendedPolicyArgs(settingsPath string) []string {
	return []string{"--settings", settingsPath, "--permission-mode", "acceptEdits"}
}

// buildUnattendedPolicySettings renders a Claude flag-settings document from
// the daemon's UnattendedPermissionPolicy. Allow rules grant Bash(<prefix>:*)
// for each safe command prefix plus //-anchored Edit and Write rules scoped to
// the worktree root.
func buildUnattendedPolicySettings(policy *bossanovav1.UnattendedPermissionPolicy, fallbackWorktree string) claudePolicySettings {
	allow := make([]string, 0, len(policy.GetAllowedCommandPrefixes())+2)
	seen := make(map[string]struct{}, len(policy.GetAllowedCommandPrefixes())+4)
	add := func(rule string) {
		if _, ok := seen[rule]; ok {
			return
		}
		seen[rule] = struct{}{}
		allow = append(allow, rule)
	}

	for _, raw := range policy.GetAllowedCommandPrefixes() {
		prefix := strings.TrimSpace(raw)
		if !isSafeClaudeCommandPrefix(prefix) {
			continue
		}
		add(fmt.Sprintf("Bash(%s:*)", prefix))
	}

	worktree := strings.TrimSpace(policy.GetWorktreePath())
	if worktree == "" {
		worktree = strings.TrimSpace(fallbackWorktree)
	}
	for _, root := range policyWorktreeRoots(worktree) {
		trimmed := strings.TrimPrefix(filepath.ToSlash(root), "/")
		if trimmed == "" {
			continue
		}
		add(fmt.Sprintf("Edit(//%s/**)", trimmed))
		add(fmt.Sprintf("Write(//%s/**)", trimmed))
	}

	return claudePolicySettings{
		Permissions: claudePolicyPermissions{Allow: allow},
	}
}

// writeUnattendedPolicySettings serializes policy into a mode-0600 JSON file
// beside logPath (or in the private cache directory when logPath is empty or
// inside the worktree) and returns its path and cleanup handle.
func writeUnattendedPolicySettings(policy *bossanovav1.UnattendedPermissionPolicy, fallbackWorktree, logPath, sessionID string) (writtenPolicySettings, error) {
	worktree := strings.TrimSpace(policy.GetWorktreePath())
	if worktree == "" {
		worktree = strings.TrimSpace(fallbackWorktree)
	}
	dir, ownedDir, err := resolvePolicySettingsDir(logPath, worktree)
	if err != nil {
		return writtenPolicySettings{}, err
	}
	cleanupDirOnErr := func() {
		if ownedDir {
			_ = os.RemoveAll(dir)
		}
	}
	if !ownedDir {
		sweepStalePolicySettings(dir, time.Now())
	}

	payload, err := json.MarshalIndent(buildUnattendedPolicySettings(policy, fallbackWorktree), "", "  ")
	if err != nil {
		cleanupDirOnErr()
		return writtenPolicySettings{}, fmt.Errorf("marshal policy settings: %w", err)
	}
	payload = append(payload, '\n')

	pattern := policySettingsFilePrefix + "*" + policySettingsFileSuffix
	if safeID := sanitizePolicyFileToken(sessionID); safeID != "" {
		pattern = fmt.Sprintf("%s%s-*%s", policySettingsFilePrefix, safeID, policySettingsFileSuffix)
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		cleanupDirOnErr()
		return writtenPolicySettings{}, fmt.Errorf("create policy settings file: %w", err)
	}
	path := f.Name()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		cleanupDirOnErr()
		return writtenPolicySettings{}, fmt.Errorf("chmod policy settings file: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		cleanupDirOnErr()
		return writtenPolicySettings{}, fmt.Errorf("write policy settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		cleanupDirOnErr()
		return writtenPolicySettings{}, fmt.Errorf("close policy settings file: %w", err)
	}
	return writtenPolicySettings{path: path, ownedDir: ownedDir}, nil
}

func resolvePolicySettingsDir(logPath, worktree string) (dir string, ownedDir bool, err error) {
	trimmedLog := strings.TrimSpace(logPath)
	if trimmedLog != "" {
		candidate := filepath.Dir(trimmedLog)
		if absDir, absErr := filepath.Abs(candidate); absErr == nil {
			candidate = filepath.Clean(absDir)
		}
		if !isPathWithinWorktree(candidate, worktree) {
			if err := os.MkdirAll(candidate, 0o700); err != nil {
				return "", false, fmt.Errorf("create policy settings dir: %w", err)
			}
			return candidate, false, nil
		}
	}
	if defaultDir, dirErr := policySettingsDefaultDir(); dirErr == nil && strings.TrimSpace(defaultDir) != "" {
		candidate := strings.TrimSpace(defaultDir)
		if absDir, absErr := filepath.Abs(candidate); absErr == nil {
			candidate = filepath.Clean(absDir)
		}
		if !isPathWithinWorktree(candidate, worktree) {
			if err := os.MkdirAll(candidate, 0o700); err != nil {
				return "", false, fmt.Errorf("create default policy settings dir: %w", err)
			}
			return candidate, false, nil
		}
	}
	dir, err = os.MkdirTemp("", fallbackPolicyDirPrefix)
	if err != nil {
		return "", false, fmt.Errorf("create fallback policy settings dir: %w", err)
	}
	return dir, true, nil
}

// sweepStalePolicySettings removes regular claude-policy-*.json files older
// than stalePolicySettingsAge from dir. Only files matching the policy naming
// convention are considered; other files in dir (such as agent logs) are kept.
func sweepStalePolicySettings(dir string, now time.Time) int {
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, policySettingsFilePrefix) || !strings.HasSuffix(name, policySettingsFileSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < stalePolicySettingsAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		removed++
	}
	return removed
}

func sweepDefaultPolicySettingsDir(now time.Time) int {
	dir, err := policySettingsDefaultDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return 0
	}
	return sweepStalePolicySettings(dir, now)
}

// policyWorktreeRoots returns the cleaned absolute worktree path and, when the
// path resolves through a symlink (e.g. macOS /var -> /private/var), the
// resolved target as well so Claude's //-anchored path matcher covers both the
// logical worktree path and realpath.
func policyWorktreeRoots(worktree string) []string {
	if worktree == "" {
		return nil
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return nil
	}
	abs = filepath.Clean(abs)
	roots := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		resolved = filepath.Clean(resolved)
		if resolved != abs {
			roots = append(roots, resolved)
		}
	}
	return roots
}

func isPathWithinWorktree(candidate, worktree string) bool {
	if candidate == "" || worktree == "" {
		return false
	}
	candRoots := policyWorktreeRoots(candidate)
	wtRoots := policyWorktreeRoots(worktree)
	for _, wt := range wtRoots {
		for _, cand := range candRoots {
			rel, err := filepath.Rel(wt, cand)
			if err != nil {
				continue
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return true
			}
		}
	}
	return false
}

func isSafeClaudeCommandPrefix(word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i < len(word); i++ {
		c := word[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if i == 0 {
			if !isAlphaNum {
				return false
			}
			continue
		}
		if !isAlphaNum && c != '.' && c != '-' && c != '+' {
			return false
		}
	}
	return true
}

func sanitizePolicyFileToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 32; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	return b.String()
}
