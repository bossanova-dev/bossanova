package skillinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExtractRecordedPreserving copies installed drift to backupDir before replacing
// the tree and stamping record. Backup paths are relative to dir (including the
// namespace), so legacy top-level directories cannot collide with payload paths.
// A copy failure leaves the installed tree untouched. The backup directory must
// be outside dir and must not already exist when there is anything to preserve.
func ExtractRecordedPreserving(dir string, fsys fs.FS, record PayloadRecord, backupDir string) ([]string, error) {
	return extractRecordedPreserving(dir, fsys, record, backupDir, copyPreservedPath)
}

func extractRecordedPreserving(dir string, fsys fs.FS, record PayloadRecord, backupDir string, copyPath func(string, string) error) (preserved []string, err error) {
	installedRoot, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	backupRoot, err := filepath.Abs(backupDir)
	if err != nil {
		return nil, err
	}
	if rel, relErr := filepath.Rel(installedRoot, backupRoot); relErr != nil || rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		return nil, fmt.Errorf("skill backup must be outside installed tree")
	}
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
			err = fmt.Errorf("release skill update lock: %w", unlockErr)
		}
	}()
	// Unlike the public report, this also walks legacy trees with only top-level
	// directories. extract removes those whether or not IsInstalled reports true.
	report, err := preservationReport(dir, fsys)
	if err != nil {
		return nil, err
	}
	paths, err := preservationPaths(dir, report)
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 {
		if err = os.MkdirAll(filepath.Dir(backupRoot), 0700); err != nil {
			return nil, fmt.Errorf("create skill backup parent: %w", err)
		}
		if err = os.Mkdir(backupRoot, 0700); err != nil {
			return nil, fmt.Errorf("create skill backup: %w", err)
		}
		for _, rel := range paths {
			if err = copyPath(filepath.Join(dir, rel), filepath.Join(backupRoot, rel)); err != nil {
				return nil, fmt.Errorf("preserve skill path %s: %w", rel, err)
			}
			preserved = append(preserved, filepath.ToSlash(rel))
		}
	}
	return preserved, extract(dir, fsys, &record)
}

func preservationReport(dir string, fsys fs.FS) (DriftReport, error) {
	if _, err := os.Lstat(filepath.Join(dir, Namespace)); err == nil {
		return driftReportLocked(dir, fsys)
	} else if !os.IsNotExist(err) {
		return DriftReport{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return DriftReport{}, err
	}
	report := DriftReport{}
	for _, entry := range entries {
		if isBossSkill(entry.Name()) {
			report.Entries = append(report.Entries, DriftEntry{Path: entry.Name(), Kind: DriftUnexpected})
		}
	}
	return report, nil
}

func preservationPaths(dir string, report DriftReport) ([]string, error) {
	var paths []string
	for _, entry := range report.Entries {
		rel := filepath.FromSlash(strings.TrimSuffix(entry.Path, "/"))
		candidates := []string{filepath.Join(Namespace, rel)}
		if !strings.Contains(entry.Path, "/") {
			// Bare names can identify both namespace-root files and top-level
			// links or legacy entries. The drift report combines those names,
			// so preserve both existing paths rather than guessing their origin.
			candidates = append(candidates, rel)
		}
		for _, candidate := range candidates {
			// Missing payload files add no bytes to preserve; Lstat still sees
			// dangling symlinks and directories displaced by symlinks.
			if _, err := os.Lstat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return nil, err
			}
			paths = append(paths, candidate)
		}
	}
	sort.Strings(paths)
	unique := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, rel := range paths {
		covered := seen[rel]
		for parent := filepath.Dir(rel); !covered && parent != "."; parent = filepath.Dir(parent) {
			covered = seen[parent]
		}
		if !covered {
			unique = append(unique, rel)
			seen[rel] = true
		}
	}
	return unique, nil
}

func copyPreservedPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPreservedPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dst, info.Mode())
	case info.Mode().IsRegular():
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return err
		}
		return os.Chmod(dst, info.Mode())
	default:
		return fmt.Errorf("cannot preserve special file %s (%s)", src, info.Mode())
	}
}
