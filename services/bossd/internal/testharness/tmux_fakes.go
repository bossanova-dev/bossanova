package testharness

import (
	"context"
	"os/exec"
	"sync"

	"github.com/recurser/bossd/internal/tmux"
)

// CronReadyTmuxFake is a programmable test double for the tmux CLI used by
// cron e2e tests. It records every invocation and returns canned outcomes for
// each `tmux` subcommand:
//
//   - `tmux -V`               → exit 0 (Available)
//   - `tmux capture-pane ...` → stdout containing the SendPlan ready marker
//     (the ❯ prompt indicator) so SendPlan's marker poll succeeds on first try
//   - `tmux has-session ...`  → exit 0 if the session is in liveSessions,
//     exit 1 otherwise. liveSessions is updated by new-session/kill-session
//     observations, so HasSession reflects the lifecycle of cron-spawned
//     sessions.
//   - All other subcommands (`new-session`, `kill-session`, `load-buffer`,
//     `paste-buffer`, `send-keys`, `set-option`, `bind-key`, ...) → exit 0
//
// This is the cron-happy-path equivalent of the per-test fakeTmux defined in
// e2e_record_chat_tmux_test.go. It lives in the package proper so cron e2e
// tests can share it via Options.TmuxCommandFactory without redefining the
// scaffolding in every file.
type CronReadyTmuxFake struct {
	mu sync.Mutex

	// calls records every (subcommand, args[1:]) invocation other than the
	// `-V` availability probe. Tests use HasCall / Calls to assert on
	// daemon behaviour (e.g. that kill-session fired during finalize).
	calls [][]string

	// liveSessions tracks which named tmux sessions are currently "alive"
	// from this fake's perspective. new-session adds; kill-session removes.
	// has-session consults this map.
	liveSessions map[string]bool
}

// NewCronReadyTmuxFake returns a fresh fake configured for cron-happy-path
// use: tmux is available, capture-pane returns the ready marker immediately,
// and new-session / kill-session toggle the per-name liveSessions map.
func NewCronReadyTmuxFake() *CronReadyTmuxFake {
	return &CronReadyTmuxFake{
		liveSessions: map[string]bool{},
	}
}

// Factory returns the tmux.CommandFactory backed by this fake. Pass it to
// testharness.NewWithOptions via Options.TmuxCommandFactory.
func (f *CronReadyTmuxFake) Factory() tmux.CommandFactory {
	return f.cmd
}

// HasLiveSession reports whether the named tmux session is currently alive
// from this fake's perspective. Mirrors what `tmux has-session -t <name>`
// would return after the same sequence of new-session / kill-session calls.
func (f *CronReadyTmuxFake) HasLiveSession(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveSessions[name]
}

// HasCall reports whether any recorded invocation matches the given
// subcommand. Tests use this for "did the daemon ever call kill-session"
// assertions without caring about exact argument shape.
func (f *CronReadyTmuxFake) HasCall(subcommand string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == subcommand {
			return true
		}
	}
	return false
}

// Calls returns a deep copy of every recorded invocation. The first element
// of each entry is the subcommand; subsequent elements are its arguments
// (the leading "tmux" and the subcommand are stripped by the recorder).
func (f *CronReadyTmuxFake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// cmd is the tmux.CommandFactory implementation. It intentionally does not
// consume ctx for cancellation control of the returned command — the
// returned exec.Cmd is bound to ctx via exec.CommandContext so callers'
// cancellation still propagates.
func (f *CronReadyTmuxFake) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name != "tmux" || len(args) == 0 {
		return exec.CommandContext(ctx, "true")
	}
	subcommand := args[0]

	// Treat `-V` as the availability probe, NOT a recorded subcommand call.
	// This matches the lifecycle_test.go fakeTmux convention so assertions
	// on Calls() don't drown in availability noise.
	if subcommand == "-V" {
		return exec.CommandContext(ctx, "true")
	}

	f.mu.Lock()
	// Record the call (subcommand + remaining args). Done before we fan out
	// to the per-subcommand handlers so the recording is consistent even
	// when the handler returns a non-trivial command.
	rec := append([]string{subcommand}, args[1:]...)
	f.calls = append(f.calls, rec)

	switch subcommand {
	case "new-session":
		// Extract -s <name> so we can mark the session live. tmux's
		// new-session takes -s NAME; scan args for it.
		if sessName := scanFlagValue(args[1:], "-s"); sessName != "" {
			f.liveSessions[sessName] = true
		}
		f.mu.Unlock()
		return exec.CommandContext(ctx, "true")

	case "kill-session":
		if sessName := scanFlagValue(args[1:], "-t"); sessName != "" {
			delete(f.liveSessions, sessName)
		}
		f.mu.Unlock()
		return exec.CommandContext(ctx, "true")

	case "has-session":
		sessName := scanFlagValue(args[1:], "-t")
		alive := f.liveSessions[sessName]
		f.mu.Unlock()
		if alive {
			return exec.CommandContext(ctx, "true")
		}
		return exec.CommandContext(ctx, "false")

	case "capture-pane":
		// Return canned stdout containing the SendPlan ready marker so
		// the daemon's first poll succeeds without sleeping. printf is
		// portable across macOS and Linux CI runners.
		f.mu.Unlock()
		return exec.CommandContext(ctx, "printf", "%s", "Welcome to Claude\n❯\n")
	}

	f.mu.Unlock()
	return exec.CommandContext(ctx, "true")
}

// scanFlagValue returns the value following flag in args, or "" if flag is
// absent or the value is missing. tmux invocations always pass flag and
// value as separate arguments (e.g. "-s", "boss-abc"), so a simple linear
// scan is sufficient and matches the production tmux client's usage.
func scanFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// CronReadyTmuxFactory is a convenience constructor for cron e2e tests that
// don't need to inspect tmux call history. It builds a fresh
// CronReadyTmuxFake and returns just the factory. Use NewCronReadyTmuxFake
// directly when assertions on the recorded calls or live-session state are
// needed.
func CronReadyTmuxFactory() tmux.CommandFactory {
	return NewCronReadyTmuxFake().Factory()
}
