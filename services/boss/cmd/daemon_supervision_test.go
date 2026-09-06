package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/bossalib/daemonstate"
)

// TestDaemonSupervisionVerdictsMatchDoctor is the executable form of what used
// to be a comment.
//
// BOS-1183 shipped daemonSupervisionLine as a hand-copied mirror of
// reportDaemonSupervision's ladder, and the only thing holding the two in step
// was prose in both files saying "change one and change the other". A comment
// asserting an invariant cannot fail when the invariant breaks. Both renderers
// now decide through daemonSupervisionOfLiveRecord, and this test drives the
// whole (Installed, Running, service PID, recorded PID) matrix through BOTH and
// requires the verdict tokens to agree — with exactly one declared exception,
// spelled out per row rather than waved at.
func TestDaemonSupervisionVerdictsMatchDoctor(t *testing.T) {
	const recordedPID = 4242

	cases := []struct {
		name string
		st   daemon.Status
		// doctorDiverges is set only on the one row where the two surfaces are
		// documented to differ: doctor answers "unknown (no service is
		// installed)" because the not-installed check owns that fact and its
		// remedy, while status has already printed "Daemon is not installed."
		// and labels the live recorded daemon unsupervised.
		doctorDiverges bool
		wantStatus     string
		wantDoctor     string
	}{
		{
			name:       "service manager owns the recorded daemon",
			st:         daemon.Status{Installed: true, Running: true, PID: recordedPID},
			wantStatus: "supervised",
			wantDoctor: "supervised",
		},
		{
			name:       "service manager does not know the job",
			st:         daemon.Status{Installed: true, Running: false},
			wantStatus: "unsupervised",
			wantDoctor: "unsupervised",
		},
		{
			name:       "service manager owns a different PID",
			st:         daemon.Status{Installed: true, Running: true, PID: recordedPID + 1},
			wantStatus: "unsupervised",
			wantDoctor: "unsupervised",
		},
		{
			name:       "service manager reports running with no PID",
			st:         daemon.Status{Installed: true, Running: true, PID: 0},
			wantStatus: "unknown",
			wantDoctor: "unknown",
		},
		{
			name:           "no service installed",
			st:             daemon.Status{Installed: false, Running: false},
			doctorDiverges: true,
			wantStatus:     "unsupervised",
			wantDoctor:     "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			stubDaemonDoctorProcess(t, nil)
			st := tc.st

			gotStatus := supervisionVerdictToken(t, daemonSupervisionLine(&st, recordedPID), "supervision: ")

			previous := daemonGetStatus
			daemonGetStatus = func() (*daemon.Status, error) { return &st, nil }
			t.Cleanup(func() { daemonGetStatus = previous })
			var out bytes.Buffer
			reportDaemonSupervision(&out, daemonstate.Metadata{PID: recordedPID}, nil)
			gotDoctor := doctorSupervisionVerdictToken(t, out.String())

			if gotStatus != tc.wantStatus {
				t.Fatalf("status verdict = %q, want %q", gotStatus, tc.wantStatus)
			}
			if gotDoctor != tc.wantDoctor {
				t.Fatalf("doctor verdict = %q, want %q", gotDoctor, tc.wantDoctor)
			}
			if tc.doctorDiverges {
				if gotStatus == gotDoctor {
					t.Fatalf("row is declared divergent but both surfaces said %q; drop the exception", gotStatus)
				}
				return
			}
			if gotStatus != gotDoctor {
				t.Fatalf("status said %q and doctor said %q for the same host; the two surfaces must not disagree", gotStatus, gotDoctor)
			}
		})
	}
}

// supervisionVerdictToken extracts the one-word verdict from a status line so
// the comparison is on the decision rather than on wording each surface owns.
func supervisionVerdictToken(t *testing.T, line, prefix string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		t.Fatalf("line = %q, want prefix %q", line, prefix)
	}
	return strings.TrimSpace(strings.SplitN(rest, " ", 2)[0])
}

// doctorSupervisionVerdictToken maps doctor's ok/FAIL/unknown vocabulary onto
// the status line's supervised/unsupervised/unknown so the two are comparable.
func doctorSupervisionVerdictToken(t *testing.T, out string) string {
	t.Helper()
	line := strings.TrimSpace(out)
	token := supervisionVerdictToken(t, line, "daemon supervision: ")
	switch token {
	case "ok":
		return "supervised"
	case "FAIL":
		return "unsupervised"
	case "unknown":
		return "unknown"
	default:
		t.Fatalf("doctor output = %q, unrecognised verdict token %q", out, token)
		return ""
	}
}

// TestDaemonSupervisionOfLiveRecordDelegatesToClassifyServingMode pins the
// delegation itself. The point of routing through daemon.ClassifyServingMode is
// that `boss daemon restart` decides its strategy from the same function, so a
// second ladder here could silently drift back apart from it.
func TestDaemonSupervisionOfLiveRecordDelegatesToClassifyServingMode(t *testing.T) {
	const recordedPID = 909

	for _, tc := range []struct {
		name        string
		st          daemon.Status
		wantVerdict daemonSupervisionVerdict
		wantServing daemon.ServingMode
	}{
		{
			name:        "supervised agrees with the serving probe",
			st:          daemon.Status{Installed: true, Running: true, PID: recordedPID},
			wantVerdict: daemonSupervisionSupervised,
			wantServing: daemon.ServingModeSupervised,
		},
		{
			name:        "detached agrees with the serving probe",
			st:          daemon.Status{Installed: true, Running: false},
			wantVerdict: daemonSupervisionUnsupervised,
			wantServing: daemon.ServingModeStandalone,
		},
		{
			name:        "foreign PID agrees with the serving probe",
			st:          daemon.Status{Installed: true, Running: true, PID: recordedPID + 1},
			wantVerdict: daemonSupervisionUnsupervised,
			wantServing: daemon.ServingModeStandalone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			got, _ := daemonSupervisionOfLiveRecord(&st, recordedPID)
			if got != tc.wantVerdict {
				t.Fatalf("verdict = %v, want %v", got, tc.wantVerdict)
			}
			serving := daemon.ClassifyServingMode(daemon.ServingFacts{
				Installed:           st.Installed,
				Running:             st.Running,
				ServiceManagerPID:   st.PID,
				StandalonePID:       recordedPID,
				StandaloneAlive:     true,
				StandaloneSupported: true,
			})
			if serving != tc.wantServing {
				t.Fatalf("ClassifyServingMode = %q, want %q; the reporting verdict no longer tracks the restart probe", serving, tc.wantServing)
			}
		})
	}
}

// TestDaemonSupervisionOfLiveRecordRefusesAnUnparseableServiceView pins the one
// place this reporting surface deliberately answers differently from
// daemon.ClassifyServingMode. A service manager that reports the job running
// while naming no PID is a tooling failure — unparseable launchctl output, a
// failed systemd MainPID read — and the restart path is right to treat the live
// recorded daemon as what it must preserve. A REPORT that did the same would
// print an unsupervised fault nobody observed.
func TestDaemonSupervisionOfLiveRecordRefusesAnUnparseableServiceView(t *testing.T) {
	st := daemon.Status{Installed: true, Running: true, PID: 0}
	verdict, reason := daemonSupervisionOfLiveRecord(&st, 77)
	if verdict != daemonSupervisionUnknown {
		t.Fatalf("verdict = %v, want unknown for a service view with no PID", verdict)
	}
	if reason != daemonSupervisionReasonNoServicePID {
		t.Fatalf("reason = %v, want daemonSupervisionReasonNoServicePID", reason)
	}
	if serving := daemon.ClassifyServingMode(daemon.ServingFacts{
		Running:             true,
		ServiceManagerPID:   0,
		StandalonePID:       77,
		StandaloneAlive:     true,
		StandaloneSupported: true,
	}); serving != daemon.ServingModeStandalone {
		t.Fatalf("ClassifyServingMode = %q, want standalone; this test documents a divergence that no longer exists", serving)
	}
}

// TestReportDaemonSupervisionFlagsFollowTheVerdict pins that doctor's
// unhealthy/remediation flags are the shared verdict rather than a per-rung
// literal.
//
// Nothing covered this before: the FAIL rungs asserted their printed text in
// other tests, but the returned flags — which decide doctor's exit code and
// whether a Remediation block appears at all — were unexercised, so a rung that
// printed FAIL while reporting healthy would have shipped green. Only an
// unsupervised daemon is a fault; an unknown is a probe that could not tell, and
// restarting on it would act on nothing observed.
func TestReportDaemonSupervisionFlagsFollowTheVerdict(t *testing.T) {
	const recordedPID = 31337

	for _, tc := range []struct {
		name          string
		st            daemon.Status
		wantUnhealthy bool
		wantLine      string
	}{
		{
			name:          "detached daemon is a fault",
			st:            daemon.Status{Installed: true, Running: false},
			wantUnhealthy: true,
			wantLine:      "FAIL",
		},
		{
			name:          "service manager owning a different PID is a fault",
			st:            daemon.Status{Installed: true, Running: true, PID: recordedPID + 1},
			wantUnhealthy: true,
			wantLine:      "FAIL",
		},
		{
			name:          "an unparseable service view is not a fault",
			st:            daemon.Status{Installed: true, Running: true, PID: 0},
			wantUnhealthy: false,
			wantLine:      "unknown",
		},
		{
			name:          "a service-manager-owned daemon is healthy",
			st:            daemon.Status{Installed: true, Running: true, PID: recordedPID},
			wantUnhealthy: false,
			wantLine:      "ok",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			stubDaemonDoctorProcess(t, nil)
			st := tc.st
			previous := daemonGetStatus
			daemonGetStatus = func() (*daemon.Status, error) { return &st, nil }
			t.Cleanup(func() { daemonGetStatus = previous })

			var out bytes.Buffer
			unhealthy, restartRemediation := reportDaemonSupervision(&out, daemonstate.Metadata{PID: recordedPID}, nil)

			if !strings.Contains(out.String(), tc.wantLine) {
				t.Fatalf("output = %q, want %q", out.String(), tc.wantLine)
			}
			if unhealthy != tc.wantUnhealthy {
				t.Fatalf("unhealthy = %t, want %t (output %q)", unhealthy, tc.wantUnhealthy, out.String())
			}
			// The two travel together by construction: a supervision fault is
			// exactly the state a restart under the service manager fixes.
			if restartRemediation != unhealthy {
				t.Fatalf("restartRemediation = %t but unhealthy = %t; the two must not drift apart", restartRemediation, unhealthy)
			}
		})
	}
}
