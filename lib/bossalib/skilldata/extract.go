package skilldata

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// isBossSkill returns true for skill directory names that belong to boss
// (either "boss" exactly or prefixed with "boss-").
func isBossSkill(name string) bool {
	return name == "boss" || strings.HasPrefix(name, "boss-")
}

// Namespace is the subdirectory under ~/.claude/skills/ where boss skill
// files are stored. Symlinks are created from the parent directory into
// this namespace, mirroring how gstack organises its skills.
const Namespace = "bossanova"

// DefaultSkillsDir returns the global Claude skills directory (~/.claude/skills).
func DefaultSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// BossSkillsInstalled returns true if the bossanova namespace directory
// exists in dir and contains at least one boss-* subdirectory.
func BossSkillsInstalled(dir string) bool {
	nsDir := filepath.Join(dir, Namespace)
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && isBossSkill(e.Name()) {
			return true
		}
	}
	return false
}

// ExtractSkills writes embedded skill files from fsys into dir/bossanova/
// and creates symlinks from dir/boss-* → bossanova/boss-* so that Claude
// discovers them as top-level skills.
//
// It removes stale boss-* symlinks and the bossanova/ directory first so
// that renamed or deleted skills don't persist across upgrades.
func ExtractSkills(dir string, fsys fs.FS) error {
	nsDir := filepath.Join(dir, Namespace)

	// Remove stale boss-* entries (symlinks or real directories) in the parent directory.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !isBossSkill(name) {
				continue
			}
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}

	// Remove the entire namespace directory so stale skills are cleaned up.
	_ = os.RemoveAll(nsDir)

	// Extract embedded skill files into the namespace directory.
	if err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// path is "skills/boss-implement/SKILL.md"
		// Strip leading "skills/" to get "boss-implement/SKILL.md"
		rel := strings.TrimPrefix(path, "skills/")
		destPath := filepath.Join(nsDir, rel)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("create skill dir: %w", err)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read embedded skill: %w", err)
		}
		// Use 0o755 for scripts so they remain executable after extraction.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(destPath, data, mode)
	}); err != nil {
		return err
	}

	// Create symlinks from dir/boss-* → bossanova/boss-* for each skill.
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		return fmt.Errorf("read namespace dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !isBossSkill(e.Name()) {
			continue
		}
		// Use a relative target so the symlink works regardless of home dir.
		target := filepath.Join(Namespace, e.Name())
		link := filepath.Join(dir, e.Name())
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("create skill symlink: %w", err)
		}
	}

	return nil
}
