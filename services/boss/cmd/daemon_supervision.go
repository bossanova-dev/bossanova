package main

import (
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
