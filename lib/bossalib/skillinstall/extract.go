package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// SourceRelPath is the repository-relative directory containing the canonical
// skill payload. It mirrors Makefile's SKILLS_SRC_DIR and must move with it.
const SourceRelPath = "services/boss/internal/skillinstall"

// Agent identifies a coding agent with a global skill directory.
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// DefaultDir returns the global Claude skills directory (~/.claude/skills).
func DefaultDir() (string, error) {
	return DirForAgent(AgentClaude)
}

// DirForAgent returns the global skill directory for a supported coding agent.
func DirForAgent(agent Agent) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch agent {
	case AgentClaude:
		return filepath.Join(home, ".claude", "skills"), nil
	case AgentCodex:
		return filepath.Join(home, ".codex", "skills"), nil
	default:
		return "", fmt.Errorf("unsupported agent %q", agent)
	}
}

// IsInstalled returns true if the bossanova namespace directory exists in dir
// and contains at least one boss-* subdirectory.
func IsInstalled(dir string) bool {
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

// Manifest returns a deterministic hash of the embedded boss skill payload.
func Manifest(fsys fs.FS) (string, error) {
	files, err := embeddedFiles(fsys)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, file := range files {
		_, _ = h.Write([]byte(file.rel))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(file.data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FindSourceRoot finds the canonical skill-source directory at or above
// startDir. It returns an absolute path and true when found, or ("", false)
// when this checkout does not contain skill sources.
func FindSourceRoot(startDir string) (string, bool) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false
	}
	for {
		srcRoot := filepath.Join(dir, SourceRelPath)
		srcInfo, srcErr := os.Lstat(srcRoot)
		skillsInfo, skillsErr := os.Lstat(filepath.Join(srcRoot, "skills"))
		if srcErr == nil && skillsErr == nil && srcInfo.IsDir() && skillsInfo.IsDir() {
			return srcRoot, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// SourceDrift reports whether installed skills differ from the canonical skill
// sources at srcRoot. It returns false when no skills are installed.
func SourceDrift(dir, srcRoot string) (bool, error) {
	return NeedsUpdate(dir, os.DirFS(srcRoot))
}

// SourceManifest returns the canonical skill-source manifest at srcRoot.
func SourceManifest(srcRoot string) (string, error) {
	return Manifest(os.DirFS(srcRoot))
}

// NeedsUpdate reports whether an already-installed boss skill tree differs
// from the embedded payload or has a broken top-level symlink layout.
func NeedsUpdate(dir string, fsys fs.FS) (bool, error) {
	if !IsInstalled(dir) {
		return false, nil
	}
	files, err := embeddedFiles(fsys)
	if err != nil {
		return false, err
	}

	expectedFiles := make(map[string][]byte, len(files))
	expectedSkills := map[string]bool{}
	for _, file := range files {
		expectedFiles[file.rel] = file.data
		skill := strings.Split(file.rel, "/")[0]
		if isBossSkill(skill) {
			expectedSkills[skill] = true
		}

		installedPath := filepath.Clean(filepath.Join(dir, Namespace, filepath.FromSlash(file.rel)))
		data, err := os.ReadFile(installedPath)
		if err != nil {
			if os.IsNotExist(err) {
				return true, nil
			}
			return false, err
		}
		if !bytes.Equal(data, file.data) {
			return true, nil
		}
	}

	nsDir := filepath.Join(dir, Namespace)
	if err := filepath.WalkDir(nsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == nsDir {
				return nil
			}
			rel, err := filepath.Rel(nsDir, path)
			if err != nil {
				return err
			}
			if filepath.Dir(rel) == "." && isBossSkill(filepath.Base(rel)) && !expectedSkills[filepath.Base(rel)] {
				return errNeedsUpdate
			}
			return nil
		}
		rel, err := filepath.Rel(nsDir, path)
		if err != nil {
			return err
		}
		if _, ok := expectedFiles[filepath.ToSlash(rel)]; !ok {
			return errNeedsUpdate
		}
		return nil
	}); err != nil {
		if err == errNeedsUpdate {
			return true, nil
		}
		return false, err
	}

	for skill := range expectedSkills {
		link := filepath.Join(dir, skill)
		target, err := os.Readlink(link)
		if err != nil {
			return true, nil
		}
		if target != filepath.Join(Namespace, skill) {
			return true, nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if isBossSkill(name) && !expectedSkills[name] {
			return true, nil
		}
	}
	return false, nil
}

// EnsureUpdated refreshes installed boss skills only when the installed tree
// differs from the embedded payload. It does not install into an empty target.
func EnsureUpdated(dir string, fsys fs.FS) (bool, error) {
	needs, err := NeedsUpdate(dir, fsys)
	if err != nil || !needs {
		return false, err
	}
	if err := Extract(dir, fsys); err != nil {
		return false, err
	}
	return true, nil
}

type embeddedFile struct {
	rel  string
	data []byte
}

var errNeedsUpdate = errors.New("skills need update")

func embeddedFiles(fsys fs.FS) ([]embeddedFile, error) {
	var files []embeddedFile
	if err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read embedded skill: %w", err)
		}
		files = append(files, embeddedFile{
			rel:  strings.TrimPrefix(path, "skills/"),
			data: data,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

// hasShebang reports whether data begins with a "#!" interpreter line. This
// catches extensionless embedded helper scripts that executableSkillFile's
// filename heuristic misses but skill prose invokes directly. Markdown headings
// begin with "# " (hash-space), never "#!", so this does not over-mark docs.
func hasShebang(data []byte) bool {
	return bytes.HasPrefix(data, []byte("#!"))
}

func executableSkillFile(path string) bool {
	if strings.HasSuffix(path, ".sh") {
		return true
	}
	if !strings.Contains(path, "/scripts/") {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cjs", ".js", ".jsx", ".mjs", ".ts", ".tsx":
		return true
	default:
		return false
	}
}

// Extract writes embedded skill files from fsys into dir/bossanova/
// and creates symlinks from dir/boss-* → bossanova/boss-* so that Claude
// discovers them as top-level skills.
//
// It removes stale boss-* symlinks and the bossanova/ directory first so
// that renamed or deleted skills don't persist across upgrades.
func Extract(dir string, fsys fs.FS) error {
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
		// path is "skills/boss-build/SKILL.md"
		// Strip leading "skills/" to get "boss-build/SKILL.md"
		rel := strings.TrimPrefix(path, "skills/")
		destPath := filepath.Join(nsDir, rel)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
			return fmt.Errorf("create skill dir: %w", err)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read embedded skill: %w", err)
		}
		// Use 0o755 for scripts so they remain executable after extraction.
		// go:embed drops the on-disk mode, so exec bits are re-derived here from
		// the filename heuristic plus a shebang probe — extensionless helper
		// scripts (e.g. support/.../scripts/review-package) are invoked directly
		// by skill prose and would otherwise hit "permission denied".
		mode := os.FileMode(0o644)
		if executableSkillFile(path) || hasShebang(data) {
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
