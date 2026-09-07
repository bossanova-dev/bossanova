package main

import (
	"fmt"

	"github.com/recurser/boss/internal/daemon"
)

// daemonUnsupervisedConsequences is the one sentence every surface that
// reports lost supervision uses: `boss daemon status`, `boss daemon start` and
// `boss daemon doctor`.
//
// The two consequences travel together because they have one cause — a bossd
// outside the GUI login session — so naming only one of them understates the
// damage. `boss daemon start` used to name only the reboot half, which reads as
// a deferred, tomorrow problem, so the operator moves on while gh is already
// falling back to unauthenticated requests today. Three surfaces named three
// different consequence sets for one fact; this constant is why they cannot
// again.
const daemonUnsupervisedConsequences = "it was started detached, so on macOS it cannot reach the login keychain and gh silently falls back to unauthenticated requests, and it has no KeepAlive restart and will not survive reboot"

// daemonSupervisionVerdict is the ownership answer that `boss daemon status`
// (daemonSupervisionLine) and `boss daemon doctor` (reportDaemonSupervision)
// both render, and that `boss daemon restart` decides its strategy from.
//
// BOS-1183 landed alongside BOS-1181, which introduced
// daemon.ClassifyServingMode for the restart path. Before this type the same
// machine could get three answers to one question: restart classified from
// ServingFacts, doctor walked its own switch ladder, and status walked a
// hand-copied mirror of doctor's whose agreement was asserted only in a
// comment. The stale-record row proved they really did diverge — a supervised
// daemon with a stale state record read supervised to restart, unknown to
// doctor and unsupervised to status. There is now one decision, below.
type daemonSupervisionVerdict int

const (
	// daemonSupervisionUnknown means ownership could not be established. It is
	// the zero value on purpose: every path that fails to observe something
	// must fall here rather than certify either health or fault.
	daemonSupervisionUnknown daemonSupervisionVerdict = iota
	// daemonSupervisionSupervised means the platform service manager owns the
	// recorded daemon.
	daemonSupervisionSupervised
	// daemonSupervisionUnsupervised means the recorded daemon is live and the
	// service manager does not own it.
	daemonSupervisionUnsupervised
)

// daemonSupervisionReason names WHICH observation produced the verdict, so the
// two surfaces can word one shared decision in their own voice without
// re-deriving it. Rendering is presentation; deciding is not.
type daemonSupervisionReason int

const (
	// daemonSupervisionReasonIndeterminate covers a nil status, a non-positive
	// recorded PID, and any state the classifier cannot attribute.
	daemonSupervisionReasonIndeterminate daemonSupervisionReason = iota
	// daemonSupervisionReasonNoServicePID is the service manager reporting the
	// job running while naming no PID: launchctl output that will not parse, or
	// a systemd MainPID read that failed. Ownership is unproven, not refuted.
	daemonSupervisionReasonNoServicePID
	// daemonSupervisionReasonDetached is a live recorded daemon the service
	// manager does not know about at all.
	daemonSupervisionReasonDetached
	// daemonSupervisionReasonForeignPID is a live recorded daemon while the
	// service manager owns a DIFFERENT PID: two daemons, or a state record that
	// outlived its process.
	daemonSupervisionReasonForeignPID
	// daemonSupervisionReasonManagerOwned is the healthy case.
	daemonSupervisionReasonManagerOwned
)

// daemonSupervisionOfLiveRecord decides whether the platform service manager
// owns the daemon recorded for this profile. The caller must already have
// established that recordedPID names a LIVE process; liveness is a probe, and
// keeping it out of here is what leaves this function pure and its whole input
// matrix testable on either platform.
//
// The decision itself is delegated to daemon.ClassifyServingMode so that this
// reporting surface and BOS-1181's restart strategy cannot disagree about one
// host. Two things are settled here rather than there, and both are deliberate:
//
//   - StandaloneSupported is passed true unconditionally. That field is
//     BOS-1181's BEHAVIOURAL scope boundary — it exists so the restart paths
//     keep their previous branch on Linux — and servingmode_other.go says in
//     as many words that it is "not a claim that Linux has no direct-spawn
//     fallback", because platformEnsureRunning in systemd.go does spawn bossd
//     directly. Reporting is cross-platform on this branch, so suppressing the
//     verdict on Linux would hide a true fact rather than scope a behaviour.
//   - "service manager reports running, names no PID" is answered above the
//     delegation. ClassifyServingMode calls that standalone, which is the right
//     answer for RESTART (a live recorded daemon is what it must preserve) and
//     the wrong one for a REPORT: unparseable launchctl output is a tooling
//     failure, and turning it into an unsupervised verdict would print a fault
//     nobody observed.
func daemonSupervisionOfLiveRecord(st *daemon.Status, recordedPID int) (daemonSupervisionVerdict, daemonSupervisionReason) {
	if st == nil || recordedPID <= 0 {
		return daemonSupervisionUnknown, daemonSupervisionReasonIndeterminate
	}
	if st.Running && st.PID == 0 {
		return daemonSupervisionUnknown, daemonSupervisionReasonNoServicePID
	}

	switch daemon.ClassifyServingMode(daemon.ServingFacts{
		Installed:           st.Installed,
		Running:             st.Running,
		ServiceManagerPID:   st.PID,
		StandalonePID:       recordedPID,
		StandaloneAlive:     true,
		StandaloneSupported: true,
	}) {
	case daemon.ServingModeSupervised:
		return daemonSupervisionSupervised, daemonSupervisionReasonManagerOwned
	case daemon.ServingModeStandalone:
		if st.Running {
			return daemonSupervisionUnsupervised, daemonSupervisionReasonForeignPID
		}
		return daemonSupervisionUnsupervised, daemonSupervisionReasonDetached
	default:
		// Unreachable from a real platformGetStatus, which only ever sets PID
		// alongside Running (launchd.go, systemd.go). Left as an explicit
		// unknown rather than an assertion: this surface must never certify
		// ownership from a status shape it does not recognise.
		return daemonSupervisionUnknown, daemonSupervisionReasonIndeterminate
	}
}

// describeDaemonSupervisionMode is the ONE supervision-MODE decision that
// `boss daemon status` and `boss daemon doctor` both render, in the same shape
// daemonSupervisionOfLiveRecord above establishes for the ownership question:
// deciding happens once, rendering happens twice.
//
// Two different questions live next to each other on both surfaces, so the
// LABELS do the separating rather than a comment asking the reader to remember
// it:
//
//   - "supervision:" (daemonSupervisionLine / reportDaemonSupervision) is an
//     OBSERVATION — does the service manager own the daemon running right now?
//   - "configured supervision substrate:" (this function) is a CONFIGURATION.
//     It has an answer on a host where nothing is installed and nothing is
//     running, and it can disagree with the observation without either being
//     wrong.
//
// unhealthy is reported only for a configuration this host would REFUSE to act
// on, never for the platform simply not having a choice to make. A doctor run
// on Linux must not exit non-zero over a macOS-only key that changes nothing
// there — that is precisely how a diagnostic teaches operators to ignore it.
func describeDaemonSupervisionMode(st daemon.SupervisionModeStatus) (description string, unhealthy bool) {
	if !st.Configurable {
		// Checked before Err, and deliberately. Off macOS every non-default
		// value is refused, so reading Err first would turn an inert key into a
		// failing doctor run on a platform where the key does nothing at all.
		base := "not applicable on this platform (bossd is supervised by its systemd user unit, which with 'loginctl enable-linger' already survives its user logging out)"
		if st.Configured != "" {
			return fmt.Sprintf("%s — the configured daemon_supervision_mode %q is inert here", base, st.Configured), false
		}
		return base, false
	}
	if st.Err != nil {
		// The error text already names the rejected value and what to do about
		// it; restating either here would be a second copy free to drift.
		return fmt.Sprintf("misconfigured — %v", st.Err), true
	}
	// A settings file that exists and could not be read resolves to the default
	// substrate (daemon.LoadSupervisionModeStatus), which is the right call for
	// the install path and the wrong thing to REPORT unqualified: an operator
	// with a corrupt settings.json would read the affirmative default line as
	// confirmation that whatever they wrote is in effect. Name the fallback.
	// It is not unhealthy — nothing about the substrate is wrong — so the
	// doctor exit code is unchanged.
	if st.SettingsErr != nil {
		return fmt.Sprintf(
			"%s (the per-user LaunchAgent in the gui/<uid> Aqua domain — the default) — settings could not be read (%v), so no configured value was consulted",
			daemon.SupervisionModeLaunchAgent, st.SettingsErr), false
	}
	switch st.Mode {
	case daemon.SupervisionModeUnattended:
		// BOS-1184 U3. This mode's line carries an OBSERVATION as well as the
		// configuration, because on its own the configuration is misleading
		// here in a way it never is for the default: `launch-agent` selected
		// and not installed is a host with a plain LaunchAgent to install,
		// while `unattended` selected and not installed is a host with NO
		// supervision at all and a root-privileged step still outstanding. The
		// observation is gathered once by daemon.LoadSupervisionModeStatus and
		// rendered here, so this stays the single decision both surfaces read
		// rather than a second ladder.
		return describeUnattendedSupervisionMode(st)
	case daemon.SupervisionModeLaunchAgent:
		// Deliberately short, and deliberately silent about WHEN this substrate
		// fails. An earlier draft explained the backgrounded-domain limitation
		// inline and thereby printed "fast user switching" on every doctor run
		// — including a bossd crash loop, whose whole diagnosis is that the
		// launchd domain is NOT the problem
		// (TestRunDaemonDoctorReportsCrashLoopingJobDistinctlyFromNeverSpawned
		// caught it). A line printed unconditionally must state the fact and
		// nothing else; the limitation belongs in
		// docs/ops/daemon-supervision-modes.md, where it cannot be mistaken for
		// a diagnosis of the host in front of you.
		return fmt.Sprintf("%s (the per-user LaunchAgent in the gui/<uid> Aqua domain — the default)", st.Mode), false
	default:
		// Unreachable: recognisedSupervisionModes has a case above for every
		// value it holds. Left as a neutral rendering rather than an assertion,
		// because a reporting surface must never refuse to name a mode the
		// resolver accepted.
		return string(st.Mode), false
	}
}

// describeUnattendedSupervisionMode renders the unattended substrate's real
// state on this host.
//
// Two of the three states are unhealthy, and for different reasons:
//
//   - INSECURE is the sharp one. The watchdog is installed and something it
//     depends on — its plist, its binary, or a directory above either — is
//     writable by a non-root user, which is a root-execution primitive sitting
//     on the machine right now. daemon.ErrUnattendedInsecurePath already names
//     the offending path and what is wrong with it, so the reason is carried
//     through verbatim rather than summarised.
//   - ABSENT means the operator selected this substrate and never completed the
//     root-privileged install, so nothing is supervising bossd. Reporting that
//     as healthy would be the BOS-1183 concealment failure in a new place.
//
// PRESENT is healthy and says so plainly, naming the plist so an operator can
// go and read the job.
func describeUnattendedSupervisionMode(st daemon.SupervisionModeStatus) (string, bool) {
	base := fmt.Sprintf("%s (the root-owned %s LaunchDaemon, which spawns bossd through 'launchctl asuser' and so does not depend on the daemon's user owning the foreground console)",
		st.Mode, daemon.WatchdogLabel)
	switch st.Unattended.State {
	case daemon.UnattendedInstallPresent:
		return fmt.Sprintf("%s — installed at %s", base, st.Unattended.PlistPath), false
	case daemon.UnattendedInstallInsecure:
		return fmt.Sprintf("%s — INSECURE: %v", base, st.Unattended.Err), true
	case daemon.UnattendedInstallAbsent:
		return fmt.Sprintf("%s — configured but NOT installed, so nothing is supervising bossd; run `%s`",
			base, daemon.WatchdogInstallCommand), true
	default:
		// The observation is only gathered when the mode resolves to
		// unattended, so this is the shape of a hand-built status rather than
		// anything a host can produce. Name the mode, claim nothing.
		return base, false
	}
}
