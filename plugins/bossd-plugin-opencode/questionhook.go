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
	path := questionHookPath(workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, fmt.Errorf("create %s dir: %w", questionHookPluginsDir, err)
	}
	if err := os.WriteFile(path, questionHookJS, 0o600); err != nil {
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
	path := questionHookPath(workDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove question hook: %w", err)
	}
	pluginsDir := filepath.Dir(path)
	removeDirIfEmpty(pluginsDir)
	removeDirIfEmpty(filepath.Dir(pluginsDir))
	return nil
}

// removeDirIfEmpty removes dir only when it holds no entries at all. Any error
// (unreadable, non-empty, already gone) is deliberately swallowed: pruning is a
// tidiness measure, never a correctness one.
func removeDirIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return
	}
	_ = os.Remove(dir)
}
