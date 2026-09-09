package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSpawnFixture loads a captured `launchctl print` dump.
//
// BOS-1183: the fixtures are in-repo on purpose. `launchctl print` has no
// format contract across macOS releases, so the only way a future rename of
// `runs` / `last exit code` becomes visible is a test that reads the shape we
// parsed against. A parser that silently stops finding its keys must fail a
// test here rather than degrade to a clean verdict on a broken daemon.
func readSpawnFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "launchctl-print", name)
	data, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return data
}

func TestParseLaunchdSpawnHistoryFixtures(t *testing.T) {
	tests := []struct {
		name              string
		fixture           string
		wantState         SpawnState
		wantRuns          int
		wantRunsKnown     bool
		wantExitCode      int
		wantExitCodeKnown bool
		wantNeverExited   bool
		wantServiceState  string
		wantReason        bool // Reason must be non-empty
	}{
		{
			// The exact state observed in the 2026-09-06 incident: launchd had
			// the job registered (so `launchctl list` exits 0 and Status.Running
			// reports true) but had never attempted to spawn it.
			name:             "never_spawned",
			fixture:          "never-spawned.txt",
			wantState:        SpawnStateNeverSpawned,
			wantRuns:         0,
			wantRunsKnown:    true,
			wantNeverExited:  true,
			wantServiceState: "not running",
		},
		{
			name:              "crash_loop",
			fixture:           "crash-loop.txt",
			wantState:         SpawnStateFailing,
			wantRuns:          47,
			wantRunsKnown:     true,
			wantExitCode:      1,
			wantExitCodeKnown: true,
			wantServiceState:  "not running",
		},
		{
			// runs = 1 with "(never exited)" is the NORMAL shape of a job that
			// is up right now and has never yet exited. The same
			// "(never exited)" text at runs = 0 is the incident. That asymmetry
			// is the whole discriminator.
			name:             "healthy",
			fixture:          "healthy.txt",
			wantState:        SpawnStateHealthy,
			wantRuns:         1,
			wantRunsKnown:    true,
			wantNeverExited:  true,
			wantServiceState: "running",
		},
		{
			// A dump whose keys have been renamed by a future macOS. Fail
			// closed: unknown, never healthy.
			name:             "unparseable",
			fixture:          "unparseable.txt",
			wantState:        SpawnStateUnknown,
			wantServiceState: "not running",
			wantReason:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLaunchdSpawnHistory(readSpawnFixture(t, tt.fixture))

			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (reason %q)", got.State, tt.wantState, got.Reason)
			}
			if got.Runs != tt.wantRuns {
				t.Errorf("Runs = %d, want %d", got.Runs, tt.wantRuns)
			}
			if got.RunsKnown != tt.wantRunsKnown {
				t.Errorf("RunsKnown = %v, want %v", got.RunsKnown, tt.wantRunsKnown)
			}
			if got.LastExitCode != tt.wantExitCode {
				t.Errorf("LastExitCode = %d, want %d", got.LastExitCode, tt.wantExitCode)
			}
			if got.LastExitCodeKnown != tt.wantExitCodeKnown {
				t.Errorf("LastExitCodeKnown = %v, want %v", got.LastExitCodeKnown, tt.wantExitCodeKnown)
			}
			if got.NeverExited != tt.wantNeverExited {
				t.Errorf("NeverExited = %v, want %v", got.NeverExited, tt.wantNeverExited)
			}
			if got.ServiceState != tt.wantServiceState {
				t.Errorf("ServiceState = %q, want %q", got.ServiceState, tt.wantServiceState)
			}
			if tt.wantReason && strings.TrimSpace(got.Reason) == "" {
				t.Error("Reason is empty; every unknown verdict must say why")
			}
			if !tt.wantReason && got.Reason != "" {
				t.Errorf("Reason = %q, want empty for a determinate verdict", got.Reason)
			}
		})
	}
}

// TestParseLaunchdSpawnHistoryUnparseableIsNotHealthy states the fail-closed
// invariant on its own, separate from the table, because it is the property
// that makes this probe worth having: an output we cannot read must never be
// reported as a working daemon.
func TestParseLaunchdSpawnHistoryUnparseableIsNotHealthy(t *testing.T) {
	got := parseLaunchdSpawnHistory(readSpawnFixture(t, "unparseable.txt"))
	if got.State == SpawnStateHealthy {
		t.Fatal("State = healthy for output whose spawn-history keys are absent; must fail closed")
	}
	if got.State != SpawnStateUnknown {
		t.Errorf("State = %q, want %q", got.State, SpawnStateUnknown)
	}
	if got.Reason == "" {
		t.Error("Reason is empty; an unknown verdict must name what was missing")
	}
}

func TestParseLaunchdSpawnHistoryEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantState SpawnState
	}{
		{
			name:      "empty_output",
			in:        "",
			wantState: SpawnStateUnknown,
		},
		{
			name:      "whitespace_only",
			in:        "\n\t\n   \n",
			wantState: SpawnStateUnknown,
		},
		{
			// Contradictory: launchd cannot have zero spawns and a real exit
			// status. Guessing either way here is how a fail-open verdict gets
			// introduced, so it is unknown.
			name:      "zero_runs_with_numeric_exit_code",
			in:        "com.bossanova.bossd = {\n\tstate = not running\n\truns = 0\n\tlast exit code = 2\n}\n",
			wantState: SpawnStateUnknown,
		},
		{
			name:      "runs_without_exit_code_line",
			in:        "com.bossanova.bossd = {\n\tstate = not running\n\truns = 3\n}\n",
			wantState: SpawnStateUnknown,
		},
		{
			name:      "exit_code_without_runs_line",
			in:        "com.bossanova.bossd = {\n\tstate = not running\n\tlast exit code = 0\n}\n",
			wantState: SpawnStateUnknown,
		},
		{
			name:      "unreadable_exit_code_value",
			in:        "com.bossanova.bossd = {\n\truns = 2\n\tlast exit code = (some future wording)\n}\n",
			wantState: SpawnStateUnknown,
		},
		{
			name:      "clean_exit_after_a_spawn_is_healthy",
			in:        "com.bossanova.bossd = {\n\tstate = not running\n\truns = 5\n\tlast exit code = 0\n}\n",
			wantState: SpawnStateHealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLaunchdSpawnHistory([]byte(tt.in))
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (reason %q)", got.State, tt.wantState, got.Reason)
			}
			if got.State == SpawnStateUnknown && got.Reason == "" {
				t.Error("Reason is empty for an unknown verdict")
			}
		})
	}
}

// TestSpawnHistoryTargetFollowsTheSubstrate pins BOS-1204's target resolution
// from either platform's test run.
//
// The rejected-configuration row is the one worth stating explicitly: a typo OF
// "unattended" is likeliest on precisely the host that has a watchdog, and
// probing a root-owned job because a settings value did not parse is the
// opposite of the fail-closed direction the resolver itself takes.
func TestSpawnHistoryTargetFollowsTheSubstrate(t *testing.T) {
	const launchAgentTarget = "gui/501/com.bossanova.bossd"

	for _, tc := range []struct {
		name        string
		supervision SupervisionModeStatus
		want        string
	}{
		{
			name:        "launch-agent",
			supervision: SupervisionModeStatus{Mode: SupervisionModeLaunchAgent, Configurable: true},
			want:        launchAgentTarget,
		},
		{
			name:        "unattended",
			supervision: SupervisionModeStatus{Mode: SupervisionModeUnattended, Configurable: true},
			want:        "system/" + WatchdogLabel,
		},
		{
			name:        "a zero status names no mode and must not route to the watchdog",
			supervision: SupervisionModeStatus{},
			want:        launchAgentTarget,
		},
		{
			name: "a rejected configuration keeps the LaunchAgent target",
			supervision: SupervisionModeStatus{
				Configured:   "unattnded",
				Configurable: true,
				Err:          ErrUnknownSupervisionMode,
			},
			want: launchAgentTarget,
		},
		{
			name: "an unreadable settings file falls back with the default target",
			supervision: SupervisionModeStatus{
				Mode:         SupervisionModeLaunchAgent,
				Configurable: true,
				SettingsErr:  errSpawnHistoryTestSettings,
			},
			want: launchAgentTarget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spawnHistoryTarget(tc.supervision, launchAgentTarget); got != tc.want {
				t.Fatalf("spawnHistoryTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

var errSpawnHistoryTestSettings = errors.New("settings.json could not be parsed")

// TestParseLaunchdDisabledServices is BOS-1222's parser truth table.
//
// The rows that carry the ticket are the ones that must NOT read as
// "enabled": an empty dump, a dump in a format this build does not recognise,
// and a value neither true nor false. Reporting enabled from any of those
// would rule the disabled cause out on evidence that does not exist, which is
// the same fail-open guess the console remediation used to make one layer up.
func TestParseLaunchdDisabledServices(t *testing.T) {
	const label = "com.bossanova.bossd"
	const wellFormed = "disabled services = {\n" +
		"\t\"com.example.other\" => false\n" +
		"\t\"com.bossanova.bossd\" => true\n" +
		"}\n"

	for _, tc := range []struct {
		name       string
		out        string
		wantState  JobDisabledState
		wantReason bool
	}{
		{
			name:      "an override of true is disabled",
			out:       wellFormed,
			wantState: JobDisabledStateDisabled,
		},
		{
			name:      "an override of false is enabled",
			out:       "disabled services = {\n\t\"com.bossanova.bossd\" => false\n}\n",
			wantState: JobDisabledStateEnabled,
		},
		{
			name:      "a label absent from a well-formed dump carries no override",
			out:       "disabled services = {\n\t\"com.example.other\" => true\n}\n",
			wantState: JobDisabledStateEnabled,
		},
		{
			name:      "an empty override dump is still a complete dump",
			out:       "disabled services = {\n}\n",
			wantState: JobDisabledStateEnabled,
		},
		{
			name:       "empty output is unknown, never enabled",
			out:        "",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			name:       "output with no marker line is unknown, never enabled",
			out:        "\t\"com.bossanova.bossd\" => true\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			name:       "an unreadable value is unknown, not a guess either way",
			out:        "disabled services = {\n\t\"com.bossanova.bossd\" => maybe\n}\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			name:       "a refusal message is not a dump",
			out:        "Could not find domain for\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			// The near miss: it shares the marker's PREFIX while carrying no
			// overrides at all, so reading it as a complete dump would rule
			// the disabled cause out from output that measured nothing.
			name:       "a near-miss line sharing the marker prefix is not a dump",
			out:        "disabled services unavailable\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			name:       "an error appended to the marker prefix is not a dump",
			out:        "disabled services = could not be read\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			// A dump cut short carries only SOME of the domain's overrides, so
			// a label missing from it is missing for an unknown reason.
			name:       "a dump opened but never closed is truncated, never enabled",
			out:        "disabled services = {\n\t\"com.example.other\" => true\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			name:       "a truncated dump is unknown even when it already listed the label",
			out:        "disabled services = {\n\t\"com.bossanova.bossd\" => false\n",
			wantState:  JobDisabledStateUnknown,
			wantReason: true,
		},
		{
			// Entries AFTER the block's close are not the domain's overrides,
			// so an override quoted in trailing prose cannot flip the verdict.
			name:      "an entry after the block close is not an override",
			out:       "disabled services = {\n}\n\t\"com.bossanova.bossd\" => true\n",
			wantState: JobDisabledStateEnabled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := parseLaunchdDisabledServices([]byte(tc.out), label)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q (reason %q)", state, tc.wantState, reason)
			}
			if tc.wantReason && reason == "" {
				t.Fatal("an indeterminate verdict must carry a Reason")
			}
			if !tc.wantReason && reason != "" {
				t.Fatalf("a determinate verdict carried a Reason %q", reason)
			}
		})
	}
}

// TestParseLaunchdDisabledServicesRejectsTheKeyValueSplitter guards the reason
// splitLaunchdDisabledEntry exists at all: splitLaunchdKeyValue deliberately
// REJECTS the `=>` form, so reusing it here would make every entry invisible
// and every dump read as "no override" — a silent fail-open.
func TestParseLaunchdDisabledServicesRejectsTheKeyValueSplitter(t *testing.T) {
	const entry = "\t\"com.bossanova.bossd\" => true"
	if _, _, ok := splitLaunchdKeyValue(strings.TrimSpace(entry)); ok {
		t.Fatal("splitLaunchdKeyValue accepted a `=>` entry; the dedicated matcher's premise no longer holds")
	}
	label, value, ok := splitLaunchdDisabledEntry(strings.TrimSpace(entry))
	if !ok || label != "com.bossanova.bossd" || value != "true" {
		t.Fatalf("splitLaunchdDisabledEntry = %q/%q/%v, want the label and \"true\"", label, value, ok)
	}
}

// TestJobDisabledDomainFollowsTheSubstrate mirrors
// TestSpawnHistoryTargetFollowsTheSubstrate, and for the same reason: under
// `unattended` the job launchd spawns is the root-owned watchdog in the
// `system` domain, so gui/<uid> holds no override that could explain
// anything. The rejected-configuration row is the one worth stating: a typo OF
// "unattended" is likeliest on precisely the host that has a watchdog, and
// probing a root-owned domain because a settings value did not parse is the
// opposite of the fail-closed direction the resolver takes.
func TestJobDisabledDomainFollowsTheSubstrate(t *testing.T) {
	const launchAgentDomain = "gui/501"
	const launchAgentLabel = "com.bossanova.bossd"

	for _, tc := range []struct {
		name        string
		supervision SupervisionModeStatus
		wantDomain  string
		wantLabel   string
	}{
		{
			name:        "launch-agent",
			supervision: SupervisionModeStatus{Mode: SupervisionModeLaunchAgent, Configurable: true},
			wantDomain:  launchAgentDomain,
			wantLabel:   launchAgentLabel,
		},
		{
			name:        "unattended",
			supervision: SupervisionModeStatus{Mode: SupervisionModeUnattended, Configurable: true},
			wantDomain:  WatchdogDomain,
			wantLabel:   WatchdogLabel,
		},
		{
			name:        "a zero status names no mode and must not route to the system domain",
			supervision: SupervisionModeStatus{},
			wantDomain:  launchAgentDomain,
			wantLabel:   launchAgentLabel,
		},
		{
			name: "a rejected configuration keeps the LaunchAgent domain",
			supervision: SupervisionModeStatus{
				Configured:   "unattnded",
				Configurable: true,
				Err:          ErrUnknownSupervisionMode,
			},
			wantDomain: launchAgentDomain,
			wantLabel:  launchAgentLabel,
		},
		{
			name: "an unreadable settings file falls back with the default domain",
			supervision: SupervisionModeStatus{
				Mode:         SupervisionModeLaunchAgent,
				Configurable: true,
				SettingsErr:  errSpawnHistoryTestSettings,
			},
			wantDomain: launchAgentDomain,
			wantLabel:  launchAgentLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain, label := jobDisabledDomain(tc.supervision, launchAgentDomain, launchAgentLabel)
			if domain != tc.wantDomain || label != tc.wantLabel {
				t.Fatalf("jobDisabledDomain = %q/%q, want %q/%q", domain, label, tc.wantDomain, tc.wantLabel)
			}
		})
	}
}

// TestJobDisabledDomainComposesTheWatchdogTarget pins the agreement the shared
// WatchdogDomain constant buys, at the one place a constant alone cannot
// reach: the SEPARATOR. jobDisabledDomain hands back the domain and label
// unjoined and spawnHistoryTarget hands back WatchdogTarget()'s joined form,
// so the two derivations are only equivalent while "/" is how launchd spells a
// service target. A future domain move now carries both; a future SEPARATOR
// change would not, and that is what this reads.
func TestJobDisabledDomainComposesTheWatchdogTarget(t *testing.T) {
	unattended := SupervisionModeStatus{Mode: SupervisionModeUnattended, Configurable: true}

	domain, label := jobDisabledDomain(unattended, "gui/501", "com.bossanova.bossd")
	if got, want := domain+"/"+label, WatchdogTarget(); got != want {
		t.Fatalf("jobDisabledDomain composed = %q, want WatchdogTarget() = %q", got, want)
	}
	if got := spawnHistoryTarget(unattended, "gui/501/com.bossanova.bossd"); got != WatchdogTarget() {
		t.Fatalf("spawnHistoryTarget = %q, want WatchdogTarget() = %q — the two resolvers no longer name the same job", got, WatchdogTarget())
	}
}
