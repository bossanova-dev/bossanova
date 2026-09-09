package main

import (
	"fmt"
	"os"

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
	// job running while its PID could not be established: a systemd MainPID
	// read that failed or would not parse. Ownership is unproven, not refuted.
	//
	// BOS-1218 changed WHICH substrate reaches this. It used to also stand for
	// a launchd job that was merely registered — the shape that produced the
	// `supervision: unknown (the service manager reports running but did not
	// report a PID; …)` line this very rung is remembered for — and that shape
	// now has its own honest verdict, because Status.Running requires a PID on
	// launchd. Since Running there implies a PID, and a PID implies
	// Status.PIDKnown, the rung is UNREACHABLE on launchd by construction. It
	// stays live for systemd, where `systemctl --user is-active` can report
	// active while the separate MainPID read fails.
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
	// daemonSupervisionReasonWatchdogOwned is the healthy case on the
	// `unattended` substrate, and it is a DIFFERENT shape of ownership rather
	// than a synonym for the one above.
	//
	// There, launchd owns the root-owned watchdog LaunchDaemon and the watchdog
	// spawns bossd through `launchctl asuser`, so bossd is a GRANDCHILD of
	// launchd and not a launchd job at all. Flattening the two would tell an
	// operator that launchd owns a PID launchd has never heard of, and would
	// send anyone debugging it to `launchctl print gui/<uid>/…` for a job that
	// deliberately does not exist there (BOS-1204 R7).
	daemonSupervisionReasonWatchdogOwned
	// daemonSupervisionReasonWatchdogNotLoaded is a watchdog installed on disk
	// whose `system`-domain job launchd does not have loaded. Nothing will
	// restart the recorded daemon, so this is a genuine fault and not an
	// unknown: reporting it as unknown would be BOS-1183's concealment failure
	// reproduced on the new substrate.
	daemonSupervisionReasonWatchdogNotLoaded
	// daemonSupervisionReasonWatchdogUnreadable is the fail-closed rung: the
	// watchdog is installed and its job could not be read. Ownership is
	// unproven, not refuted (BOS-1204 R6).
	daemonSupervisionReasonWatchdogUnreadable
	// daemonSupervisionReasonWatchdogInsecure is an installed watchdog one of
	// whose paths is not safe for a root-owned job.
	//
	// It resolves to UNKNOWN rather than to a fault, and that is deliberate:
	// the fault is real and is already reported, in full, by the
	// `configured supervision substrate` line that
	// describeUnattendedSupervisionMode renders on both surfaces. Reporting it
	// again here would print two failures for one fact — the same
	// duplicate-failure shape reportDaemonSupervision's `!st.Installed` rung
	// already exists to avoid.
	daemonSupervisionReasonWatchdogInsecure
)

// daemonLoadSupervisionMode is the seam BOTH reporting surfaces read the
// configured substrate through.
//
// It is a shared package var rather than a parameter on each renderer for the
// reason daemonSupervisionOfLiveRecord itself is shared: what previously let
// `boss daemon status` and `boss daemon doctor` disagree about one host was
// each surface deriving its own inputs. Reading one seam makes a future caller
// unable to hand the two renderers different answers, and lets a test state one
// host once.
var daemonLoadSupervisionMode = daemon.LoadSupervisionModeStatus

// daemonObserveWatchdogOwnership is the seam for the `system`-domain ownership
// probe. Separate from the seam above because it must stay LAZY: it shells out
// to launchctl, and LoadSupervisionModeStatus is called from newClient on every
// single boss command.
var daemonObserveWatchdogOwnership = daemon.ObserveWatchdogOwnership

// daemonUnattendedSubstrateConfigured reports whether this host's supervision
// substrate is the unattended watchdog rather than the per-user LaunchAgent.
//
// Err is checked as well as Mode because a rejected configuration resolves to
// NO mode (daemon.LoadSupervisionModeStatus), and a reporting surface must
// never route by a value the resolver refused.
func daemonUnattendedSubstrateConfigured(supervision daemon.SupervisionModeStatus) bool {
	return supervision.Err == nil && supervision.Mode == daemon.SupervisionModeUnattended
}

// daemonUnattendedSubstrateOwnsVerdict reports whether the substrate branch of
// daemonSupervisionOfLiveRecord will answer the ownership question for this
// host, rather than the LaunchAgent delegation below it.
//
// Absent deliberately does NOT qualify. A host that selected the unattended
// mode and never completed the root install has no supervision at all, and the
// existing delegation already reports exactly that from the LaunchAgent status
// — with today's wording, which requirement 5 wants left alone wherever it is
// still true.
func daemonUnattendedSubstrateOwnsVerdict(supervision daemon.SupervisionModeStatus) bool {
	if !daemonUnattendedSubstrateConfigured(supervision) {
		return false
	}
	switch supervision.Unattended.State {
	case daemon.UnattendedInstallPresent, daemon.UnattendedInstallInsecure:
		return true
	default:
		return false
	}
}

// daemonSupervisionInputs gathers the two substrate facts both renderers decide
// from, probing launchctl only on the host whose verdict depends on it.
//
// The laziness is requirement 5 made mechanical rather than aspirational: on
// the default substrate — and on an unattended host with nothing installed —
// this performs no launchctl invocation at all, so default reporting cannot
// change because there is nothing new in its path to change it.
func daemonSupervisionInputs() (daemon.SupervisionModeStatus, daemon.WatchdogOwnership) {
	supervision := daemonLoadSupervisionMode()
	if daemonServiceProbingDisabled() ||
		!daemonUnattendedSubstrateConfigured(supervision) ||
		supervision.Unattended.State != daemon.UnattendedInstallPresent {
		return supervision, daemon.WatchdogOwnership{}
	}
	return supervision, daemonObserveWatchdogOwnership()
}

// daemonServiceProbingDisabled reports whether the operator (or a test harness,
// or CI) has switched service-manager probing off.
//
// It lives beside the gather rather than only in the two renderers because
// BOS-1204 AC9 is "short-circuit ahead of every substrate read, on BOTH
// surfaces", and a guard written once per renderer is a guard one new caller
// can forget. `boss daemon doctor` checked it before gathering and
// `boss daemon status` did not, so the two surfaces already disagreed about
// whether the launchctl probe runs. Guarding the shared gather makes them
// unable to.
func daemonServiceProbingDisabled() bool {
	return os.Getenv("BOSS_DAEMON_SKIP_LAUNCHCTL") != ""
}

// watchdogOwnershipReason renders why an ownership probe could not answer,
// falling back to a neutral sentence when the observation carries none.
//
// The fallback is the unprobed zero value: a caller that reached a reporting
// rung without ever probing has observed nothing, and an empty parenthesis
// would read as a truncated message rather than as the absence of a probe.
func watchdogOwnershipReason(ownership daemon.WatchdogOwnership) string {
	if ownership.Reason != "" {
		return ownership.Reason
	}
	return "the service manager was not probed"
}

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
//   - "service manager reports running, its PID could not be established" is
//     answered above the delegation. ClassifyServingMode calls that standalone,
//     which is the right answer for RESTART (a live recorded daemon is what it
//     must preserve) and the wrong one for a REPORT: a service manager that
//     could not name the PID of a job it says is running is a tooling failure,
//     and turning it into an unsupervised verdict would print a fault nobody
//     observed. BOS-1218 re-keyed that guard on Status.PIDKnown rather than on
//     PID == 0; see the guard for what changed under it.
//
// BOS-1204 adds `supervision` and `ownership`: which substrate this host is
// configured for, and — on a host whose substrate is the root-owned watchdog —
// whether launchd has that job loaded. They open a branch ABOVE the delegation
// rather than a second ladder beside it, so every other mode falls through to
// today's decision untouched and requirement 5 is mechanical rather than
// asserted. `st` keeps meaning exactly what it always meant, the per-user
// LaunchAgent substrate; see the branch body for why repointing it instead
// would have manufactured a louder false fault.
func daemonSupervisionOfLiveRecord(
	st *daemon.Status,
	recordedPID int,
	supervision daemon.SupervisionModeStatus,
	ownership daemon.WatchdogOwnership,
) (daemonSupervisionVerdict, daemonSupervisionReason) {
	if st == nil || recordedPID <= 0 {
		return daemonSupervisionUnknown, daemonSupervisionReasonIndeterminate
	}
	// The substrate branch sits ABOVE both the no-service-PID guard and the
	// delegation, because on this substrate `st` describes the wrong thing
	// entirely: it is the per-user LaunchAgent's status, and the unattended
	// install deliberately supersedes that LaunchAgent. Reading a stale agent's
	// PID mismatch on such a host would answer a question nobody asked.
	//
	// It deliberately does NOT repoint st.PID at the watchdog and fall through.
	// Under this substrate launchd owns the WATCHDOG and the watchdog owns
	// bossd — two different processes by design — so ClassifyServingMode would
	// compare a watchdog PID against a bossd PID, find them different, and
	// print `FAIL … two daemons, or a stale state record`. That is a new and
	// louder false fault than the one BOS-1204 is removing.
	if daemonUnattendedSubstrateOwnsVerdict(supervision) {
		if supervision.Unattended.State == daemon.UnattendedInstallInsecure {
			return daemonSupervisionUnknown, daemonSupervisionReasonWatchdogInsecure
		}
		switch ownership.State {
		case daemon.WatchdogOwnershipLoaded:
			// KNOWN BOUND, recorded rather than papered over: a loaded watchdog
			// proves launchd owns THE WATCHDOG. It does not prove the recorded
			// PID is the watchdog's child. A bossd started detached by the
			// `boss daemon start` fallback, or a leftover LaunchAgent's, can
			// hold the recorded PID on a host whose watchdog is also loaded,
			// and this rung reports it supervised.
			//
			// Closing it needs a parentage observation, and there is no cheap
			// honest one here: bossd is reached through `launchctl asuser` and
			// `sudo -u`, so its PPID is an intermediate that has usually
			// already exited, leaving the process reparented. A PPID compare
			// would therefore report "not the watchdog's child" for the
			// ordinary healthy host — a false fault, which is the exact class
			// BOS-1204 exists to remove. The plan's `Risks / unknowns` note
			// scopes out LIVENESS ("the watchdog being loaded is not proof
			// bossd is healthy"); this is the narrower ownership question and
			// is NOT covered by it.
			return daemonSupervisionSupervised, daemonSupervisionReasonWatchdogOwned
		case daemon.WatchdogOwnershipNotLoaded:
			return daemonSupervisionUnsupervised, daemonSupervisionReasonWatchdogNotLoaded
		default:
			// Unknown AND the unprobed zero value. A caller that reached here
			// without probing has observed nothing, and nothing is not health.
			return daemonSupervisionUnknown, daemonSupervisionReasonWatchdogUnreadable
		}
	}

	// BOS-1218 R5a: `PID == 0` is now QUALIFIED by whether the observation
	// settled, in the daemonbin.Inspect shape this ticket mirrors — a value is
	// read as authoritative only when its known flag says it was established.
	// A zero PID with PIDKnown set is the service manager answering "this job
	// owns no process", which is an observation with its own honest verdict
	// and not this rung's business; a zero PID without it is "could not tell",
	// which is exactly what this rung reports.
	//
	// The unqualified condition used to be reached by a launchd job that was
	// merely registered — the shape this rung is remembered for — and Running
	// now requires a PID there, so it is unreachable on launchd. Leaving the
	// key unqualified would have left a rung that still READS as the launchd
	// diagnostic while being unreachable on launchd, which is the worst
	// outcome available: this is the line an operator uses to recognise a
	// recurrence. See the constant for which substrate still reaches it.
	//
	// A launchd probe that could not be READ reports Running = false and lands
	// in the delegation below instead, where a live recorded daemon renders
	// `detached`. That is today's behaviour for a not-loaded job and is
	// deliberately unchanged here: an unreadable answer and a genuinely
	// not-loaded job are indistinguishable in Status by design (both are
	// PIDKnown = false), and separating them is the doctor ticket this one
	// unblocks, not this one.
	if st.Running && st.PID == 0 && !st.PIDKnown {
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
