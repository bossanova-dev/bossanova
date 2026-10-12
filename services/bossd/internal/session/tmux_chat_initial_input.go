package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// initialInputFileSuffix names the per-launch file a runner writes its rendered
// first input to. It lives beside the agent log (outside every worktree).
const initialInputFileSuffix = ".initial-input"

// staleInitialInputFileAge is how old a leftover input file must be before the
// startup sweep removes it. A live launch reads (and removes) its file within
// its start window, so a day-old file can only be a crashed bossd's leftover.
const staleInitialInputFileAge = 24 * time.Hour

// launchInput is the first input a launch offers its runner, and the private
// file (path, empty when none could be allocated) the runner may have the
// login shell read it from at exec time instead of carrying it through tmux's
// size-limited command.
type launchInput struct {
	prompt, command, path string
}

// offered reports whether the launch handed the runner a first input at all.
func (in launchInput) offered() bool { return in.prompt != "" || in.command != "" }

// prepareLaunchInput decides what first input a launch offers its runner. Only
// a submitted input is offered: a runner that consumes it submits it from
// argv, which a prefill (left in the composer for someone else to submit) must
// never do, so a prefill keeps the pane path. A submitted input also gets a
// fresh private file path beside the agent log. The caller removes that file
// when the start fails.
func (l *Lifecycle) prepareLaunchInput(input ChatInput, agentSessionID, sessionID string) launchInput {
	if input.Delivery != DeliverySubmit {
		return launchInput{}
	}
	in := launchInput{prompt: input.Prompt, command: input.Command}
	if !in.offered() {
		return in
	}
	path, err := newInitialInputPath(l.agentLogsDir, agentSessionID)
	if err != nil {
		// Not fatal: the runner inlines the input, and the tmux size guard
		// still refuses a launch too large to start.
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Msg("no initial input file; runner will inline the first input")
		return in
	}
	in.path = path
	return in
}

// settleLaunchInputFile reconciles the launch's input file with the runner's
// argv. When argv reads the file, the pane's shell reads (and then removes) it
// at exec time, so it must be in place and private before the launch; when it
// does not, the runner inlined the input or left it for the pane, and the
// unused file is removed.
func (l *Lifecycle) settleLaunchInputFile(in launchInput, argv []string, sessionID string) error {
	if in.path == "" {
		return nil
	}
	if slices.Contains(argv, in.path) {
		return verifyInitialInputFile(in.path)
	}
	l.removeInitialInputFile(in.path, sessionID)
	return nil
}

// newInitialInputPath returns a fresh path in agentLogsDir for one launch's
// first input. A random component keeps a relaunch of the same chat from ever
// reading a predecessor's file. It only names the file; the runner writes it.
func newInitialInputPath(agentLogsDir, agentSessionID string) (string, error) {
	if agentLogsDir == "" {
		return "", errors.New("no agent logs directory configured")
	}
	if err := os.MkdirAll(agentLogsDir, 0o700); err != nil {
		return "", fmt.Errorf("create agent logs directory: %w", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate initial input file name: %w", err)
	}
	return filepath.Join(agentLogsDir, agentSessionID+"."+hex.EncodeToString(nonce[:])+initialInputFileSuffix), nil
}

// verifyInitialInputFile checks the file a runner's argv will read at exec
// time: it must exist, be a regular file (not a symlink), and be readable by
// its owner only, since it carries the user's prompt.
func verifyInitialInputFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("initial input file the runner's argv reads is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("initial input file %s is not a regular file (mode %s)", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("initial input file %s has mode %o; want owner-only (0600)", path, perm)
	}
	return nil
}

// removeInitialInputFile deletes a launch's input file. On a successful launch
// the login shell removes it once read; bossd removes it whenever the start
// fails or the runner did not use it. A file that is already gone is fine.
func (l *Lifecycle) removeInitialInputFile(path, sessionID string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("path", path).
			Msg("could not remove initial input file")
	}
}

// SweepStaleInitialInputFiles removes input files older than a day from the
// agent logs directory: a bossd that crashed mid-start can leave one behind
// (on a successful launch the login shell removes it, on a failed start bossd
// does). It returns how many it removed. Only regular files carrying the
// input-file suffix are considered; anything else in the directory is left
// alone. Call it once at startup, after SetAgentLogsDir.
func (l *Lifecycle) SweepStaleInitialInputFiles(now time.Time) int {
	if l.agentLogsDir == "" {
		return 0
	}
	entries, err := os.ReadDir(l.agentLogsDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			l.logger.Warn().Err(err).Str("dir", l.agentLogsDir).Msg("could not list agent logs dir for stale input files")
		}
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), initialInputFileSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < staleInitialInputFileAge {
			continue
		}
		path := filepath.Join(l.agentLogsDir, entry.Name())
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			l.logger.Warn().Err(err).Str("path", path).Msg("could not remove stale initial input file")
			continue
		}
		removed++
	}
	return removed
}
