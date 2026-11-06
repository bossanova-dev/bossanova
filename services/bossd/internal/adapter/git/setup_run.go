package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// setupResidueFile records the tracked files a setup script left modified,
// keyed by porcelain path, with a digest of what the script left in each. It
// sits under .boss/, which ensureGitInfoExclude hides from git status before
// any setup script runs.
const setupResidueFile = ".boss/setup-residue.json"

// absentDigest stands for a tracked file the setup script deleted.
const absentDigest = "absent"

type setupResidue struct {
	Files map[string]string `json:"files"`
}

// setupRunOpts names the per-call-site inputs of runAndLogSetup.
type setupRunOpts struct {
	Op       string    // "create" | "resurrect" | "create_from_existing_branch"; tags the log event
	RepoPath string    // main repo path
	Worktree string    // worktree the script runs in
	Branch   string    // branch checked out there; log context only
	Script   *string   // the stored setup_script value; nil or blank means "no setup step"
	Output   io.Writer // caller's live stream; nil falls back to os.Stderr inside setupscript
}

// runAndLogSetup runs the repo's configured setup script (when there is one)
// and records the run in the daemon log. All three setup call sites go through
// it, so the "is there a script" guard, the log sink, the timing and the log
// event cannot drift apart between create, resurrect, and
// create-from-existing-branch.
//
// The returned duration is the caller's wall-clock (parse + exec) — what
// CreateResult.SetupScriptDuration reports — and is zero when no script ran.
// The error is the setup failure, which every call site treats as non-fatal;
// it is already logged here.
func (m *Manager) runAndLogSetup(ctx context.Context, opts setupRunOpts) (time.Duration, error) {
	if opts.Script == nil || strings.TrimSpace(*opts.Script) == "" {
		return 0, nil
	}
	started := time.Now()
	runDuration, err := runSetupScript(ctx, opts.RepoPath, opts.Worktree, *opts.Script, m.LoginShell,
		opts.Output, setupTailLogWriter{logger: m.logger, worktree: opts.Worktree})
	m.logSetupScriptRun(opts.Op, opts.Worktree, opts.Branch, runDuration, err)
	m.recordSetupResidue(ctx, opts)
	return time.Since(started), err
}

// recordSetupResidue snapshots the tracked files the setup script left
// modified, so finalize can tell them apart from the agent's own edits. A
// setup script that rewrites a lockfile otherwise makes every unattended run
// look like it left uncommitted work. Best-effort: a failure is logged and the
// worktree simply has no residue record.
func (m *Manager) recordSetupResidue(ctx context.Context, opts setupRunOpts) {
	status, err := m.Status(ctx, opts.Worktree)
	if err != nil {
		m.logger.Warn().Err(err).Str("worktree", opts.Worktree).Msg("setup residue: git status failed")
		return
	}
	var paths []string
	for line := range strings.SplitSeq(status, "\n") {
		if path, ok := trackedPorcelainPath(line); ok {
			paths = append(paths, path)
		}
	}
	recorded, err := WriteSetupResidue(opts.Worktree, paths)
	if err != nil {
		m.logger.Warn().Err(err).Str("worktree", opts.Worktree).Msg("setup residue: write record failed")
		return
	}
	if len(recorded) > 0 {
		m.logger.Warn().
			Str("op", opts.Op).
			Str("worktree", opts.Worktree).
			Strs("paths", recorded).
			Msg("setup script left tracked files modified; finalize will not count them as session output")
	}
}

// WriteSetupResidue records paths (relative to worktreePath, as git status
// prints them) with a digest of their current content, replacing any earlier
// record. Unreadable paths are left out. With nothing to record it removes the
// record. It returns the recorded paths, sorted.
func WriteSetupResidue(worktreePath string, paths []string) ([]string, error) {
	files := map[string]string{}
	for _, path := range paths {
		if digest, ok := contentDigest(filepath.Join(worktreePath, path)); ok {
			files[path] = digest
		}
	}
	record := filepath.Join(worktreePath, setupResidueFile)
	if len(files) == 0 {
		if err := os.Remove(record); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	body, err := json.Marshal(setupResidue{Files: files})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(record, body, 0o600); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(files)), nil
}

// SetupResidue returns the tracked paths the setup script left modified that
// still hold exactly what the script left. Nobody has edited them since, so
// they are setup output rather than session work. A missing or unreadable
// record yields nil, so every file counts as session output.
func SetupResidue(worktreePath string) []string {
	if worktreePath == "" {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(worktreePath, setupResidueFile))
	if err != nil {
		return nil
	}
	var record setupResidue
	if json.Unmarshal(body, &record) != nil {
		return nil
	}
	var unchanged []string
	for path, recorded := range record.Files {
		if digest, ok := contentDigest(filepath.Join(worktreePath, path)); ok && digest == recorded {
			unchanged = append(unchanged, path)
		}
	}
	slices.Sort(unchanged)
	return unchanged
}

// trackedPorcelainPath returns the path of a `git status --porcelain` line
// that changes a tracked file, sliced the way finalize's managed-path filter
// slices it. Renames and quoted paths are skipped: neither round-trips through
// that filter.
func trackedPorcelainPath(line string) (string, bool) {
	if len(line) < 4 || strings.HasPrefix(line, "??") || strings.HasPrefix(line, "!!") {
		return "", false
	}
	path := strings.TrimSpace(line[3:])
	if path == "" || strings.Contains(path, " -> ") || strings.HasPrefix(path, `"`) {
		return "", false
	}
	return path, true
}

func contentDigest(path string) (string, bool) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return absentDigest, true
	}
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
}
