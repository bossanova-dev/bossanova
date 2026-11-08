package git

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// WorktreeRepoBase is the repository-specific directory used by Create.
func WorktreeRepoBase(base, name string) string { return worktreeRepoBase(base, name) }

// WorktreeEntry is the safety-relevant state of a registered worktree.
type WorktreeEntry struct {
	Path             string
	Locked, Detached bool
}

// ListWorktrees reads NUL-delimited porcelain, preserving whitespace in paths.
func ListWorktrees(ctx context.Context, repo string) ([]WorktreeEntry, error) {
	data, err := preparedGitOutput(ctx, repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(string(data)), nil
}

func parseWorktrees(data string) []WorktreeEntry {
	var entries []WorktreeEntry
	for _, field := range strings.Split(data, "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok {
			entries = append(entries, WorktreeEntry{Path: path})
			continue
		}
		if len(entries) == 0 {
			continue
		}
		entry := &entries[len(entries)-1]
		if field == "locked" || strings.HasPrefix(field, "locked ") {
			entry.Locked = true
		}
		if field == "detached" {
			entry.Detached = true
		}
	}
	return entries
}

// WorktreeWithin checks both lexical and symlink-resolved containment.
func WorktreeWithin(base, path string) bool {
	realBase, err := resolveExistingPrefix(base)
	if err != nil {
		return false
	}
	realPath, err := resolveExistingPrefix(path)
	return err == nil && ensureWithin(realBase, realPath) == nil
}

// UnrecordedWorktreeHold returns a hold reason, or an error when safety is unknown.
func UnrecordedWorktreeHold(ctx context.Context, entry WorktreeEntry, now time.Time) (string, error) {
	if entry.Locked {
		return "locked", nil
	}
	private, err := preparedGitOutput(ctx, entry.Path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	gitDir := strings.TrimSpace(string(private))
	if _, err := os.Lstat(filepath.Join(gitDir, preparedWorktreeMarkerName)); err == nil {
		return "prepared", nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	changes, err := preparedGitOutput(ctx, entry.Path, "status", "--porcelain", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return "", err
	}
	if len(changes) > 0 {
		return "dirty", nil
	}
	loose, err := unrecordedPrivateCommits(ctx, entry.Path, gitDir)
	if err != nil {
		return "", err
	}
	if len(loose) > 0 {
		return "unreachable-commits", nil
	}
	for _, path := range []string{entry.Path, filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "index"), filepath.Join(gitDir, "logs", "HEAD")} {
		info, err := os.Stat(path)
		if os.IsNotExist(err) && path != entry.Path {
			continue
		}
		if err != nil {
			return "", err
		}
		if now.Sub(info.ModTime()) < 24*time.Hour {
			return "recent", nil
		}
	}
	return "", nil
}

// Only private roots disappear on removal. Shared branch reflogs survive and
// must not hold unrelated worktrees. Read both sides of each HEAD log entry:
// expiration can leave an abandoned commit only in an entry's old value.
func unrecordedPrivateCommits(ctx context.Context, path, gitDir string) ([]byte, error) {
	// Alternate backends (such as reftable) have no logs/HEAD file. Until we
	// can inventory their private recovery roots, cleanup must hold the worktree.
	storage, err := preparedGitOutput(ctx, path, "config", "--default=files", "--get", "extensions.refStorage")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(storage)) != "files" {
		return nil, fmt.Errorf("cannot inspect private reflog for reference storage %q", strings.TrimSpace(string(storage)))
	}
	log, err := os.ReadFile(filepath.Join(gitDir, "logs", "HEAD"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var roots strings.Builder
	roots.WriteString("HEAD\n")
	for _, line := range strings.Split(strings.TrimSuffix(string(log), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid private HEAD reflog entry")
		}
		for _, oid := range fields[:2] {
			if _, err := hex.DecodeString(oid); err != nil || (len(oid) != 40 && len(oid) != 64) {
				return nil, fmt.Errorf("invalid private HEAD reflog object")
			}
			if strings.Trim(oid, "0") != "" {
				roots.WriteString(oid + "\n")
			}
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-list", "--max-count=1", "--stdin", "--not", "--branches", "--tags", "--remotes")
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(roots.String())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("check private worktree commits: %w", err)
	}
	return out, nil
}

// RemoveWorktreeResidue removes only an exact hook-settings residue directory.
// It revalidates the contents and containment immediately before deletion.
func RemoveWorktreeResidue(base, path string) (bool, error) {
	if err := ensureWithin(base, path); err != nil {
		return false, err
	}
	if !IsWorktreeResidue(path) {
		return false, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("remove worktree residue: %w", err)
	}
	for parent := filepath.Dir(path); ensureWithin(base, parent) == nil; parent = filepath.Dir(parent) {
		if err := os.Remove(parent); err != nil {
			break
		}
	}
	return true, nil
}

// IsWorktreeResidue recognizes the hook writer's files and no other content.
func IsWorktreeResidue(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".claude" || !entries[0].IsDir() {
		return false
	}
	files, err := os.ReadDir(filepath.Join(path, ".claude"))
	if err != nil || len(files) == 0 {
		return false
	}
	for _, file := range files {
		if !file.Type().IsRegular() {
			return false
		}
		name := file.Name()
		if name != "settings.local.json" && !strings.HasPrefix(name, ".settings.local.json.tmp-") {
			return false
		}
	}
	return true
}
