package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/spf13/cobra"
)

const startupPID = 4242

// startupNow is the fixed clock every startup test reads, so age is exact and
// nothing sleeps.
var startupNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// startupHost describes one host shape for the startup classifier.
type startupHost struct {
	reachable   bool
	record      bool
	pid         int
	age         time.Duration
	status      daemon.Status
	supervision daemon.SupervisionModeStatus
	ownership   daemon.WatchdogOwnership
	signalErr   error
}

// supervisedBootingHost is the incident: launchd owns a live recorded bossd
// that started 52s ago and has not bound its socket.
func supervisedBootingHost() startupHost {
	return startupHost{
		record:      true,
		pid:         startupPID,
		age:         52 * time.Second,
		status:      daemon.Status{Installed: true, Running: true, PID: startupPID, PIDKnown: true},
		supervision: launchAgentSupervisionStatus(),
	}
}

// stubStartupHost points the profile at a temp app-data dir, writes the state
// record the row asks for and pins every seam the classifier reads.
func (deps *Group) stubStartupHost(t *testing.T, host startupHost) (socketPath string) {
	t.Helper()
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	if host.record {
		if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
			PID:       host.pid,
			StartedAt: startupNow.Add(-host.age),
		}); err != nil {
			t.Fatalf("daemonstate.Write: %v", err)
		}
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return host.reachable }
	deps.daemonNow = func() time.Time { return startupNow }
	status := host.status
	deps.daemonGetStatus = func() (*daemon.Status, error) { return &status, nil }
	deps.stubDaemonDoctorProcess(t, host.signalErr)
	deps.stubDaemonSupervisionInputs(t, host.supervision, host.ownership)
	return socketPath
}

// stubWedgedDaemonSocket makes the socket unreachable and moves the clock past
// the startup grace, so a fixture that records a fresh StartedAt reads as a
// wedged daemon rather than one still starting.
func (deps *Group) stubWedgedDaemonSocket(t *testing.T) {
	t.Helper()
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.daemonNow = func() time.Time { return time.Now().Add(daemonStartupGrace + time.Minute) }
}

func TestObserveDaemonStartupClassifiesASupervisedBootInProgress(t *testing.T) {
	row := func(edit func(*startupHost)) startupHost {
		host := supervisedBootingHost()
		edit(&host)
		return host
	}
	cases := []struct {
		name string
		host startupHost
		want bool
	}{
		{"supervised, live, unreachable, 52s old", supervisedBootingHost(), true},
		{"exactly at the grace", row(func(h *startupHost) { h.age = daemonStartupGrace }), true},
		{"past the grace is wedged", row(func(h *startupHost) { h.age = daemonStartupGrace + time.Second }), false},
		{"negative age (clock skew)", row(func(h *startupHost) { h.age = -time.Second }), false},
		{"socket reachable", row(func(h *startupHost) { h.reachable = true }), false},
		{"detached: the service manager does not own it", row(func(h *startupHost) {
			h.status = daemon.Status{Installed: true}
		}), false},
		{"foreign PID: the service manager owns another process", row(func(h *startupHost) {
			h.status.PID = startupPID + 1
		}), false},
		{"no state record", row(func(h *startupHost) { h.record = false }), false},
		{"record names no PID", row(func(h *startupHost) { h.pid = 0 }), false},
		{"recorded PID is dead", row(func(h *startupHost) { h.signalErr = syscall.ESRCH }), false},
		{"watchdog-owned on the unattended substrate", row(func(h *startupHost) {
			h.status = daemon.Status{}
			h.supervision = unattendedSupervisionStatus(daemon.UnattendedInstallPresent)
			h.ownership = watchdogOwnership(daemon.WatchdogOwnershipLoaded)
		}), true},
		{"watchdog not loaded", row(func(h *startupHost) {
			h.status = daemon.Status{}
			h.supervision = unattendedSupervisionStatus(daemon.UnattendedInstallPresent)
			h.ownership = watchdogOwnership(daemon.WatchdogOwnershipNotLoaded)
		}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestGroup()
			socketPath := deps.stubStartupHost(t, tc.host)
			obs := deps.observeDaemonStartup(socketPath)
			if obs.Starting != tc.want {
				t.Fatalf("Starting = %v, want %v (observation %+v)", obs.Starting, tc.want, obs)
			}
			if tc.want && (obs.PID != startupPID || obs.Age != tc.host.age) {
				t.Fatalf("observation = %+v, want PID %d and age %s", obs, startupPID, tc.host.age)
			}
		})
	}

	t.Run("probing disabled is not starting", func(t *testing.T) {
		deps := newTestGroup()
		socketPath := deps.stubStartupHost(t, supervisedBootingHost())
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		if obs := deps.observeDaemonStartup(socketPath); obs.Starting {
			t.Fatalf("Starting = true under BOSS_DAEMON_SKIP_LAUNCHCTL: %+v", obs)
		}
	})
}

func TestDaemonStartupDescribeNamesThePIDAndWholeSecondAge(t *testing.T) {
	got := daemonStartupObservation{Starting: true, PID: 68863, Age: 52*time.Second + 600*time.Millisecond}.describe()
	want := "bossd PID 68863 is still starting (53s since it started; it has not bound the daemon socket yet)"
	if got != want {
		t.Fatalf("describe() = %q, want %q", got, want)
	}
}

func TestRunDaemonStartReportsADaemonThatIsStillStarting_DaemonLifecycle_AC216801(t *testing.T) {
	refuse := func(string, daemon.DetachedFallbackPolicy) (daemon.StartMode, error) {
		return daemon.StartModeUnknown, errors.Join(daemon.ErrServiceInstalledNotServing, errors.New("/tmp/com.example.bossd.plist"))
	}

	t.Run("starting", func(t *testing.T) {
		deps := newTestGroup()
		deps.stubStartupHost(t, supervisedBootingHost())
		deps.daemonEnsureRunningWithPolicy = refuse
		var err error
		out := captureStdout(t, func() { err = deps.runDaemonStart(daemonStartCommand(t, false)) })
		if err == nil {
			t.Fatalf("runDaemonStart returned nil for a daemon that is not serving yet; stdout = %q", out)
		}
		msg := err.Error()
		for _, want := range []string{"PID 4242", "still starting", "52s"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q is missing %q", msg, want)
			}
		}
		for _, banned := range []string{"restart", "--detached"} {
			if strings.Contains(msg, banned) {
				t.Errorf("error %q must not suggest %q to an operator whose daemon is booting", msg, banned)
			}
		}
		if errors.Is(err, daemon.ErrServiceInstalledNotServing) {
			t.Errorf("a still-starting daemon must not wrap ErrServiceInstalledNotServing: %v", err)
		}
		if strings.Contains(out, "Daemon started.") {
			t.Errorf("stdout = %q, must not report a start that has not served", out)
		}
	})

	t.Run("past the grace keeps today's refusal", func(t *testing.T) {
		deps := newTestGroup()
		host := supervisedBootingHost()
		host.age = daemonStartupGrace + time.Second
		deps.stubStartupHost(t, host)
		deps.daemonEnsureRunningWithPolicy = refuse
		var err error
		captureStdout(t, func() { err = deps.runDaemonStart(daemonStartCommand(t, false)) })
		if !errors.Is(err, daemon.ErrServiceInstalledNotServing) {
			t.Fatalf("err = %v, want ErrServiceInstalledNotServing", err)
		}
		if !strings.Contains(err.Error(), "'boss daemon restart' to re-bootstrap the service") {
			t.Fatalf("err = %v, want today's refusal text", err)
		}
	})
}

// runStartupDoctor runs doctor on a darwin fixture whose recorded bossd is
// launchd-owned, alive and age old at startupNow, with its socket unreachable.
func (deps *Group) runStartupDoctor(t *testing.T, age time.Duration, status *daemon.Status, setup ...func()) (string, error) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("doctor's socket check sits after the macOS-only early return")
	}
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	home, _, stagedPath := deps.prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	// The staged binary predates the process, so the running-image check
	// reads current and adds no verdict of its own.
	staged := startupNow.Add(-time.Hour)
	if err := os.Chtimes(stagedPath, staged, staged); err != nil {
		t.Fatalf("chtimes staged bossd: %v", err)
	}
	deps.writeDaemonDoctorStateStartedAt(t, stagedPath, startupNow.Add(-age))
	if status != nil {
		deps.daemonGetStatus = func() (*daemon.Status, error) { return status, nil }
	}
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.daemonNow = func() time.Time { return startupNow }
	for _, apply := range setup {
		apply()
	}

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := deps.runDaemonDoctor(cmd)
	return output.String(), err
}

func TestRunDaemonDoctorReportsASupervisedDaemonThatIsStillStarting_DaemonLifecycle_AC216802(t *testing.T) {
	deps := newTestGroup()
	got, err := deps.runStartupDoctor(t, 52*time.Second, nil)
	if err != nil {
		t.Fatalf("starting must not by itself make doctor unhealthy, got err=%v:\n%s", err, got)
	}
	socketLine := daemonDoctorLine(t, got, "daemon socket:")
	for _, want := range []string{"— starting:", "PID 4242", "52s"} {
		if !strings.Contains(socketLine, want) {
			t.Errorf("socket line %q is missing %q", socketLine, want)
		}
	}
	for _, banned := range []string{"FAIL daemon socket", "run 'boss daemon start'", "startup diagnosis:"} {
		if strings.Contains(got, banned) {
			t.Errorf("doctor printed %q for a daemon that is still starting:\n%s", banned, got)
		}
	}
	if !strings.Contains(got, "wait for bossd to finish starting, then re-run 'boss daemon doctor'") {
		t.Errorf("doctor did not tell the operator to wait:\n%s", got)
	}
}

// A KeepAlive crash loop's respawned instance can be alive, launchd-owned and
// inside the grace. Doctor reports it as starting, so its remediation must
// say wait too, never the restart that reproduces the crash.
func TestRunDaemonDoctorRemediatesAStartingCrashLoopByWaiting_DaemonLifecycle_AC216802(t *testing.T) {
	deps := newTestGroup()
	got, _ := deps.runStartupDoctor(t, 52*time.Second, nil, func() {
		deps.stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
			State:             daemon.SpawnStateFailing,
			Target:            daemonDoctorSpawnHistoryTarget,
			Runs:              47,
			RunsKnown:         true,
			LastExitCode:      1,
			LastExitCodeKnown: true,
		}, nil)
	})
	if !strings.Contains(daemonDoctorLine(t, got, "daemon socket:"), "— starting:") {
		t.Fatalf("fixture is not in the starting state:\n%s", got)
	}
	_, remediation, found := strings.Cut(got, "Remediation:")
	if !found {
		t.Fatalf("a crash-looping job must still print a Remediation section:\n%s", got)
	}
	if !strings.Contains(remediation, "wait for bossd to finish starting") {
		t.Errorf("remediation for a daemon reported as starting does not say wait:\n%s", remediation)
	}
	for _, unwanted := range []string{"run 'boss daemon restart'", "run 'boss daemon start'"} {
		if strings.Contains(remediation, unwanted) {
			t.Errorf("a daemon reported as starting was offered %q:\n%s", unwanted, remediation)
		}
	}
}

func TestRunDaemonDoctorStillFailsAWedgedOrUnsupervisedSocket_DaemonLifecycle_AC216802(t *testing.T) {
	cases := []struct {
		name   string
		age    time.Duration
		status *daemon.Status
	}{
		{name: "older than the grace", age: daemonStartupGrace + time.Second},
		{name: "not supervised", age: 52 * time.Second, status: &daemon.Status{Installed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestGroup()
			got, err := deps.runStartupDoctor(t, tc.age, tc.status)
			if !errors.Is(err, errDaemonDoctorUnhealthy) {
				t.Fatalf("err = %v, want errDaemonDoctorUnhealthy:\n%s", err, got)
			}
			if !strings.Contains(got, "FAIL daemon socket:") || strings.Contains(got, "— starting:") {
				t.Fatalf("doctor did not keep the FAIL verdict:\n%s", got)
			}
		})
	}
}

func TestRunDaemonRestartAnnouncesItIsStoppingADaemonMidStartup_DaemonLifecycle_AC216803(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[asJSON], func(t *testing.T) {
			deps := newTestGroup()
			deps.stubStartupHost(t, supervisedBootingHost())
			deps.daemonStandaloneServed = func(*daemon.Status) bool { return false }
			var stderr, stdout bytes.Buffer
			var events []string
			restarted := false
			deps.daemonSocketReachable = func(string) bool { return restarted }
			deps.daemonStop = func() error {
				events = append(events, "stop:note="+map[bool]string{false: "absent", true: "present"}[strings.Contains(stderr.String(), "Note:")])
				return nil
			}
			deps.waitForDaemonSocketGone = func(string) bool { return true }
			deps.restartDaemon = func() error {
				restarted = true
				events = append(events, "restart")
				return nil
			}

			cmd := &cobra.Command{}
			cmd.Flags().Bool(cliutil.JSONFlagName, false, "")
			if asJSON {
				if err := cmd.Flags().Set(cliutil.JSONFlagName, "true"); err != nil {
					t.Fatalf("set --json: %v", err)
				}
			}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			var err error
			captureStdout(t, func() { err = deps.runDaemonRestart(cmd) })
			if err != nil {
				t.Fatalf("runDaemonRestart: %v", err)
			}
			if want := []string{"stop:note=present", "restart"}; strings.Join(events, ",") != strings.Join(want, ",") {
				t.Fatalf("events = %v, want %v (the note must precede the stop)", events, want)
			}
			note := stderr.String()
			for _, want := range []string{"Note: bossd PID 4242 is still starting", "restarting stops that startup and begins a new one"} {
				if !strings.Contains(note, want) {
					t.Errorf("stderr %q is missing %q", note, want)
				}
			}
			if !asJSON {
				return
			}
			dec := json.NewDecoder(&stdout)
			var envelope map[string]any
			if err := dec.Decode(&envelope); err != nil {
				t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, stdout.String())
			}
			if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
				t.Fatalf("stdout carries more than one JSON value (err=%v)", err)
			}
		})
	}
}

func TestRunDaemonRestartIsSilentWhenTheDaemonIsNotStarting(t *testing.T) {
	deps := newTestGroup()
	host := supervisedBootingHost()
	host.age = daemonStartupGrace + time.Second
	deps.stubStartupHost(t, host)
	deps.daemonStandaloneServed = func(*daemon.Status) bool { return false }
	restarted := false
	deps.daemonSocketReachable = func(string) bool { return restarted }
	deps.daemonStop = func() error { return nil }
	deps.waitForDaemonSocketGone = func(string) bool { return true }
	deps.restartDaemon = func() error { restarted = true; return nil }

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	var err error
	captureStdout(t, func() { err = deps.runDaemonRestart(cmd) })
	if err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no startup note for a wedged daemon", stderr.String())
	}
}
