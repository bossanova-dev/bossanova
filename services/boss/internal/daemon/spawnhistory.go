package daemon

import (
	"fmt"
	"strconv"
	"strings"
)

// SpawnState classifies what the platform service manager's spawn history says
// about the installed bossd job.
//
// BOS-1183: `launchctl list com.bossanova.bossd` exits 0 for a job launchd has
// REGISTERED but never spawned, so Status.Running reports true for a daemon
// that will never start. That is not a hypothetical: on 2026-09-06 a machine
// whose Aqua session had been backgrounded by fast user switching had the job
// loaded in a domain launchd would not run anything in, and every
// configuration-reading check reported healthy. The decisive extra evidence is
// `launchctl print gui/<uid>/<label>`, which reports how many times launchd has
// actually spawned the job and how it last exited.
type SpawnState string

const (
	// SpawnStateUnknown means the spawn history could not be determined. It is
	// the fail-closed verdict: `launchctl print` is a human-readable dump with
	// no format contract across macOS releases, so anything we cannot read maps
	// here and NEVER to SpawnStateHealthy. Every unknown carries a Reason.
	SpawnStateUnknown SpawnState = "unknown"
	// SpawnStateUnsupported means the platform has no launchd-style spawn
	// history to read (systemd exposes unit substates directly instead).
	SpawnStateUnsupported SpawnState = "unsupported"
	// SpawnStateNeverSpawned means the job is registered but launchd has never
	// attempted to spawn it. This is always a DOMAIN problem — a session
	// launchd will not run jobs in — and never a bossd crash.
	SpawnStateNeverSpawned SpawnState = "never-spawned"
	// SpawnStateFailing means launchd did spawn the job and it exited non-zero:
	// bossd itself started and failed.
	SpawnStateFailing SpawnState = "failing"
	// SpawnStateHealthy means launchd has spawned the job and its last exit is
	// not a failure.
	SpawnStateHealthy SpawnState = "healthy"
)

// SpawnHistory is one reading of the service manager's spawn history for the
// installed bossd job. The raw fields are kept alongside State so a caller can
// report the evidence, not just the verdict.
type SpawnHistory struct {
	// State is the classification. Callers must treat anything other than
	// SpawnStateHealthy as "not proven working".
	State SpawnState
	// Target is the service-manager target that was probed, e.g.
	// "gui/501/com.bossanova.bossd". Always set on platforms that have one,
	// including on failures, so a report can name what it asked about.
	Target string
	// Runs is the number of times the service manager has spawned the job.
	// Meaningful only when RunsKnown is true.
	Runs int
	// RunsKnown reports whether Runs was read from the output.
	RunsKnown bool
	// LastExitCode is the job's last exit status. Meaningful only when
	// LastExitCodeKnown is true.
	LastExitCode int
	// LastExitCodeKnown reports whether a NUMERIC last exit code was read. It
	// is false when the job has never exited (see NeverExited).
	LastExitCodeKnown bool
	// NeverExited reports that launchd printed "(never exited)" as the last
	// exit code. On its own this says nothing: paired with Runs == 0 it is the
	// never-spawned incident, paired with Runs > 0 it is an ordinary running
	// daemon.
	NeverExited bool
	// ServiceState is the raw `state = ...` value when the output carried one,
	// e.g. "running" or "not running". Informational only — it is the same
	// registration-level fact Status.Running already reports, which is why it
	// takes no part in the classification.
	ServiceState string
	// Reason explains an unknown or unsupported verdict in human-readable
	// terms. Empty for determinate verdicts.
	Reason string
}

// GetSpawnHistory reports the spawn history of the installed bossd job.
//
// Error discipline: a non-nil error means the probe could not be ATTEMPTED
// (the service manager binary could not be executed at all). "We asked and
// could not tell" is not an error — it is a populated SpawnHistory with State
// SpawnStateUnknown and a Reason. The returned SpawnHistory is always
// fail-closed and safe to report even when the error is non-nil: it is never
// SpawnStateHealthy on any failure path.
func GetSpawnHistory() (SpawnHistory, error) {
	return platformSpawnHistory()
}

// launchdField is one `key = value` line captured from a `launchctl print`
// dump, together with the brace depth it was found at.
type launchdField struct {
	value string
	depth int
	found bool
}

// record keeps the SHALLOWEST occurrence of a key. `launchctl print` nests
// sub-dictionaries (arguments, environment, endpoints, event triggers) inside
// the job's own block, and a key that happens to repeat inside one of those
// describes the sub-dictionary, not the job. Taking the shallowest occurrence
// binds us to the job's own level without hard-coding a depth, which matters
// because the surrounding block structure is exactly the part of this format
// most likely to change between macOS releases.
func (f *launchdField) record(value string, depth int) {
	if f.found && depth >= f.depth {
		return
	}
	f.value = value
	f.depth = depth
	f.found = true
}

// splitLaunchdKeyValue splits a trimmed `launchctl print` line into its key and
// value. It reports false for lines that are not key/value pairs at all, and
// for the `key => value` form launchd uses inside environment dictionaries —
// splitting those on the bare `=` would yield a value beginning with '>'.
func splitLaunchdKeyValue(line string) (key, value string, ok bool) {
	idx := strings.Index(line, "=")
	if idx < 0 {
		return "", "", false
	}
	if idx+1 < len(line) && line[idx+1] == '>' {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

// launchdNeverExited is the literal launchd prints for a job that has not yet
// exited. Any OTHER non-numeric value is treated as unreadable rather than
// guessed at.
const launchdNeverExited = "(never exited)"

// parseLaunchdSpawnHistory classifies the output of
// `launchctl print gui/<uid>/<label>`.
//
// The whole point of this function is the asymmetry between two lines that
// share the same "(never exited)" text:
//
//	runs = 0, last exit code = (never exited)  -> launchd NEVER TRIED to spawn
//	runs = 1, last exit code = (never exited)  -> ordinary running daemon
//
// The second is the normal shape of a healthy job that is up right now, so it
// must classify healthy; the first is the BOS-1183 incident. Do not "fix" that
// asymmetry — collapsing the two is the bug this probe exists to catch.
//
// Everything it cannot read classifies unknown with a Reason. There is no
// format contract for this output, so a future macOS that renames a key must
// make this probe say "I don't know" rather than quietly report a clean
// daemon.
func parseLaunchdSpawnHistory(out []byte) SpawnHistory {
	var runsField, exitField, stateField launchdField

	depth := 0
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if key, value, ok := splitLaunchdKeyValue(line); ok {
			// The key is evaluated at the depth it is written at, BEFORE this
			// line's own braces are applied, so `arguments = {` counts as a key
			// of the enclosing block rather than of the block it opens.
			switch key {
			case "runs":
				runsField.record(value, depth)
			case "last exit code":
				exitField.record(value, depth)
			case "state":
				stateField.record(value, depth)
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth < 0 {
			depth = 0
		}
	}

	h := SpawnHistory{ServiceState: stateField.value}

	// A negative spawn count is not something launchd emits; treating it as
	// unreadable keeps the classifier's arithmetic honest instead of inventing
	// a meaning for it.
	if runsField.found {
		if n, err := strconv.Atoi(runsField.value); err == nil && n >= 0 {
			h.Runs = n
			h.RunsKnown = true
		}
	}

	exitReadable := false
	if exitField.found {
		if exitField.value == launchdNeverExited {
			h.NeverExited = true
			exitReadable = true
		} else if code, err := strconv.Atoi(exitField.value); err == nil {
			h.LastExitCode = code
			h.LastExitCodeKnown = true
			exitReadable = true
		}
	}

	h.State, h.Reason = classifyLaunchdSpawnHistory(h, runsField, exitField, exitReadable)
	return h
}

// classifyLaunchdSpawnHistory maps a parsed reading onto a SpawnState. It is
// split out so the fail-closed ladder reads top to bottom: every branch that is
// not a positive proof of a spawn ends in unknown.
func classifyLaunchdSpawnHistory(h SpawnHistory, runsField, exitField launchdField, exitReadable bool) (SpawnState, string) {
	switch {
	case !runsField.found && !exitField.found:
		// Empty output, or output that is not a launchctl print dump at all.
		return SpawnStateUnknown, "launchctl print output carried neither a `runs` nor a `last exit code` line (empty output, or a format this build does not recognise)"
	case !runsField.found:
		return SpawnStateUnknown, "launchctl print output carried a `last exit code` line but no `runs` line, so the spawn count is unknown"
	case !exitField.found:
		return SpawnStateUnknown, "launchctl print output carried a `runs` line but no `last exit code` line, so the exit status is unknown"
	case !h.RunsKnown:
		return SpawnStateUnknown, fmt.Sprintf("launchctl print reported an unreadable `runs` value %q", runsField.value)
	case !exitReadable:
		return SpawnStateUnknown, fmt.Sprintf("launchctl print reported an unreadable `last exit code` value %q", exitField.value)
	}

	if h.Runs == 0 {
		if h.NeverExited {
			return SpawnStateNeverSpawned, ""
		}
		// Contradictory: launchd cannot report an exit status for a job it
		// never spawned. Reading this either way would be a guess, and the
		// fail-open guess is precisely the failure mode this probe exists to
		// remove.
		return SpawnStateUnknown, fmt.Sprintf("launchctl print reported runs = 0 with last exit code = %d, which contradict each other", h.LastExitCode)
	}

	if h.LastExitCodeKnown && h.LastExitCode != 0 {
		return SpawnStateFailing, ""
	}
	// runs > 0 with either a clean exit or "(never exited)" — see the doc
	// comment above for why "(never exited)" is healthy HERE and an incident at
	// runs = 0.
	return SpawnStateHealthy, ""
}
