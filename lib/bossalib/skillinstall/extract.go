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

	"github.com/gofrs/flock"
)

// isBossSkill returns true for skill directory names that belong to boss
// (either "boss" exactly or prefixed with "boss-").
func isBossSkill(name string) bool {
	return name == "boss" || strings.HasPrefix(name, "boss-")
}

// Namespace is the subdirectory under ~/.claude/skills/ where boss skill
// files are stored. Symlinks are created from the parent directory into
// this namespace, so the payload stays in one owned subtree while each
// skill still resolves by its bare name at the top level.
const Namespace = "bossanova"

const updateLockFile = ".bossanova-skills.lock"

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

// DriftKind classifies a single installed-vs-payload difference. A caller that
// only has the path cannot tell an installed file that is missing entirely from
// one whose bytes merely changed, which is the discrimination consumers of the
// drift report need in order to choose a remedy.
type DriftKind string

const (
	// DriftAbsent is a payload file with no installed counterpart.
	DriftAbsent DriftKind = "absent"
	// DriftContent is an installed file whose bytes differ from the payload.
	DriftContent DriftKind = "content"
	// DriftMode is an installed file whose executable bit was lost.
	DriftMode DriftKind = "mode"
	// DriftUnexpected is an installed file or skill directory the payload does
	// not contain.
	DriftUnexpected DriftKind = "unexpected"
	// DriftBrokenSymlink is a top-level skill symlink that is missing or points
	// somewhere other than the namespaced payload directory.
	DriftBrokenSymlink DriftKind = "broken-symlink"
)

// DriftEntry is one installed-vs-payload difference: the slash-separated path
// relative to the payload root, plus what kind of difference it is.
type DriftEntry struct {
	Path string
	Kind DriftKind
}

// DriftReport is the structured result of one drift walk. Compared counts the
// payload files the walk checked, so a clean report can state its own coverage
// instead of being a silent success.
type DriftReport struct {
	Installed bool
	Compared  int
	Entries   []DriftEntry
}

// Paths projects the report onto the sorted slash-separated path slice that
// SourceDriftPaths has always returned. The projection is the only producer of
// that slice, so the two cannot disagree.
func (r DriftReport) Paths() []string {
	paths := make([]string, 0, len(r.Entries))
	for _, entry := range r.Entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

// SourceDriftReport reports each installed path that differs from the canonical
// skill sources at srcRoot, carrying the kind of each difference. It reports an
// uninstalled tree as Installed false with no entries.
func SourceDriftReport(dir, srcRoot string) (DriftReport, error) {
	return driftReport(dir, os.DirFS(srcRoot))
}

// SourceDriftPaths reports the installed paths that differ from the canonical
// skill sources at srcRoot, as slash-separated paths relative to the payload
// root. It returns an empty slice when no skills are installed.
func SourceDriftPaths(dir, srcRoot string) ([]string, error) {
	report, err := driftReport(dir, os.DirFS(srcRoot))
	if err != nil {
		return nil, err
	}
	return report.Paths(), nil
}

// SourceManifest returns the canonical skill-source manifest at srcRoot.
func SourceManifest(srcRoot string) (string, error) {
	return Manifest(os.DirFS(srcRoot))
}

// EnsureUpdatedFromSource refreshes an installed skill tree from a checkout's
// canonical skill sources rather than from an embedded payload. It is the write
// counterpart of SourceDrift: a process running inside a checkout treats that
// checkout as authoritative, so a binary whose embed has fallen behind repairs
// the install instead of re-staling it.
func EnsureUpdatedFromSource(dir, srcRoot string) (bool, error) {
	return EnsureUpdated(dir, os.DirFS(srcRoot))
}

// ExtractFromSource is Extract against a checkout's skill sources.
func ExtractFromSource(dir, srcRoot string) error { return Extract(dir, os.DirFS(srcRoot)) }

// InstalledNeedsUpdate reports whether a boss skill tree is installed and, if
// it is, whether it differs from fsys. When a writer has already created its
// per-agent update lock, it holds a shared lock across both observations so a
// concurrent Extract cannot be mistaken for a current tree while its namespace
// is temporarily absent. A legacy install without a lock is checked
// optimistically and retried if a writer creates one during the check.
func InstalledNeedsUpdate(dir string, fsys fs.FS) (installed, needsUpdate bool, err error) {
	if _, statErr := os.Stat(dir); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, false, nil
		}
		return false, false, statErr
	}
	for {
		lock, locked, lockErr := acquireExistingUpdateLock(dir)
		if lockErr != nil {
			return false, false, lockErr
		}
		if locked {
			installed = IsInstalled(dir)
			if installed {
				needsUpdate, err = needsUpdateLocked(dir, fsys)
			}
			if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
				return false, false, fmt.Errorf("release skill update lock: %w", unlockErr)
			}
			return installed, needsUpdate, err
		}

		installed = IsInstalled(dir)
		if installed {
			needsUpdate, err = needsUpdateLocked(dir, fsys)
			if err != nil {
				return false, false, err
			}
		}
		if _, statErr := os.Stat(filepath.Join(dir, updateLockFile)); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return false, false, fmt.Errorf("stat skill update lock: %w", statErr)
		}
		return installed, needsUpdate, nil
	}
}

// NeedsUpdate reports whether an already-installed boss skill tree differs
// from the embedded payload or has a broken top-level symlink layout.
func NeedsUpdate(dir string, fsys fs.FS) (bool, error) {
	_, needsUpdate, err := InstalledNeedsUpdate(dir, fsys)
	return needsUpdate, err
}

func driftReport(dir string, fsys fs.FS) (DriftReport, error) {
	if _, statErr := os.Stat(dir); statErr != nil {
		if os.IsNotExist(statErr) {
			return DriftReport{}, nil
		}
		return DriftReport{}, statErr
	}
	for {
		lock, locked, lockErr := acquireExistingUpdateLock(dir)
		if lockErr != nil {
			return DriftReport{}, lockErr
		}
		if locked {
			if !IsInstalled(dir) {
				if unlockErr := lock.Unlock(); unlockErr != nil {
					return DriftReport{}, fmt.Errorf("release skill update lock: %w", unlockErr)
				}
				return DriftReport{}, nil
			}
			report, err := driftReportLocked(dir, fsys)
			if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
				return DriftReport{}, fmt.Errorf("release skill update lock: %w", unlockErr)
			}
			return report, err
		}

		if !IsInstalled(dir) {
			return DriftReport{}, nil
		}
		report, err := driftReportLocked(dir, fsys)
		if err != nil {
			return DriftReport{}, err
		}
		if _, statErr := os.Stat(filepath.Join(dir, updateLockFile)); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return DriftReport{}, fmt.Errorf("stat skill update lock: %w", statErr)
		}
		return report, nil
	}
}

func needsUpdateLocked(dir string, fsys fs.FS) (bool, error) {
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
		modeDrift, err := executableModeDrift(installedPath, file)
		if err != nil {
			return false, err
		}
		if modeDrift {
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

func driftReportLocked(dir string, fsys fs.FS) (DriftReport, error) {
	files, err := embeddedFiles(fsys)
	if err != nil {
		return DriftReport{}, err
	}

	// Each branch below owns a disjoint key space — payload-relative file paths,
	// unexpected installed paths, and bare top-level skill names — so a path is
	// classified exactly once and the first kind recorded is the only one.
	drift := map[string]DriftKind{}
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
				drift[file.rel] = DriftAbsent
				continue
			}
			return DriftReport{}, err
		}
		if !bytes.Equal(data, file.data) {
			drift[file.rel] = DriftContent
			continue
		}
		modeDrift, err := executableModeDrift(installedPath, file)
		if err != nil {
			return DriftReport{}, err
		}
		if modeDrift {
			drift[file.rel] = DriftMode
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
				drift[filepath.ToSlash(rel)+"/"] = DriftUnexpected
			}
			return nil
		}
		rel, err := filepath.Rel(nsDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := expectedFiles[rel]; !ok {
			drift[rel] = DriftUnexpected
		}
		return nil
	}); err != nil {
		return DriftReport{}, err
	}

	for skill := range expectedSkills {
		link := filepath.Join(dir, skill)
		target, err := os.Readlink(link)
		if err != nil || target != filepath.Join(Namespace, skill) {
			drift[skill] = DriftBrokenSymlink
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// The whole installed tree is gone, so a reinstall only adds files.
			drift[Namespace+"/"] = DriftAbsent
		} else {
			return DriftReport{}, err
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if isBossSkill(name) && !expectedSkills[name] {
			drift[name] = DriftUnexpected
		}
	}

	paths := make([]string, 0, len(drift))
	for path := range drift {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	report := DriftReport{Installed: true, Compared: len(files), Entries: make([]DriftEntry, 0, len(paths))}
	for _, path := range paths {
		report.Entries = append(report.Entries, DriftEntry{Path: path, Kind: drift[path]})
	}
	return report, nil
}

func executableModeDrift(path string, file embeddedFile) (bool, error) {
	if !executableSkillFile("skills/"+file.rel) && !hasShebang(file.data) {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o111 == 0, nil
}

// EnsureUpdated refreshes installed boss skills only when the installed tree
// differs from the embedded payload. It does not install into an empty target.
func EnsureUpdated(dir string, fsys fs.FS) (updated bool, err error) {
	// Check only that the target directory exists before acquiring the lock:
	// acquireUpdateLock creates it, while a concurrent Extract can temporarily
	// remove Namespace. Recheck installation after the lock serializes that gap.
	if _, statErr := os.Stat(dir); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	}

	if !IsInstalled(dir) {
		if _, lockErr := os.Stat(filepath.Join(dir, updateLockFile)); lockErr != nil {
			if os.IsNotExist(lockErr) {
				return false, nil
			}
			return false, lockErr
		}
	}

	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return false, err
	}
	defer func() {
		if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
			updated = false
			err = fmt.Errorf("release skill update lock: %w", unlockErr)
		}
	}()
	if !IsInstalled(dir) {
		return false, nil
	}

	needs, err := needsUpdateLocked(dir, fsys)
	if err != nil || !needs {
		return false, err
	}
	if err := extract(dir, fsys); err != nil {
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
func Extract(dir string, fsys fs.FS) (err error) {
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
			err = fmt.Errorf("release skill update lock: %w", unlockErr)
		}
	}()

	return extract(dir, fsys)
}

// acquireUpdateLock serializes all destructive skill-tree rewrites for one
// agent directory. The lock lives alongside the namespace so Extract's cleanup
// cannot remove it while another process is waiting.
func acquireUpdateLock(dir string) (*flock.Flock, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create skill directory for update lock: %w", err)
	}
	lock := flock.New(filepath.Join(dir, updateLockFile))
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("acquire skill update lock: %w", err)
	}
	return lock, nil
}

// acquireExistingUpdateLock takes a shared lock only when a writer has already
// created the durable lock file. Read-only checks must not leave a new lock
// artifact in legacy installed skill directories.
func acquireExistingUpdateLock(dir string) (*flock.Flock, bool, error) {
	path := filepath.Join(dir, updateLockFile)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat skill update lock: %w", err)
	}
	lock := flock.New(path, flock.SetFlag(os.O_RDONLY))
	if err := lock.RLock(); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("acquire existing skill update lock: %w", err)
	}
	return lock, true, nil
}

func extract(dir string, fsys fs.FS) error {
	nsDir := filepath.Join(dir, Namespace)

	// Remove stale boss-* entries (symlinks or real directories) in the parent directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read skill directory: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !isBossSkill(name) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale skill %q: %w", name, err)
		}
	}

	// Remove the entire namespace directory so stale skills are cleaned up.
	if err := os.RemoveAll(nsDir); err != nil {
		return fmt.Errorf("remove skill namespace: %w", err)
	}

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
	entries, err = os.ReadDir(nsDir)
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
