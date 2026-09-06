package testharness

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

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

	// sessionCreated records each live session's tmux `session_created`
	// instant, reported by the list-sessions arm. new-session stamps the
	// current time; AgeSession rewinds it so a test can present a pane as
	// older than a grace window without sleeping.
	sessionCreated map[string]time.Time

	// sessionEnv records the `-e KEY=VALUE` pairs each session was created
	// with, so show-environment can read them back the way real tmux does.
	// This is what lets a test exercise the reaper's ownership stamp.
	sessionEnv map[string]map[string]string

	// ListSessionsStderr, when non-empty, makes every `tmux list-sessions`
	// exit non-zero with this string on stderr. Set it to a "no server
	// running ..." message for the idle-host case and to anything else for
	// the unreadable-tmux case the reaper must fail closed on.
	ListSessionsStderr string

	// CapturePaneOutput, when non-empty, replaces the canned capture-pane
	// stdout. Tests that need the daemon to react to specific pane CONTENT —
	// a login banner, a usage-limit banner, a transient 5xx banner — script it
	// here instead of standing up a second fake. Whatever is supplied must
	// still carry the ready marker (❯) if the test exercises a Send* path, as
	// those poll capture-pane for it. Empty (the zero value) keeps the original
	// cron-happy-path pane, so existing callers are unaffected.
	CapturePaneOutput string

	// FailNewSession, when non-empty, makes every `tmux new-session` exit
	// non-zero writing this string to stderr — reproducing a real tmux launch
	// failure (e.g. "missing or unsuitable terminal: xterm-ghostty") so tests
	// can exercise the daemon's StartError-stamping path. The session is NOT
	// marked live when this fires, matching a failed spawn.
	FailNewSession string
}

// NewCronReadyTmuxFake returns a fresh fake configured for cron-happy-path
// use: tmux is available, capture-pane returns the ready marker immediately,
// and new-session / kill-session toggle the per-name liveSessions map.
func NewCronReadyTmuxFake() *CronReadyTmuxFake {
	return &CronReadyTmuxFake{
		liveSessions:   map[string]bool{},
		sessionCreated: map[string]time.Time{},
		sessionEnv:     map[string]map[string]string{},
	}
}

// AgeSession rewinds a live session's reported `session_created` by d, so a
// test can present a pane as older than the reaper's grace window without
// sleeping. No-op for a session this fake never saw created.
func (f *CronReadyTmuxFake) AgeSession(name string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if created, ok := f.sessionCreated[name]; ok {
		f.sessionCreated[name] = created.Add(-d)
	}
}

// SessionEnv returns the `-e KEY=VALUE` pairs the named session was created
// with, as show-environment would read them back.
func (f *CronReadyTmuxFake) SessionEnv(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.sessionEnv[name]))
	for k, v := range f.sessionEnv[name] {
		out[k] = v
	}
	return out
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
		// Simulate a tmux launch failure: exit non-zero with FailNewSession on
		// stderr and leave the session absent from liveSessions. Passing the
		// message as $1 to `sh -c` avoids any shell quoting of its contents.
		if f.FailNewSession != "" {
			f.mu.Unlock()
			// #nosec G204 -- test-only tmux fake; message passed as $1 to `sh -c`, no shell interpolation of its contents
			// owner=@recurser review-by=2027-01-18 issue=BOS-28
			return exec.CommandContext(ctx, "sh", "-c",
				`printf '%s' "$1" >&2; exit 1`, "sh", f.FailNewSession)
		}
		// Extract -s <name> so we can mark the session live. tmux's
		// new-session takes -s NAME; scan args for it.
		if sessName := scanFlagValue(args[1:], "-s"); sessName != "" {
			f.liveSessions[sessName] = true
			f.sessionCreated[sessName] = time.Now()
			f.sessionEnv[sessName] = scanSessionEnv(args[1:])
		}
		f.mu.Unlock()
		return exec.CommandContext(ctx, "true")

	case "kill-session":
		if sessName := scanFlagValue(args[1:], "-t"); sessName != "" {
			delete(f.liveSessions, sessName)
			delete(f.sessionCreated, sessName)
			delete(f.sessionEnv, sessName)
		}
		f.mu.Unlock()
		return exec.CommandContext(ctx, "true")

	case "list-sessions":
		// Without this arm the default `true` at the bottom of this function
		// answers with empty stdout, which parses as "nothing is running" —
		// an orphan-reaper test would then pass while exercising nothing.
		// Emit the production format instead:
		// "#{session_name}\t#{session_created}\t#{session_attached}", creation
		// as Unix seconds and no attached clients.
		if f.ListSessionsStderr != "" {
			stderr := f.ListSessionsStderr
			f.mu.Unlock()
			// #nosec G204 -- test-only tmux fake; message passed as $1 to `sh -c`, no shell interpolation of its contents
			// owner=@recurser review-by=2027-01-18 issue=BOS-28
			return exec.CommandContext(ctx, "sh", "-c",
				`printf '%s\n' "$1" >&2; exit 1`, "sh", stderr)
		}
		var b strings.Builder
		for _, sessName := range slices.Sorted(maps.Keys(f.liveSessions)) {
			if !f.liveSessions[sessName] {
				continue
			}
			fmt.Fprintf(&b, "%s\t%d\t0\n", sessName, f.sessionCreated[sessName].Unix())
		}
		f.mu.Unlock()
		cmd := exec.CommandContext(ctx, "cat")
		cmd.Stdin = strings.NewReader(b.String())
		return cmd

	case "show-environment":
		// args = ["show-environment", "-t", name, key]. Real tmux prints
		// "KEY=value" for a set variable and exits non-zero otherwise, which
		// is how an unstamped pane is expressed.
		sessName := scanFlagValue(args[1:], "-t")
		var line string
		if len(args) >= 4 {
			if v, ok := f.sessionEnv[sessName][args[3]]; ok {
				line = args[3] + "=" + v + "\n"
			}
		}
		f.mu.Unlock()
		if line == "" {
			return exec.CommandContext(ctx, "false")
		}
		cmd := exec.CommandContext(ctx, "cat")
		cmd.Stdin = strings.NewReader(line)
		return cmd

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
		// the daemon's first poll succeeds without sleeping. `cat` is
		// portable across macOS and Linux CI runners and echoes stdin
		// verbatim, so a scripted CapturePaneOutput containing "%" is
		// emitted unchanged. The pane text travels via STDIN rather than
		// argv deliberately: a constant argv keeps this off gosec's G204
		// (subprocess launched with variable) radar without needing a
		// suppression, and mirrors the load-buffer fake in tmux_test.go.
		pane := f.CapturePaneOutput
		if pane == "" {
			pane = "Welcome to Claude\n❯\n"
		}
		f.mu.Unlock()
		cmd := exec.CommandContext(ctx, "cat")
		cmd.Stdin = strings.NewReader(pane)
		return cmd
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

// scanSessionEnv collects the `-e KEY=VALUE` pairs from a new-session argv,
// mirroring how tmux seeds a session environment. A flag with no `=` is
// skipped rather than stored under an empty key.
func scanSessionEnv(args []string) map[string]string {
	env := map[string]string{}
	for i, a := range args {
		if a != "-e" || i+1 >= len(args) {
			continue
		}
		if k, v, ok := strings.Cut(args[i+1], "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

// CronReadyTmuxFactory is a convenience constructor for cron e2e tests that
// don't need to inspect tmux call history. It builds a fresh
// CronReadyTmuxFake and returns just the factory. Use NewCronReadyTmuxFake
// directly when assertions on the recorded calls or live-session state are
// needed.
func CronReadyTmuxFactory() tmux.CommandFactory {
	return NewCronReadyTmuxFake().Factory()
}
