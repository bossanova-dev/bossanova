package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/bossalib/config"
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
		// supervision and ownership describe the substrate. The zero
		// SupervisionModeStatus is not usable here — it names no mode at all —
		// so every row states one, and the default-substrate rows state the
		// launch-agent one explicitly.
		supervision daemon.SupervisionModeStatus
		ownership   daemon.WatchdogOwnership
		// doctorDiverges is set only on the one row where the two surfaces are
		// documented to differ: doctor answers "unknown (no service is
		// installed)" because the not-installed check owns that fact and its
		// remedy, while status has already printed "Daemon is not installed."
		// and labels the live recorded daemon unsupervised.
		doctorDiverges bool
		wantStatus     string
		wantDoctor     string
		// wantShared is the reason's distinguishing VOCABULARY, required on
		// both surfaces. The verdict token alone was the whole pin, so the two
		// hand-maintained reason->wording switches — one in handlers.go, one in
		// daemon_doctor.go, grown from four arms to eight by BOS-1204 — could
		// describe one host two different ways with every gate green. AC7's
		// grandchild wording is the case that matters: it is the entire point
		// of the new reason, and nothing asserted that doctor said it too.
		wantShared []string
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
		// BOS-1204 AC4: the new substrate reason is covered by this pin rather
		// than exempted from it. Each of these rows has NO installed
		// LaunchAgent, which is the ordinary shape of an unattended host -- the
		// install supersedes the agent -- and is precisely the shape that made
		// doctor's not-installed rung swallow the verdict before the fix.
		{
			name:        "unattended: the watchdog job is loaded",
			st:          daemon.Status{Installed: false, Running: false},
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipLoaded),
			wantStatus:  "supervised",
			wantDoctor:  "supervised",
			wantShared:  []string{daemon.WatchdogLabel, "grandchild of launchd"},
		},
		{
			name:        "unattended: the watchdog is installed but its job is not loaded",
			st:          daemon.Status{Installed: false, Running: false},
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipNotLoaded),
			wantStatus:  "unsupervised",
			wantDoctor:  "unsupervised",
			wantShared:  []string{"watchdog is installed at", "launchd does not have", "loaded"},
		},
		{
			name:        "unattended: the watchdog job could not be read",
			st:          daemon.Status{Installed: false, Running: false},
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipUnknown),
			wantStatus:  "unknown",
			wantDoctor:  "unknown",
			wantShared:  []string{"watchdog is installed", "could not be established"},
		},
		{
			name:        "unattended: the watchdog is installed and insecure",
			st:          daemon.Status{Installed: false, Running: false},
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallInsecure),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipLoaded),
			wantStatus:  "unknown",
			wantDoctor:  "unknown",
			wantShared:  []string{"not safe for a root-owned job"},
		},
		{
			name:           "unattended but never installed keeps the launch-agent divergence",
			st:             daemon.Status{Installed: false, Running: false},
			supervision:    unattendedSupervisionStatus(daemon.UnattendedInstallAbsent),
			doctorDiverges: true,
			wantStatus:     "unsupervised",
			wantDoctor:     "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			stubDaemonDoctorProcess(t, nil)
			supervision := tc.supervision
			if supervision.Mode == "" && supervision.Err == nil {
				supervision = launchAgentSupervisionStatus()
			}
			stubDaemonSupervisionInputs(t, supervision, tc.ownership)
			st := tc.st

			statusLine := daemonSupervisionLine(&st, recordedPID, supervision, tc.ownership)
			gotStatus := supervisionVerdictToken(t, statusLine, "supervision: ")

			previous := daemonGetStatus
			daemonGetStatus = func() (*daemon.Status, error) { return &st, nil }
			t.Cleanup(func() { daemonGetStatus = previous })
			var out bytes.Buffer
			reportDaemonSupervisionGathered(&out, daemonstate.Metadata{PID: recordedPID}, nil)
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
			// The verdict token agreeing is not the two surfaces agreeing: the
			// sentence is what an operator reads, and it is written twice.
			for _, want := range tc.wantShared {
				if !strings.Contains(statusLine, want) {
					t.Fatalf("status wording is missing %q:\n%s", want, statusLine)
				}
				if !strings.Contains(out.String(), want) {
					t.Fatalf("doctor wording is missing %q:\n%s", want, out.String())
				}
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
			got, _ := daemonSupervisionOfLiveRecord(&st, recordedPID, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
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
	verdict, reason := daemonSupervisionOfLiveRecord(&st, 77, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
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
			unhealthy, remediation := reportDaemonSupervisionGathered(&out, daemonstate.Metadata{PID: recordedPID}, nil)

			if !strings.Contains(out.String(), tc.wantLine) {
				t.Fatalf("output = %q, want %q", out.String(), tc.wantLine)
			}
			if unhealthy != tc.wantUnhealthy {
				t.Fatalf("unhealthy = %t, want %t (output %q)", unhealthy, tc.wantUnhealthy, out.String())
			}
			// On THIS substrate the two travel together by construction: every
			// LaunchAgent-shaped supervision fault is exactly the state a
			// restart under the service manager fixes. The unattended
			// substrate's watchdog-not-loaded rung is the one that does not,
			// and it is pinned separately.
			wantRemediation := daemonSupervisionRemediationNone
			if unhealthy {
				wantRemediation = daemonSupervisionRemediationRestart
			}
			if remediation != wantRemediation {
				t.Fatalf("remediation = %v, want %v (unhealthy = %t)", remediation, wantRemediation, unhealthy)
			}
		})
	}
}

// TestDescribeDaemonSupervisionModeMatrix walks the shared mode decision both
// `boss daemon status` and `boss daemon doctor` render.
//
// The rows that carry weight are the platform ones. On a host with no
// selectable substrate every non-default value is refused by the resolver, so a
// renderer that read Err before Configurable would make `boss daemon doctor`
// exit non-zero on Linux over a macOS-only key that changes nothing there.
func TestDescribeDaemonSupervisionModeMatrix(t *testing.T) {
	cases := []struct {
		name          string
		status        daemon.SupervisionModeStatus
		wantContains  []string
		wantUnhealthy bool
	}{
		{
			name:         "default mode on a configurable host",
			status:       daemon.SupervisionModeStatus{Mode: daemon.SupervisionModeLaunchAgent, Configurable: true},
			wantContains: []string{string(daemon.SupervisionModeLaunchAgent), "default"},
		},
		{
			name: "rejected mode on a configurable host is unhealthy",
			status: daemon.SupervisionModeStatus{
				Configured:   "asuser",
				Configurable: true,
				Err:          fmt.Errorf("%w: %q is not recognised", daemon.ErrUnknownSupervisionMode, "asuser"),
			},
			wantContains:  []string{"misconfigured", "asuser"},
			wantUnhealthy: true,
		},
		{
			// BOS-1184 U3. The configuration alone is not enough to report
			// here: `unattended` selected with nothing installed is a host
			// with NO supervision, and naming only the mode would read as
			// confirmation that it is in effect.
			name: "unattended installed is healthy and names the job",
			status: daemon.SupervisionModeStatus{
				Configured:   "unattended",
				Mode:         daemon.SupervisionModeUnattended,
				Configurable: true,
				Unattended: daemon.UnattendedInstall{
					State:     daemon.UnattendedInstallPresent,
					PlistPath: "/Library/LaunchDaemons/com.bossanova.bossd-watchdog.plist",
				},
			},
			wantContains: []string{"unattended", daemon.WatchdogLabel, "installed at", "/Library/LaunchDaemons/com.bossanova.bossd-watchdog.plist"},
		},
		{
			name: "unattended configured but not installed is unhealthy and names the install command",
			status: daemon.SupervisionModeStatus{
				Configured:   "unattended",
				Mode:         daemon.SupervisionModeUnattended,
				Configurable: true,
				Unattended:   daemon.UnattendedInstall{State: daemon.UnattendedInstallAbsent},
			},
			wantContains:  []string{"unattended", "NOT installed", daemon.WatchdogInstallCommand},
			wantUnhealthy: true,
		},
		{
			name: "unattended installed on an insecure path is unhealthy and carries the reason",
			status: daemon.SupervisionModeStatus{
				Configured:   "unattended",
				Mode:         daemon.SupervisionModeUnattended,
				Configurable: true,
				Unattended: daemon.UnattendedInstall{
					State: daemon.UnattendedInstallInsecure,
					Err:   fmt.Errorf("%w: the watchdog bossd binary /usr/local/libexec/bossanova/bossd is owned by uid 501, not uid 0", daemon.ErrUnattendedInsecurePath),
				},
			},
			wantContains:  []string{"unattended", "INSECURE", "/usr/local/libexec/bossanova/bossd", "uid 501"},
			wantUnhealthy: true,
		},
		{
			name: "unrecognised value on a configurable host is unhealthy",
			status: daemon.SupervisionModeStatus{
				Configured:   "system-daemon",
				Configurable: true,
				Err:          fmt.Errorf("%w: %q is not recognised", daemon.ErrUnknownSupervisionMode, "system-daemon"),
			},
			wantContains:  []string{"misconfigured", "system-daemon"},
			wantUnhealthy: true,
		},
		{
			name:         "unconfigurable host with no key says the concept does not apply",
			status:       daemon.SupervisionModeStatus{Mode: daemon.SupervisionModeLaunchAgent},
			wantContains: []string{"not applicable"},
		},
		{
			// The key is inert here, so it must be reported as inert rather
			// than as a fault — and the operator still gets told the value they
			// can see in settings.json does nothing on this host.
			name: "unconfigurable host reports a configured value as inert, not as a fault",
			status: daemon.SupervisionModeStatus{
				Configured: "unattended",
				Err:        fmt.Errorf("%w: macOS only", daemon.ErrSupervisionModeUnsupportedPlatform),
			},
			wantContains: []string{"not applicable", "inert", "unattended"},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			description, unhealthy := describeDaemonSupervisionMode(tt.status)
			if unhealthy != tt.wantUnhealthy {
				t.Fatalf("unhealthy = %t, want %t (description: %s)", unhealthy, tt.wantUnhealthy, description)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(description, want) {
					t.Errorf("description %q does not contain %q", description, want)
				}
			}
		})
	}
}

// TestDescribeDaemonSupervisionModeNeverFailsAnUnconfigurablePlatform is the
// Linux-untouched pin for the REPORTING half: no settings value, valid or not,
// may make a doctor run unhealthy on a platform that has no substrate to
// select. Pairs with the daemon package's
// TestSupervisionModeNeverReachesTheSystemdPath, which pins the same property
// for the resolver.
func TestDescribeDaemonSupervisionModeNeverFailsAnUnconfigurablePlatform(t *testing.T) {
	statuses := []daemon.SupervisionModeStatus{
		{Mode: daemon.SupervisionModeLaunchAgent},
		{Configured: "launch-agent", Mode: daemon.SupervisionModeLaunchAgent},
		{Configured: "unattended", Err: daemon.ErrSupervisionModeUnsupportedPlatform},
		{Configured: "system-daemon", Err: daemon.ErrUnknownSupervisionMode},
	}
	for _, status := range statuses {
		description, unhealthy := describeDaemonSupervisionMode(status)
		if unhealthy {
			t.Errorf("configured %q made an unconfigurable platform unhealthy: %s", status.Configured, description)
		}
	}
}

// TestDescribeDaemonSupervisionModeNamesAnUnreadableSettingsFile pins that a
// settings file which could not be read is REPORTED as the fallback it is,
// rather than as an affirmative configuration.
//
// The resolver deliberately still yields the default there (R5 — an unreadable
// settings.json must not newly break `boss daemon install`), so the fix is not
// in the substrate decision but in the wording: an operator with a corrupt
// settings.json must not read this line as confirmation that the mode they
// wrote is in effect. It stays healthy, because nothing about the substrate is
// wrong and a doctor that failed here would change default-path exit codes.
func TestDescribeDaemonSupervisionModeNamesAnUnreadableSettingsFile(t *testing.T) {
	status := daemon.SupervisionModeStatus{
		Mode:         daemon.SupervisionModeLaunchAgent,
		Configurable: true,
		SettingsErr:  errors.New("settings.json: unexpected end of JSON input"),
	}
	description, unhealthy := describeDaemonSupervisionMode(status)
	if unhealthy {
		t.Errorf("an unreadable settings file must not make the doctor unhealthy: %s", description)
	}
	for _, want := range []string{"settings could not be read", "unexpected end of JSON input"} {
		if !strings.Contains(description, want) {
			t.Errorf("description %q does not name %q", description, want)
		}
	}
}

// TestDaemonSupervisionModeSurfacesCannotDisagree pins the BOS-1183 design this
// reporting follows: one decision, two voices. Both surfaces call
// describeDaemonSupervisionMode, so doctor's line must be status's line plus
// doctor's own FAIL marker — never a separately-worded second ladder that can
// drift into naming a different mode.
func TestDaemonSupervisionModeSurfacesCannotDisagree(t *testing.T) {
	var out bytes.Buffer
	doctorUnhealthy, doctorRemediation := reportDaemonSupervisionMode(&out, daemon.LoadSupervisionModeStatus())
	doctorLine := strings.TrimSpace(out.String())

	statusDescription, statusUnhealthy := describeDaemonSupervisionMode(daemon.LoadSupervisionModeStatus())

	if doctorUnhealthy != statusUnhealthy {
		t.Fatalf("doctor unhealthy = %t but the shared decision says %t", doctorUnhealthy, statusUnhealthy)
	}
	if !strings.Contains(doctorLine, statusDescription) {
		t.Fatalf("doctor line %q does not carry the shared description %q", doctorLine, statusDescription)
	}
	wantPrefix := "daemon supervision substrate: "
	if doctorUnhealthy {
		wantPrefix += "FAIL "
	}
	if !strings.HasPrefix(doctorLine, wantPrefix) {
		t.Fatalf("doctor line %q does not start with %q", doctorLine, wantPrefix)
	}
	// The remedy is derived from the SAME gathered status the description is,
	// so a host cannot be described in one state and remediated for another.
	if want := daemonSupervisionModeRemediation(daemon.LoadSupervisionModeStatus()); doctorRemediation != want {
		t.Fatalf("doctor remediation = %q, want %q", doctorRemediation, want)
	}
}

// TestDaemonSupervisionModeRemediationMatchesTheFault pins the remedy split
// BOS-1184 U3 introduced. A settings edit is the right answer for a value the
// resolver refused and the WRONG one for the two unattended fault states: an
// operator whose watchdog is simply not installed has no bad key to fix, and
// one whose watchdog sits on a user-writable path needs the path repaired.
func TestDaemonSupervisionModeRemediationMatchesTheFault(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       daemon.SupervisionModeStatus
		wantContains []string
		wantAbsent   []string
	}{
		{
			name: "a refused settings value sends the operator to settings.json",
			status: daemon.SupervisionModeStatus{
				Configured: "system-daemon", Configurable: true,
				Err: daemon.ErrUnknownSupervisionMode,
			},
			wantContains: []string{"daemon_supervision_mode", "settings.json"},
		},
		{
			name: "an uninstalled watchdog sends the operator to the root install",
			status: daemon.SupervisionModeStatus{
				Mode: daemon.SupervisionModeUnattended, Configurable: true,
				Unattended: daemon.UnattendedInstall{State: daemon.UnattendedInstallAbsent},
			},
			wantContains: []string{daemon.WatchdogInstallCommand},
			wantAbsent:   []string{"settings.json"},
		},
		{
			name: "an insecure watchdog sends the operator to repair the path",
			status: daemon.SupervisionModeStatus{
				Mode: daemon.SupervisionModeUnattended, Configurable: true,
				Unattended: daemon.UnattendedInstall{State: daemon.UnattendedInstallInsecure, Err: daemon.ErrUnattendedInsecurePath},
			},
			wantContains: []string{"root-owned", daemon.WatchdogUninstallCommand, daemon.WatchdogInstallCommand},
			wantAbsent:   []string{"settings.json"},
		},
		{
			name: "a healthy unattended host still falls back to the settings remedy",
			status: daemon.SupervisionModeStatus{
				Mode: daemon.SupervisionModeUnattended, Configurable: true,
				Unattended: daemon.UnattendedInstall{State: daemon.UnattendedInstallPresent},
			},
			wantContains: []string{"settings.json"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := daemonSupervisionModeRemediation(tt.status)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("remedy %q does not name %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("remedy %q misdirects the operator to %q", got, absent)
				}
			}
		})
	}
}

// writeSupervisionModeSettings points BOSS_SETTINGS_PATH at a settings file
// carrying the given daemon_supervision_mode value, so the tests below exercise
// the real chain — settings file, resolver, renderer — rather than a hand-built
// status struct.
func writeSupervisionModeSettings(t *testing.T, configured string) {
	t.Helper()
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings := config.DefaultSettings()
	settings.DaemonSupervisionMode = configured
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
}

// TestReportDaemonSupervisionModeReadsRealSettings walks the whole chain from a
// settings.json on disk to doctor's line. The unit tests above build a
// SupervisionModeStatus by hand, which cannot catch the key being read from the
// wrong field or not read at all.
func TestReportDaemonSupervisionModeReadsRealSettings(t *testing.T) {
	t.Run("absent key", func(t *testing.T) {
		writeSupervisionModeSettings(t, "")
		var out bytes.Buffer
		unhealthy, _ := reportDaemonSupervisionMode(&out, daemon.LoadSupervisionModeStatus())
		if unhealthy {
			t.Fatalf("the default mode must never be unhealthy: %s", out.String())
		}
		if !strings.Contains(out.String(), "daemon supervision substrate: ") {
			t.Fatalf("output %q does not carry the mode line", out.String())
		}
	})

	t.Run("refused value", func(t *testing.T) {
		// A value no platform recognises, so the verdict depends only on the
		// platform posture and not on what happens to be installed on the
		// machine running the test.
		writeSupervisionModeSettings(t, "system-daemon")
		var out bytes.Buffer
		unhealthy, remediation := reportDaemonSupervisionMode(&out, daemon.LoadSupervisionModeStatus())
		line := out.String()
		if !strings.Contains(line, "system-daemon") {
			t.Fatalf("output %q does not name the configured value", line)
		}
		// macOS is the only platform where this key selects anything, so it is
		// the only one where configuring it can be a fault. Everywhere else the
		// key is inert and reporting it as a failure would be a false FAIL.
		wantUnhealthy := runtime.GOOS == "darwin"
		if unhealthy != wantUnhealthy {
			t.Fatalf("unhealthy = %t on %s, want %t (line: %s)", unhealthy, runtime.GOOS, wantUnhealthy, line)
		}
		if !strings.Contains(remediation, "settings.json") {
			t.Fatalf("remediation %q for a bad settings value does not send the operator to settings.json", remediation)
		}
	})

	t.Run("unattended names the root-owned watchdog", func(t *testing.T) {
		// Deliberately asserts the WORDING and not the health verdict: whether
		// this mode is healthy depends on whether a root-owned LaunchDaemon is
		// installed on the machine running the test, which a unit test must
		// neither create nor assume. The state matrix is covered by
		// TestDescribeDaemonSupervisionModeMatrix, which builds the status by
		// hand.
		if runtime.GOOS != "darwin" {
			t.Skip("the unattended substrate is selectable on macOS only")
		}
		writeSupervisionModeSettings(t, "unattended")
		var out bytes.Buffer
		reportDaemonSupervisionMode(&out, daemon.LoadSupervisionModeStatus())
		line := out.String()
		for _, want := range []string{"unattended", daemon.WatchdogLabel} {
			if !strings.Contains(line, want) {
				t.Fatalf("output %q does not name %q", line, want)
			}
		}
	})
}

// TestRunDaemonStatusReportsSupervisionMode is the R4 pin for `boss daemon
// status`: the configured substrate must appear on the surface an operator
// actually reads, not merely be computable by a function it could forget to
// call.
func TestRunDaemonStatusReportsSupervisionMode(t *testing.T) {
	restoreDaemonCommandStubs(t)
	// writeDaemonStatusProfile already writes a hermetic settings.json with the
	// key absent and points BOSS_SETTINGS_PATH at it. Layering a second
	// settings file on top would repoint the profile away from its temp app
	// data dir and make the run read the developer's real daemon state.
	writeDaemonStatusProfile(t)
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID, ServicePath: "/tmp/service"}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	want, _ := describeDaemonSupervisionMode(daemon.LoadSupervisionModeStatus())
	if !strings.Contains(out, "configured supervision substrate: "+want) {
		t.Fatalf("status output does not report the supervision mode %q:\n%s", want, out)
	}
}

// TestRunDaemonDoctorFailsOnMisconfiguredSupervisionMode pins the doctor
// wiring end to end, including the part most easily got wrong: the remedy.
//
// A settings-file mistake is not fixed by restarting the daemon, so this run
// must NOT print the restart line that the unhealthyNonAuth ladder ends in.
// Asserting its absence is the whole point — folding the mode verdict into that
// flag would still produce a red doctor run and still look correct.
func TestRunDaemonDoctorFailsOnMisconfiguredSupervisionMode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the supervision mode is selectable on macOS only")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	// Overwrites the default settings the fixture just wrote, and keeps
	// BOSS_SETTINGS_PATH pointing at HOME rather than at a second temp dir, so
	// nothing else the fixture configured moves.
	settingsPath := filepath.Join(home, "settings.json")
	settings := config.DefaultSettings()
	settings.DaemonSupervisionMode = "system-daemon"
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	// Short-circuits the ownership check against the developer's real launchd
	// domain, so this test's only unhealthy input is the settings value.
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy; output:\n%s", err, output.String())
	}
	out := output.String()
	if !strings.Contains(out, "daemon supervision substrate: FAIL") {
		t.Errorf("doctor output does not FAIL the supervision mode:\n%s", out)
	}
	if !strings.Contains(out, "system-daemon") {
		t.Errorf("doctor output does not name the rejected value:\n%s", out)
	}
	if !strings.Contains(out, strings.TrimSpace(daemonSupervisionModeSettingsRemediation)) {
		t.Errorf("doctor output does not carry the settings remediation:\n%s", out)
	}
	if strings.Contains(out, "run 'boss daemon restart'") {
		t.Errorf("a settings-file mistake must not be remediated with a restart:\n%s", out)
	}
}

// launchAgentSupervisionStatus is the default-substrate host: the per-user
// LaunchAgent, nothing observed about any watchdog. It is what every
// pre-BOS-1204 row of these tables describes, spelled once so a row that means
// "unchanged" cannot drift into meaning something else.
func launchAgentSupervisionStatus() daemon.SupervisionModeStatus {
	return daemon.SupervisionModeStatus{
		Mode:         daemon.SupervisionModeLaunchAgent,
		Configurable: true,
	}
}

// unattendedSupervisionStatus is a host configured for the unattended
// substrate, with the watchdog observed in the given install state.
func unattendedSupervisionStatus(state daemon.UnattendedInstallState) daemon.SupervisionModeStatus {
	status := daemon.SupervisionModeStatus{
		Configured:   string(daemon.SupervisionModeUnattended),
		Mode:         daemon.SupervisionModeUnattended,
		Configurable: true,
		Unattended: daemon.UnattendedInstall{
			State:      state,
			PlistPath:  "/Library/LaunchDaemons/" + daemon.WatchdogLabel + ".plist",
			BinaryPath: "/usr/local/libexec/bossanova/bossd",
		},
	}
	if state == daemon.UnattendedInstallInsecure {
		status.Unattended.Err = errors.New("/usr/local is writable by group")
	}
	return status
}

// watchdogOwnership is a probe result in the given state.
func watchdogOwnership(state daemon.WatchdogOwnershipState) daemon.WatchdogOwnership {
	observation := daemon.WatchdogOwnership{State: state, Target: daemon.WatchdogTarget()}
	if state != daemon.WatchdogOwnershipLoaded {
		observation.Reason = "stated by the test"
	}
	return observation
}

// TestDaemonSupervisionOfLiveRecordAcrossSubstrates is BOS-1204's decision
// matrix: every UnattendedInstallState crossed with every ownership
// observation, PLUS every pre-existing launch-agent row re-asserted unchanged.
//
// The launch-agent half is not padding. Requirement 5 — default reporting is
// unchanged — is the requirement most easily satisfied in prose and broken in
// code, and the only mechanical form of it is asserting the old rows through
// the new signature.
func TestDaemonSupervisionOfLiveRecordAcrossSubstrates(t *testing.T) {
	const recordedPID = 4242

	installed := daemon.Status{Installed: true, Running: true, PID: recordedPID}
	detached := daemon.Status{Installed: false, Running: false}

	for _, tc := range []struct {
		name        string
		st          daemon.Status
		supervision daemon.SupervisionModeStatus
		ownership   daemon.WatchdogOwnership
		wantVerdict daemonSupervisionVerdict
		wantReason  daemonSupervisionReason
	}{
		{
			name:        "unattended, installed and loaded, is supervised by the watchdog",
			st:          detached,
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipLoaded),
			wantVerdict: daemonSupervisionSupervised,
			wantReason:  daemonSupervisionReasonWatchdogOwned,
		},
		{
			name:        "unattended, installed but the job is not loaded, is unsupervised",
			st:          detached,
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipNotLoaded),
			wantVerdict: daemonSupervisionUnsupervised,
			wantReason:  daemonSupervisionReasonWatchdogNotLoaded,
		},
		{
			name:        "unattended, installed but the probe could not be read, is unknown",
			st:          detached,
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipUnknown),
			wantVerdict: daemonSupervisionUnknown,
			wantReason:  daemonSupervisionReasonWatchdogUnreadable,
		},
		{
			name:        "unattended, installed but never probed, is unknown",
			st:          detached,
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
			ownership:   daemon.WatchdogOwnership{},
			wantVerdict: daemonSupervisionUnknown,
			wantReason:  daemonSupervisionReasonWatchdogUnreadable,
		},
		{
			name:        "unattended and insecure defers to the substrate line",
			st:          detached,
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallInsecure),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipLoaded),
			wantVerdict: daemonSupervisionUnknown,
			wantReason:  daemonSupervisionReasonWatchdogInsecure,
		},
		{
			name:        "unattended and absent falls through to the LaunchAgent delegation",
			st:          detached,
			supervision: unattendedSupervisionStatus(daemon.UnattendedInstallAbsent),
			ownership:   daemon.WatchdogOwnership{},
			wantVerdict: daemonSupervisionUnsupervised,
			wantReason:  daemonSupervisionReasonDetached,
		},
		{
			name: "a rejected supervision mode never routes to the substrate branch",
			st:   detached,
			supervision: daemon.SupervisionModeStatus{
				Configured:   "unattnded",
				Configurable: true,
				Err:          errors.New("unrecognised daemon supervision mode"),
			},
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipLoaded),
			wantVerdict: daemonSupervisionUnsupervised,
			wantReason:  daemonSupervisionReasonDetached,
		},
		{
			name:        "launch-agent: the service manager owns the recorded daemon",
			st:          installed,
			supervision: launchAgentSupervisionStatus(),
			wantVerdict: daemonSupervisionSupervised,
			wantReason:  daemonSupervisionReasonManagerOwned,
		},
		{
			name:        "launch-agent: the service manager does not know the job",
			st:          daemon.Status{Installed: true, Running: false},
			supervision: launchAgentSupervisionStatus(),
			wantVerdict: daemonSupervisionUnsupervised,
			wantReason:  daemonSupervisionReasonDetached,
		},
		{
			name:        "launch-agent: the service manager owns a different PID",
			st:          daemon.Status{Installed: true, Running: true, PID: recordedPID + 1},
			supervision: launchAgentSupervisionStatus(),
			wantVerdict: daemonSupervisionUnsupervised,
			wantReason:  daemonSupervisionReasonForeignPID,
		},
		{
			name:        "launch-agent: the service manager reports running with no PID",
			st:          daemon.Status{Installed: true, Running: true, PID: 0},
			supervision: launchAgentSupervisionStatus(),
			wantVerdict: daemonSupervisionUnknown,
			wantReason:  daemonSupervisionReasonNoServicePID,
		},
		{
			name:        "launch-agent: no service installed",
			st:          detached,
			supervision: launchAgentSupervisionStatus(),
			wantVerdict: daemonSupervisionUnsupervised,
			wantReason:  daemonSupervisionReasonDetached,
		},
		{
			name:        "a watchdog observation is ignored on the default substrate",
			st:          installed,
			supervision: launchAgentSupervisionStatus(),
			ownership:   watchdogOwnership(daemon.WatchdogOwnershipNotLoaded),
			wantVerdict: daemonSupervisionSupervised,
			wantReason:  daemonSupervisionReasonManagerOwned,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			verdict, reason := daemonSupervisionOfLiveRecord(&st, recordedPID, tc.supervision, tc.ownership)
			if verdict != tc.wantVerdict {
				t.Fatalf("verdict = %v, want %v (reason %v)", verdict, tc.wantVerdict, reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %v, want %v", reason, tc.wantReason)
			}
		})
	}
}

// stubDaemonSupervisionInputs pins BOTH substrate seams for a test, so a run's
// verdict cannot depend on the settings.json or the launchd domain of whatever
// machine is running the suite.
//
// It stubs them together on purpose: they are the two halves of one host
// description, and a test that pinned only the configuration would silently
// shell out to the real launchctl on any row that reaches the ownership probe.
func stubDaemonSupervisionInputs(t *testing.T, supervision daemon.SupervisionModeStatus, ownership daemon.WatchdogOwnership) {
	t.Helper()
	previousMode := daemonLoadSupervisionMode
	previousOwnership := daemonObserveWatchdogOwnership
	daemonLoadSupervisionMode = func() daemon.SupervisionModeStatus { return supervision }
	daemonObserveWatchdogOwnership = func() daemon.WatchdogOwnership { return ownership }
	t.Cleanup(func() {
		daemonLoadSupervisionMode = previousMode
		daemonObserveWatchdogOwnership = previousOwnership
	})
}

// TestDaemonSupervisionInputsProbeOnlyWhereTheVerdictNeedsIt is BOS-1204
// requirement 5 in its most mechanical form: on the default substrate nothing
// new runs at all.
//
// The probe shells out to launchctl and LoadSupervisionModeStatus is reached
// from newClient on every single boss command, so an eagerly-gathered
// observation would be a launchctl invocation added to the steady-state path of
// the whole CLI. The test fails the probe loudly rather than counting calls: a
// count assertion passes at zero for a build that never wired the probe up.
func TestDaemonSupervisionInputsProbeOnlyWhereTheVerdictNeedsIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		supervision daemon.SupervisionModeStatus
		wantProbe   bool
	}{
		{name: "launch-agent", supervision: launchAgentSupervisionStatus()},
		{name: "unattended and absent", supervision: unattendedSupervisionStatus(daemon.UnattendedInstallAbsent)},
		{name: "unattended and insecure", supervision: unattendedSupervisionStatus(daemon.UnattendedInstallInsecure)},
		{name: "unattended and present", supervision: unattendedSupervisionStatus(daemon.UnattendedInstallPresent), wantProbe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probed := false
			previousMode := daemonLoadSupervisionMode
			previousOwnership := daemonObserveWatchdogOwnership
			daemonLoadSupervisionMode = func() daemon.SupervisionModeStatus { return tc.supervision }
			daemonObserveWatchdogOwnership = func() daemon.WatchdogOwnership {
				probed = true
				return watchdogOwnership(daemon.WatchdogOwnershipLoaded)
			}
			t.Cleanup(func() {
				daemonLoadSupervisionMode = previousMode
				daemonObserveWatchdogOwnership = previousOwnership
			})

			_, ownership := daemonSupervisionInputs()
			if probed != tc.wantProbe {
				t.Fatalf("watchdog probe invoked = %t, want %t", probed, tc.wantProbe)
			}
			if !tc.wantProbe && ownership.State != daemon.WatchdogOwnershipNotObserved {
				t.Fatalf("unprobed ownership state = %v, want WatchdogOwnershipNotObserved", ownership.State)
			}
		})
	}
}

// TestRunDaemonStatusHeaderFollowsTheUnattendedSubstrate is BOS-1204 AC1 on the
// surface an operator actually reads.
//
// The host it describes is the whole bug: the unattended install SUPERSEDES the
// per-user LaunchAgent, so `daemon.Status.Installed` is false on a machine that
// is working perfectly, and the pre-fix ladder printed "Daemon is not
// installed." above a reachable socket and told the operator to run an install
// they had already done.
//
// Both halves of the assertion are load-bearing. The absence of the
// uninstalled/unsupervised claims is the defect; the presence of "Daemon is
// running." is what stops a build that simply printed nothing from passing the
// absence half trivially.
func TestRunDaemonStatusHeaderFollowsTheUnattendedSubstrate(t *testing.T) {
	restoreDaemonCommandStubs(t)
	writeDaemonStatusProfile(t)
	stubDaemonDoctorProcess(t, nil)
	stubDaemonSupervisionInputs(t,
		unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
		watchdogOwnership(daemon.WatchdogOwnershipLoaded))
	// The LaunchAgent is absent, which is the ORDINARY state of an unattended
	// host rather than a fault: platformInstallUnattended removes it.
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})

	for _, forbidden := range []string{
		"Daemon is not installed.",
		"supervision: unsupervised",
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("status reported %q on a healthy unattended host:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "Daemon is running.") {
		t.Fatalf("status did not report the daemon running:\n%s", out)
	}
	if !strings.Contains(out, "supervision: supervised") {
		t.Fatalf("status did not report the daemon supervised:\n%s", out)
	}
	// AC7: the wording must distinguish watchdog ownership from a LaunchAgent's
	// direct ownership. An operator told only "supervised" would go looking for
	// gui/<uid>/com.bossanova.bossd, which is absent here by design, and read
	// its absence as the fault.
	for _, want := range []string{daemon.WatchdogLabel, "grandchild of launchd"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status supervision wording does not mention %q:\n%s", want, out)
		}
	}
}

// TestDoctorRemediatesAnUnloadedWatchdogWithTheInstallCommand pins WHICH remedy
// the watchdog-not-loaded rung prints.
//
// The rung's fault is a root-owned LaunchDaemon that is present on disk and
// that launchd has not bootstrapped. `boss daemon restart` operates the per-user
// LaunchAgent this substrate deliberately supersedes, so it can run to
// completion, report success, and leave nothing supervising bossd — a remedy
// that certifies a repair it did not perform. The remedy that works needs root,
// and the doctor already knows its sentence.
func TestDoctorRemediatesAnUnloadedWatchdogWithTheInstallCommand(t *testing.T) {
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	stubDaemonDoctorProcess(t, nil)
	stubDaemonSupervisionInputs(t,
		unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
		watchdogOwnership(daemon.WatchdogOwnershipNotLoaded))
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}

	var out bytes.Buffer
	unhealthy, remediation := reportDaemonSupervisionGathered(&out, daemonstate.Metadata{PID: 4711}, nil)

	if !unhealthy {
		t.Fatalf("an unloaded watchdog was not reported as a fault:\n%s", out.String())
	}
	if remediation != daemonSupervisionRemediationInstallWatchdog {
		t.Fatalf("remediation = %v, want the watchdog install remedy; a per-user restart cannot load a root-owned LaunchDaemon\n%s",
			remediation, out.String())
	}
	if remediation == daemonSupervisionRemediationRestart {
		t.Fatalf("doctor routed an unloaded root-owned watchdog to 'boss daemon restart'")
	}
	printed := daemonWatchdogNotLoadedRemediation()
	if !strings.Contains(printed, daemon.WatchdogInstallCommand) {
		t.Fatalf("the watchdog remedy does not name %q: %q", daemon.WatchdogInstallCommand, printed)
	}
	if strings.Contains(printed, "run 'boss daemon restart'") {
		t.Fatalf("the watchdog remedy still instructs a per-user restart: %q", printed)
	}
}

// TestRunDaemonStatusShortCircuitsAheadOfTheOwnershipProbe is the status-surface
// half of BOS-1204 AC9, which reads "on both surfaces". Doctor had a test and
// status did not, and the two behaved differently: `boss daemon doctor`
// checked BOSS_DAEMON_SKIP_LAUNCHCTL before gathering, while `boss daemon
// status` called daemonSupervisionInputs unconditionally and so drove the
// launchctl seam on a host that had asked for no service-manager probing.
//
// The assertion is the seam itself rather than the rendered text, because the
// text short-circuits in daemonSupervisionLine either way — which is exactly
// how the probe stayed invisible while it ran.
func TestRunDaemonStatusShortCircuitsAheadOfTheOwnershipProbe(t *testing.T) {
	restoreDaemonCommandStubs(t)
	writeDaemonStatusProfile(t)
	stubDaemonDoctorProcess(t, nil)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	previousMode := daemonLoadSupervisionMode
	previousOwnership := daemonObserveWatchdogOwnership
	daemonLoadSupervisionMode = func() daemon.SupervisionModeStatus {
		return unattendedSupervisionStatus(daemon.UnattendedInstallPresent)
	}
	daemonObserveWatchdogOwnership = func() daemon.WatchdogOwnership {
		t.Errorf("the watchdog ownership probe ran under BOSS_DAEMON_SKIP_LAUNCHCTL")
		return daemon.WatchdogOwnership{}
	}
	t.Cleanup(func() {
		daemonLoadSupervisionMode = previousMode
		daemonObserveWatchdogOwnership = previousOwnership
	})
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})

	if !strings.Contains(out, "supervision: unknown (service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL)") {
		t.Fatalf("status did not short-circuit the supervision line:\n%s", out)
	}
}

// TestDaemonSupervisionLineWordsWatchdogOwnershipDistinctly pins AC7 at the
// renderer, so the distinction survives a change to the status header.
//
// The negative half matters as much as the positive: "the service manager owns
// PID N" is the LaunchAgent sentence, and reusing it here would flatten two
// genuinely different supervision shapes into one claim that is false about the
// launchd job table.
func TestDaemonSupervisionLineWordsWatchdogOwnershipDistinctly(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	stubDaemonDoctorProcess(t, nil)
	const recordedPID = 918
	st := daemon.Status{Installed: false, Running: false}

	line := daemonSupervisionLine(&st, recordedPID,
		unattendedSupervisionStatus(daemon.UnattendedInstallPresent),
		watchdogOwnership(daemon.WatchdogOwnershipLoaded))

	if !strings.HasPrefix(line, "supervision: supervised") {
		t.Fatalf("line = %q, want a supervised verdict", line)
	}
	for _, want := range []string{daemon.WatchdogLabel, "launchctl asuser", "grandchild of launchd"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line = %q, want it to mention %q", line, want)
		}
	}
	if strings.Contains(line, "the service manager owns PID") {
		t.Fatalf("line = %q reuses the LaunchAgent sentence, flattening the two ownership shapes", line)
	}
}

// TestUnattendedSubstrateIsUnreachableOffMacOS is BOS-1204 AC10.
//
// supervisionModeAvailability refuses every non-default mode where no substrate
// is selectable, so a Linux host that literally writes
// `daemon_supervision_mode: unattended` into settings.json resolves to NO mode
// and an Err. Both predicates must refuse it: routing on the raw configured
// string would have made a macOS-only substrate decide a Linux verdict.
func TestUnattendedSubstrateIsUnreachableOffMacOS(t *testing.T) {
	linuxStatus := daemon.SupervisionModeStatus{
		Configured:   string(daemon.SupervisionModeUnattended),
		Configurable: false,
		Err:          daemon.ErrSupervisionModeUnsupportedPlatform,
	}
	if daemonUnattendedSubstrateConfigured(linuxStatus) {
		t.Fatal("a refused supervision mode was treated as the configured substrate")
	}
	if daemonUnattendedSubstrateOwnsVerdict(linuxStatus) {
		t.Fatal("a refused supervision mode was allowed to decide the ownership verdict")
	}

	// And the verdict itself is the pre-BOS-1204 one: a live recorded daemon
	// with no service installed reads unsupervised/detached, exactly as it did
	// before this branch existed.
	st := daemon.Status{Installed: false, Running: false}
	verdict, reason := daemonSupervisionOfLiveRecord(&st, 4242, linuxStatus,
		watchdogOwnership(daemon.WatchdogOwnershipLoaded))
	if verdict != daemonSupervisionUnsupervised || reason != daemonSupervisionReasonDetached {
		t.Fatalf("verdict/reason = %v/%v, want unsupervised/detached", verdict, reason)
	}
}

// reportDaemonSupervisionGathered composes the substrate gather with the
// renderer exactly as runDaemonDoctor does, so a test exercises the real
// composition rather than a hand-supplied pair of inputs the command would
// never produce together.
func reportDaemonSupervisionGathered(out io.Writer, metadata daemonstate.Metadata, metadataErr error) (bool, daemonSupervisionRemediation) {
	supervision, ownership := daemonSupervisionInputs()
	return reportDaemonSupervision(out, metadata, metadataErr, supervision, ownership)
}
