package daemon

import (
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
