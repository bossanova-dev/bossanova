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
// REGISTERED but never spawned. That is not a hypothetical: on 2026-09-06 a
// machine whose Aqua session had been backgrounded by fast user switching had
// the job loaded in a domain launchd would not run anything in, and every
// configuration-reading check reported healthy. BOS-1218 has since narrowed
// Status.Running to require a PID from that answer, so such a job reports
// Running = false rather than true — but "no PID" is still not a diagnosis.
// The decisive extra evidence is `launchctl print gui/<uid>/<label>`, which
// reports how many times launchd has actually spawned the job and how it last
// exited.
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
	// e.g. "running" or "not running". Informational only — it is launchd's
	// own word for the liveness Status.Running already reports (BOS-1218), and
	// a free-form one with no format contract across macOS releases, which is
	// why it takes no part in the classification.
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

// spawnHistoryTarget resolves which service-manager target carries the spawn
// history for bossd on this host.
//
// BOS-1204: the answer is not a constant, because it is not the same JOB under
// the two supervision substrates. Under the default the per-user LaunchAgent IS
// bossd, so gui/<uid>/<label> is the job launchd spawns. Under `unattended`
// launchd spawns the root-owned WATCHDOG, which then spawns bossd through
// `launchctl asuser` — so bossd has no launchd job of its own and gui/<uid>
// carries no history at all.
//
// It takes the LaunchAgent target as an argument rather than building it,
// because Label and os.Getuid() belong to the darwin build while this rule does
// not, and keeping the rule here is what makes the whole matrix — including the
// rejected-configuration row — provable from either platform's test run.
//
// A rejected configuration resolves to the LaunchAgent target. That is the same
// fail-closed direction ResolveSupervisionMode itself takes: a typo must never
// be read as a request to probe a root-owned job.
func spawnHistoryTarget(supervision SupervisionModeStatus, launchAgentTarget string) string {
	if supervision.Err == nil && supervision.Mode == SupervisionModeUnattended {
		return WatchdogTarget()
	}
	return launchAgentTarget
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

// JobDisabledState classifies whether the platform service manager holds a
// DISABLE override for the installed bossd job.
//
// BOS-1222: a never-spawned job (runs = 0) has several possible causes, and
// doctor used to assert one it had never measured. A disable override is one
// of the causes it CAN settle cheaply, so it is read rather than guessed.
//
// Fail-closed exactly like SpawnState above: `launchctl print-disabled` is a
// human-readable dump with no format contract across macOS releases, so
// anything unreadable maps to JobDisabledStateUnknown with a Reason and NEVER
// to JobDisabledStateEnabled. Reporting "enabled" is what would let a real
// disable override be reported as ruled out.
type JobDisabledState string

const (
	// JobDisabledStateUnknown means the disable state could not be
	// determined. It is the fail-closed verdict and always carries a Reason.
	// It asserts nothing: a caller must report "not checked", never a side.
	JobDisabledStateUnknown JobDisabledState = "unknown"
	// JobDisabledStateUnsupported means the platform has no launchd-style
	// per-domain disable override to read.
	JobDisabledStateUnsupported JobDisabledState = "unsupported"
	// JobDisabledStateEnabled means the service manager holds no disable
	// override for the label, so being disabled is RULED OUT as a cause.
	JobDisabledStateEnabled JobDisabledState = "enabled"
	// JobDisabledStateDisabled means the service manager holds a disable
	// override for the label, which ESTABLISHES it as the cause: launchd will
	// not spawn a job it has been told is disabled, no matter what else is
	// true of the domain.
	JobDisabledStateDisabled JobDisabledState = "disabled"
)

// JobDisabled is one reading of the disable override for the installed bossd
// job. Domain and Label are kept alongside State so a report can name what was
// asked about — including on the failure paths, where naming the question is
// the whole remaining value.
type JobDisabled struct {
	// State is the classification. Anything other than
	// JobDisabledStateEnabled must be treated as "not proven enabled".
	State JobDisabledState
	// Domain is the service-manager domain that was probed, e.g. "gui/501".
	// Always set on platforms that have one, including on failures.
	Domain string
	// Label is the job label the domain's dump was searched for.
	Label string
	// Reason explains an unknown or unsupported verdict in human-readable
	// terms. Empty for determinate verdicts.
	Reason string
}

// GetJobDisabled reports whether the installed bossd job is disabled in its
// service-manager domain.
//
// Error discipline (identical to GetSpawnHistory): a non-nil error means the
// probe could not be ATTEMPTED — the service manager binary could not be
// executed at all. "We asked and could not tell" is NOT an error; it is a
// populated JobDisabled with State JobDisabledStateUnknown and a Reason. The
// returned JobDisabled is always fail-closed and safe to report even when the
// error is non-nil: it is never JobDisabledStateEnabled on any failure path.
func GetJobDisabled() (JobDisabled, error) {
	return platformJobDisabled()
}

// jobDisabledDomain resolves which service-manager domain and label carry the
// disable override for the job launchd actually spawns.
//
// It applies the same substrate rule spawnHistoryTarget directly above does,
// for the same BOS-1204 reason: under `unattended` launchd spawns the
// root-owned WATCHDOG in the watchdog domain and gui/<uid> carries nothing
// about it, so asking the wrong domain would answer a question nobody asked.
// The two agree by CONSTRUCTION and not by inspection — spawnHistoryTarget
// returns WatchdogTarget(), which is WatchdogDomain plus WatchdogLabel, and
// this returns the same two halves unjoined — so a future domain move carries
// both rather than leaving this one confidently probing a domain that no
// longer holds the job.
//
// It takes the LaunchAgent domain AND label as arguments rather than building
// them, for the reason spawnHistoryTarget gives: Label and os.Getuid() belong
// to the darwin build while this rule does not, and keeping the rule here is
// what makes the whole matrix — including the rejected-configuration row —
// provable from either platform's test run. The label is passed for the same
// reason the domain is; `Label` does not exist outside the darwin build, so
// naming it here would put the rule back inside the platform it must not
// depend on. WatchdogLabel needs no such treatment: watchdog.go is already
// platform-independent.
//
// A rejected configuration resolves to the LaunchAgent domain: the same
// fail-closed direction spawnHistoryTarget and ResolveSupervisionMode take, so
// a typo can never be read as a request to probe a root-owned domain.
func jobDisabledDomain(supervision SupervisionModeStatus, launchAgentDomain, launchAgentLabel string) (domain, label string) {
	if supervision.Err == nil && supervision.Mode == SupervisionModeUnattended {
		return WatchdogDomain, WatchdogLabel
	}
	return launchAgentDomain, launchAgentLabel
}

// launchdDisabledMarker opens the line `launchctl print-disabled` starts its
// dump with. It is a PREFIX and not the whole line, so it is never evidence of
// well-formedness on its own; see parseLaunchdDisabledServices, which requires
// the block's opening brace and its matching close.
const launchdDisabledMarker = "disabled services"

// launchdDisabledBlockOpen is the token that closes the dump's opening line,
// and launchdDisabledBlockClose the line that closes the block.
//
// BOS-1222: matching the marker as a bare prefix accepted a near-miss or error
// format such as `disabled services unavailable` as a complete dump, and the
// absence inference below then RULED THE DISABLED CAUSE OUT from output that
// carried no overrides at all. That is the fail-open guess this ticket exists
// to remove, reproduced inside the parser that was written to prevent it.
const (
	launchdDisabledBlockOpen  = "{"
	launchdDisabledBlockClose = "}"
)

// splitLaunchdDisabledEntry splits one `"label" => value` entry out of a
// print-disabled dump.
//
// A dedicated matcher rather than splitLaunchdKeyValue, which deliberately
// REJECTS the `=>` form (it exists to parse the `key = value` lines of
// `launchctl print`, where a `=>` line means an environment sub-dictionary).
// Reusing it here would reject every entry and make the parser see an empty
// dump.
func splitLaunchdDisabledEntry(line string) (label, value string, ok bool) {
	idx := strings.Index(line, "=>")
	if idx < 0 {
		return "", "", false
	}
	label = strings.Trim(strings.TrimSpace(line[:idx]), `"`)
	value = strings.TrimSpace(line[idx+len("=>"):])
	if label == "" {
		return "", "", false
	}
	return label, value, true
}

// parseLaunchdDisabledServices classifies the output of
// `launchctl print-disabled <domain>`, which looks like:
//
//	disabled services = {
//		"com.example.foo" => true
//		"com.example.bar" => false
//	}
//
// Well-formedness is decided by the DUMP SHAPE — a `disabled services` line
// that opens a brace block, and the matching close — and nothing else. Without
// both the dump is Unknown with a Reason: there is no format contract for this
// output across macOS releases, and the same discipline parseLaunchdSpawnHistory
// applies is what stops a renamed key being read as a clean verdict.
//
// The close matters as much as the open. A dump cut short mid-block carries
// only SOME of the domain's overrides, so a label missing from it is missing
// for an unknown reason; and a near-miss line such as `disabled services
// unavailable` shares the marker's prefix while carrying no overrides at all.
// Both used to satisfy a bare prefix match and both then ruled the disabled
// cause out.
//
// ABSENCE MEANS ENABLED, and that is the one place this parser reasons from
// something not being there, so the inference is stated rather than assumed:
// `print-disabled` lists only the labels that carry a disable OVERRIDE, so a
// label the dump does not mention has no override and is enabled. This is not
// a fail-open guess — it is sound only because the opening and closing lines
// already proved we are looking at a COMPLETE dump of the overrides in that
// domain. Weaken that shape check and this inference becomes exactly the
// fail-open guess BOS-1222 exists to remove.
func parseLaunchdDisabledServices(out []byte, label string) (JobDisabledState, string) {
	lines := strings.Split(string(out), "\n")

	// Well-formedness is settled FIRST and for the whole dump, so no entry can
	// be classified out of output this build has not recognised as a
	// print-disabled dump at all.
	openIdx := -1
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, launchdDisabledMarker) && strings.HasSuffix(trimmed, launchdDisabledBlockOpen) {
			openIdx = i
			break
		}
	}
	if openIdx < 0 {
		return JobDisabledStateUnknown, fmt.Sprintf("launchctl print-disabled output carried no %q line opening a %q block (empty output, or a format this build does not recognise)", launchdDisabledMarker, launchdDisabledBlockOpen)
	}

	closeIdx := -1
	for i := openIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == launchdDisabledBlockClose {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return JobDisabledStateUnknown, fmt.Sprintf("launchctl print-disabled output opened a %q block that never closed with %q, so the dump is truncated and a label missing from it proves nothing", launchdDisabledMarker, launchdDisabledBlockClose)
	}

	// Only the entries INSIDE the block are the domain's overrides.
	for _, raw := range lines[openIdx+1 : closeIdx] {
		entry, value, ok := splitLaunchdDisabledEntry(strings.TrimSpace(raw))
		if !ok || entry != label {
			continue
		}
		switch value {
		case "true":
			return JobDisabledStateDisabled, ""
		case "false":
			return JobDisabledStateEnabled, ""
		default:
			return JobDisabledStateUnknown, fmt.Sprintf("launchctl print-disabled reported an unreadable value %q for %q", value, label)
		}
	}

	// See the doc comment: only reachable once the opening and closing lines
	// proved this is a COMPLETE override dump, where an unlisted label carries
	// no override.
	return JobDisabledStateEnabled, ""
}
