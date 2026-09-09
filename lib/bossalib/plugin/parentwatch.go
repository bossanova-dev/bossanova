package plugin

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ParentPIDEnvVar names the environment variable the bossd host stamps with
// its own PID when it launches a plugin subprocess. It deliberately sits in
// the BOSSANOVA_PLUGIN_* host/plugin protocol namespace (alongside
// MagicCookieKey) rather than the BOSS_PLUGIN_* namespace, because the latter
// is a verbatim projection of operator-supplied per-plugin config keys — a
// config key spelled "PARENT_PID" would otherwise collide with this.
const ParentPIDEnvVar = "BOSSANOVA_PLUGIN_PARENT_PID"

// DefaultParentPollInterval is how often an armed watchdog samples its parent
// PID. The trade is orphan lifetime against idle wakeups: the leak this guards
// (BOS-1221) left plugins alive for nearly two days, while a couple of seconds
// of detection latency costs nothing. One wakeup every 2s is not measurable
// against an otherwise idle plugin.
const DefaultParentPollInterval = 2 * time.Second

// ParentWatchArmedMsg is the log message an armed watchdog emits, exported so
// a test observing a real plugin subprocess can assert that arming actually
// happened rather than assuming it from the inputs.
//
// Arming is otherwise invisible from outside the process: every fail-open
// branch either stays silent or warns, and a plugin whose stamp was shadowed
// behaves identically to an armed one until its parent dies. Tests that only
// name arming in their premise -- the graceful-kill and parent-alive controls
// -- pass unchanged against a build where the watchdog never armed, so the
// positive signal has to exist for them to be worth anything.
const ParentWatchArmedMsg = "parent-death watchdog armed"

// StartParentWatch arms the parent-death watchdog for a plugin subprocess and
// returns a function that stops it. Production callers (every plugin main)
// discard the return value: the watcher is meant to outlive every caller and
// die with the process.
//
// Why this exists. A go-plugin subprocess has no way to learn that its parent
// died. github.com/hashicorp/go-plugin@v1.8.0 sets cmd.Stdin = os.Stdin
// (client.go:659), so the child inherits *the host's* stdin rather than a pipe
// from the host — under launchd that is /dev/null, which never EOFs — and
// Serve waits only on its own gRPC done channel while deliberately ignoring
// os.Interrupt ("Eat the interrupts", server.go:460). On the crash or SIGKILL
// path the host never runs its own teardown, so nothing reaps the plugin and
// it idles indefinitely, reparented to PID 1. If a future go-plugin version
// hands the child a real pipe, this watchdog becomes redundant rather than
// wrong.
//
// Why it polls. PR_SET_PDEATHSIG is Linux-only and this is a macOS-first
// defect, and go-plugin's ClientConfig exposes no way to hand the child an
// extra descriptor on this path. os.Getppid is portable and needs no
// cooperation from go-plugin: when the parent dies the child is reparented.
//
// Why it fails OPEN. This watchdog is destructive — a false positive kills a
// live plugin under a healthy daemon and takes its sessions with it — so a
// signal it cannot positively interpret is UNKNOWN, never "parent dead". It
// refuses to arm when the stamp is absent, unparseable, or does not describe
// this process (see startParentWatch).
func StartParentWatch(logger zerolog.Logger) func() {
	return startParentWatch(logger, os.LookupEnv, os.Getppid, DefaultParentPollInterval, os.Exit)
}

// startParentWatch is the injectable core of StartParentWatch. Modelled on
// handleSigterm in plugins/bossd-plugin-repair/main.go: the parent-PID source,
// the poll interval and the exit action are all parameters, so every branch is
// assertable without spawning a real orphan.
//
// It arms only when the stamped identity positively describes this process,
// and returns a no-op stop function in every UNKNOWN case:
//
//   - variable absent or empty — the compatibility case: a plugin launched by
//     anything other than a stamping bossd keeps running;
//   - variable unparseable — same, plus one log line, because it also hides a
//     host-side bug;
//   - variable naming a PID that is not this process's parent at startup — the
//     catastrophic one. launchPlugin builds cmd.Env from os.Environ() plus the
//     stamp and go-plugin appends os.Environ() again (SkipHostEnv is false),
//     so a same-named variable already in the host's environment would land
//     last and win under os/exec's last-wins dedup. Without this guard a
//     shadowed or stale stamp would exit every plugin on its first poll.
//
// The exit action is called at most once: the watcher returns immediately
// after firing, so a parent that stays gone does not produce one exit per
// poll. Exit code 0, for the same reason handleSigterm uses it — nothing keys
// off the code.
func startParentWatch(
	logger zerolog.Logger,
	lookupEnv func(string) (string, bool),
	parentPID func() int,
	interval time.Duration,
	exit func(int),
) func() {
	noop := func() {}

	raw, ok := lookupEnv(ParentPIDEnvVar)
	if !ok || strings.TrimSpace(raw) == "" {
		// UNKNOWN: no identity at all. Fail open and stay silent — running
		// without a stamping host is a supported way to run a plugin.
		return noop
	}

	expected, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || expected <= 0 {
		logger.Warn().
			Str("var", ParentPIDEnvVar).
			Str("value", raw).
			Msg("unparseable parent identity; parent-death watchdog not armed")
		return noop
	}

	if live := parentPID(); live != expected {
		logger.Warn().
			Str("var", ParentPIDEnvVar).
			Int("stamped", expected).
			Int("actual", live).
			Msg("stamped parent identity is not this process's parent; parent-death watchdog not armed")
		return noop
	}

	if interval <= 0 {
		interval = DefaultParentPollInterval
	}

	logger.Info().
		Int("parent", expected).
		Dur("poll_interval", interval).
		Msg(ParentWatchArmedMsg)

	done := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				observed := parentPID()
				if observed == expected {
					continue
				}
				logger.Warn().
					Int("expected_parent", expected).
					Int("observed_parent", observed).
					Msg("parent process is gone; exiting so this plugin does not outlive it")
				exit(0)
				return
			}
		}
	}()

	// The returned stop waits for the watcher to actually return, so a caller
	// (and goleak) sees a fully drained goroutine rather than one still
	// unwinding.
	return func() {
		once.Do(func() { close(done) })
		<-stopped
	}
}
