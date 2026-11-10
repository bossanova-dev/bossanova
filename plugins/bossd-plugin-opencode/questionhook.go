package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// questionHookJS is the static opencode plugin asset that feeds BOS-485's
// question-signal receiver. It is embedded rather than generated per run: it
// reads the loopback port and bearer token from process.env at RUNTIME
// (BOSS_HOOK_PORT / BOSS_HOOK_TOKEN), so the same bytes are written for every
// session, no secret ever lands in the worktree, and re-writing it is a no-op.
// See bossd-question.js for the event contract.
//
//go:embed bossd-question.js
var questionHookJS []byte

const (
	// hookEnvPort / hookEnvToken are the ExtraEnv keys bossd uses to hand the
	// agent process its loopback hook context. BOTH must be present for the
	// hook to be injected: without either one the injected JS could not reach
	// (or authenticate to) the receiver, so writing it would only litter the
	// worktree.
	hookEnvPort  = "BOSS_HOOK_PORT"
	hookEnvToken = "BOSS_HOOK_TOKEN"

	// questionHookRootDir is the opencode config directory inside the worktree.
	// questionHookPluginsDir (PLURAL) is the auto-load directory: opencode loads
	// every local .js/.ts there at startup, including in headless `opencode run`.
	questionHookRootDir    = ".opencode"
	questionHookPluginsDir = "plugins"

	// questionHookFileName is bossd's own file inside that directory. It is
	// namespaced so a user-authored sibling plugin is never touched.
	questionHookFileName = "bossd-question.js"
)

// questionHookPath returns the absolute path of the injected asset for workDir.
func questionHookPath(workDir string) string {
	return filepath.Join(workDir, questionHookRootDir, questionHookPluginsDir, questionHookFileName)
}

// installQuestionHook writes the embedded opencode event-hook plugin into
// workDir/.opencode/plugins/, creating the directories as needed.
//
// It is a no-op (installed=false, nil error) unless workDir is set and BOTH
// hook env vars are present in extraEnv — an opencode run without loopback
// context must still start normally, just with no question signal. The write is
// unconditional (not read-compare-write) because the asset is static: an
// existing copy from a crashed run is simply overwritten, which makes injection
// idempotent across retries and resumes.
//
// The returned bool reports whether a file was written, so the caller knows
// whether to arm the run-end cleanup.
func installQuestionHook(workDir string, extraEnv map[string]string) (bool, error) {
	if workDir == "" {
		return false, nil
	}
	if extraEnv[hookEnvPort] == "" || extraEnv[hookEnvToken] == "" {
		return false, nil
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return false, fmt.Errorf("open question hook worktree: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := questionHookParents(root, true); err != nil {
		return false, err
	}
	name := questionHookPath("")
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return false, fmt.Errorf("unsafe question hook file %q", name)
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|questionHookOpenFlags, 0o600)
	if err != nil {
		return false, fmt.Errorf("open question hook: %w", err)
	}
	_, writeErr := file.Write(questionHookJS)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return false, fmt.Errorf("write question hook: %w", err)
	}
	return true, nil
}

// removeQuestionHook deletes the injected asset and prunes the directories it
// created, leaving any pre-existing user `.opencode/` content alone.
//
// Idempotent by construction: a missing file is success, and each directory is
// removed only when it is COMPLETELY empty — so a worktree that already had
// `.opencode/opencode.json` or a user-authored `.opencode/plugins/mine.js`
// keeps both, and a second call after a successful first one is a silent no-op.
// It never returns an error for "already gone", so callers can run it on every
// exit path.
func removeQuestionHook(workDir string) error {
	if workDir == "" {
		return nil
	}
	root, err := os.OpenRoot(workDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open question hook worktree: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := questionHookParents(root, false); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	name := questionHookPath("")
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe question hook file %q", name)
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove question hook: %w", err)
		}
	}
	removeQuestionHookDirIfEmpty(root, filepath.Join(questionHookRootDir, questionHookPluginsDir))
	removeQuestionHookDirIfEmpty(root, questionHookRootDir)
	return nil
}

// questionHookParents rejects symlinks, including links within the worktree.
// Root confines operations even if a parent is replaced after this check.
func questionHookParents(root *os.Root, create bool) error {
	for _, name := range []string{questionHookRootDir, filepath.Join(questionHookRootDir, questionHookPluginsDir)} {
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := root.Mkdir(name, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = root.Lstat(name)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("unsafe question hook parent %q", name)
		}
	}
	return nil
}

// Pruning is best effort; Remove refuses a directory that has any entries.
func removeQuestionHookDirIfEmpty(root *os.Root, name string) {
	info, err := root.Lstat(name)
	if err == nil && info.IsDir() {
		_ = root.Remove(name)
	}
}
