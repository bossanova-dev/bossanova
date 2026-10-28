package daemon

import (
	"bytes"
	"strings"
	"testing"

	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/bossalib/daemonstate"
)

func TestCorroborateDetachedVerdict(t *testing.T) {
	cases := []struct {
		name    string
		state   daemon.LaunchAgentJobState
		pid     int
		verdict daemonSupervisionVerdict
		reason  daemonSupervisionReason
	}{
		{"not applicable", daemon.LaunchAgentJobNotApplicable, 0, daemonSupervisionUnsupervised, daemonSupervisionReasonDetached},
		{"not loaded", daemon.LaunchAgentJobNotLoaded, 0, daemonSupervisionUnsupervised, daemonSupervisionReasonDetached},
		{"registered", daemon.LaunchAgentJobLoadedNoPID, 0, daemonSupervisionUnsupervised, daemonSupervisionReasonDetached},
		{"same PID", daemon.LaunchAgentJobRunning, 4242, daemonSupervisionSupervised, daemonSupervisionReasonManagerOwned},
		{"different PID", daemon.LaunchAgentJobRunning, 5555, daemonSupervisionUnknown, daemonSupervisionReasonServiceViewDisputed},
		{"unreadable", daemon.LaunchAgentJobUnknown, 0, daemonSupervisionUnknown, daemonSupervisionReasonServiceViewDisputed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := corroborateDetachedVerdict(daemonSupervisionUnsupervised, daemonSupervisionReasonDetached, 4242, daemon.LaunchAgentJob{State: tc.state, PID: tc.pid})
			if verdict != tc.verdict || reason != tc.reason {
				t.Fatalf("got %v/%v, want %v/%v", verdict, reason, tc.verdict, tc.reason)
			}
		})
	}
	verdict, reason := corroborateDetachedVerdict(daemonSupervisionSupervised, daemonSupervisionReasonManagerOwned, 4242, daemon.LaunchAgentJob{})
	if verdict != daemonSupervisionSupervised || reason != daemonSupervisionReasonManagerOwned {
		t.Fatalf("non-detached changed: %v/%v", verdict, reason)
	}
}

func TestSupervisionCorroborationAcrossReportingSurfaces_DaemonLifecycle_AC6_AC7(t *testing.T) {
	const target = "gui/501/com.bossanova.bossd"
	cases := []struct {
		name        string
		job         daemon.LaunchAgentJob
		wantDoctor  string
		wantStatus  string
		wantWarning bool
		shared      []string
	}{
		{name: "incident same PID", job: daemon.LaunchAgentJob{State: daemon.LaunchAgentJobRunning, PID: 4242}, wantDoctor: "daemon supervision: ok", wantStatus: "supervision: supervised", shared: []string{"4242"}},
		{name: "different PID", job: daemon.LaunchAgentJob{State: daemon.LaunchAgentJobRunning, PID: 5555}, wantDoctor: "daemon supervision: unknown", wantStatus: "supervision: unknown", shared: []string{"launchctl list", "launchctl print " + target, "5555"}},
		{name: "timeout", job: daemon.LaunchAgentJob{Reason: "did not answer within 3s"}, wantDoctor: "daemon supervision: unknown", wantStatus: "supervision: unknown", shared: []string{"launchctl list", "launchctl print " + target, "3s"}},
		{name: "probe failure", job: daemon.LaunchAgentJob{Reason: "exit status 5"}, wantDoctor: "daemon supervision: unknown", wantStatus: "supervision: unknown", shared: []string{"launchctl list", "launchctl print " + target, "exit status 5"}},
		{name: "not loaded", job: daemon.LaunchAgentJob{State: daemon.LaunchAgentJobNotLoaded}, wantDoctor: "daemon supervision: FAIL", wantStatus: "supervision: unsupervised", wantWarning: true},
		{name: "registered without PID", job: daemon.LaunchAgentJob{State: daemon.LaunchAgentJobLoadedNoPID}, wantDoctor: "daemon supervision: FAIL", wantStatus: "supervision: unsupervised", wantWarning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestGroup()
			clearSupervisionWarningEnv(t)
			deps.stubDaemonDoctorProcess(t, nil)
			deps.stubDaemonSupervisionInputs(t, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
			st := &daemon.Status{Installed: true}
			deps.daemonGetStatus = func() (*daemon.Status, error) { return st, nil }
			calls := 0
			tc.job.Target = target
			deps.daemonObserveLaunchAgentJob = func() daemon.LaunchAgentJob { calls++; return tc.job }
			pid := 4242
			writeSupervisionProfile(t, &pid)
			deps.useRealLocalSupervisionObservation(t)
			_, stderr, cmd := newStalenessWarningCommand(t, "boss", "ls")
			deps.warnIfDaemonUnsupervised(cmd)
			if (stderr.Len() > 0) != tc.wantWarning {
				t.Fatalf("warning = %q, want emitted %v", stderr.String(), tc.wantWarning)
			}
			var out bytes.Buffer
			unhealthy, _ := deps.reportDaemonSupervisionGathered(&out, daemonstate.Metadata{PID: pid}, nil)
			status := deps.daemonSupervisionLine(st, pid, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
			if unhealthy != tc.wantWarning || !strings.Contains(out.String(), tc.wantDoctor) || !strings.Contains(status, tc.wantStatus) {
				t.Fatalf("doctor %q unhealthy %v; status %q", out.String(), unhealthy, status)
			}
			for _, text := range tc.shared {
				if !strings.Contains(out.String(), text) || !strings.Contains(status, text) {
					t.Errorf("missing %q in doctor %q or status %q", text, out.String(), status)
				}
			}
			if calls != 3 {
				t.Fatalf("probe calls = %d, want one per observation (3)", calls)
			}
		})
	}
}

func TestSupervisionCorroborationIsLazy(t *testing.T) {
	deps := newTestGroup()
	clearSupervisionWarningEnv(t)
	deps.stubDaemonDoctorProcess(t, nil)
	st := &daemon.Status{Installed: true, Running: true, PID: 4242, PIDKnown: true}
	deps.daemonGetStatus = func() (*daemon.Status, error) { return st, nil }
	deps.daemonObserveLaunchAgentJob = func() daemon.LaunchAgentJob {
		t.Fatal("healthy status probed launchctl print")
		return daemon.LaunchAgentJob{}
	}
	observation := deps.observeDaemonSupervision(daemonstate.Metadata{PID: 4242}, nil, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
	if observation.Verdict != daemonSupervisionSupervised {
		t.Fatalf("got %+v", observation)
	}
	if line := deps.daemonSupervisionLine(st, 4242, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{}); !strings.Contains(line, "supervision: supervised") {
		t.Fatal(line)
	}
}
