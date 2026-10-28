package daemon

import (
	"fmt"
	"os"

	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/spf13/cobra"
)

// corroborateDetachedVerdict implements BOS-2009's reporting-only check.
// Lifecycle decisions retain their original service-manager observation.
func corroborateDetachedVerdict(verdict daemonSupervisionVerdict, reason daemonSupervisionReason, recordedPID int, job daemon.LaunchAgentJob) (daemonSupervisionVerdict, daemonSupervisionReason) {
	if reason != daemonSupervisionReasonDetached {
		return verdict, reason
	}
	switch job.State {
	case daemon.LaunchAgentJobNotApplicable, daemon.LaunchAgentJobNotLoaded, daemon.LaunchAgentJobLoadedNoPID:
		return verdict, reason
	case daemon.LaunchAgentJobUnknown:
		return daemonSupervisionUnknown, daemonSupervisionReasonServiceViewDisputed
	case daemon.LaunchAgentJobRunning:
		if job.PID == recordedPID {
			return daemonSupervisionSupervised, daemonSupervisionReasonManagerOwned
		}
	}
	return daemonSupervisionUnknown, daemonSupervisionReasonServiceViewDisputed
}

func (deps *Group) decideLiveRecordSupervision(st *daemon.Status, recordedPID int, supervision daemon.SupervisionModeStatus, ownership daemon.WatchdogOwnership) (daemonSupervisionVerdict, daemonSupervisionReason, daemon.LaunchAgentJob) {
	verdict, reason := daemonSupervisionOfLiveRecord(st, recordedPID, supervision, ownership)
	var job daemon.LaunchAgentJob
	if reason == daemonSupervisionReasonDetached {
		job = deps.daemonObserveLaunchAgentJob()
		verdict, reason = corroborateDetachedVerdict(verdict, reason, recordedPID, job)
	}
	return verdict, reason, job
}

func disputedServiceViewText(recordedPID int, job daemon.LaunchAgentJob) string {
	answer := "could not answer: " + job.Reason
	if job.State == daemon.LaunchAgentJobRunning {
		answer = fmt.Sprintf("reported PID %d", job.PID)
	}
	return fmt.Sprintf("launchctl list did not report PID %d running, and launchctl print %s %s", recordedPID, job.Target, answer)
}

// daemonUnsupervisedWarningText is the single line every `boss` subcommand may
// emit when the daemon serving this profile is live and the service manager
// does not own it. The PID is appended per invocation.
//
// Deliberately short: daemonUnsupervisedConsequences is the full explanation
// and belongs in doctor, not on every command.
const daemonUnsupervisedWarningText = "boss: the service manager does not own the bossd serving this profile " +
	"(details: boss daemon doctor)"

// daemonSupervisionWarningRemedyCommands are the commands that already report
// supervision in full, or that are themselves its remedy. `boss daemon start`
// is deliberately absent: with a detached daemon already serving it prints
// "Daemon is already running.", and the operator should learn what kind.
var daemonSupervisionWarningRemedyCommands = []string{
	"boss daemon doctor",
	"boss daemon status",
	"boss daemon stop",
	"boss daemon restart",
	"boss daemon install",
	"boss daemon uninstall",
}

// warnIfDaemonUnsupervised writes at most one stderr line when the bossd
// serving this profile is live and the service manager does not own it — the
// third per-invocation warning beside warnIfDaemonBinaryStale and
// warnIfBossBinaryBehindCheckout, for the same "detection exists, nobody sees
// it" failure: on 2026-10-11 a detached bossd served the socket for a day while
// only `boss daemon doctor` said so.
//
// It reads the observation doctor renders (daemonLocalSupervisionObservation)
// rather than re-deriving one, and only the unsupervised verdict warns. Every
// unknown stays silent: a per-invocation line for a probe that could not tell
// trains operators to ignore it, and doctor is where unknowns are explained.
//
// It can never fail a command: nothing is written to stdout — a --json
// consumer would be corrupted — and the exit code is untouched.
//
// PersistentPreRunE runs before newClient's auto-start, so an invocation that
// itself spawns a detached daemon stays silent and the next one reports it.
func (deps *Group) warnIfDaemonUnsupervised(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	if os.Getenv(skipDaemonSupervisionWarningEnv) != "" {
		return
	}
	// Under BOSS_DAEMON_SKIP_LAUNCHCTL the status shape is byte-identical to a
	// detached daemon; see reportDaemonSupervision.
	if daemonServiceProbingDisabled() {
		return
	}
	// --remote, --host and BOSS_SOCKET all name a daemon other than this
	// profile's, which is exactly when newClient skips local startup.
	if !localDaemonTarget(cmd) {
		return
	}
	if commandPathIsOneOf(cmd.CommandPath(), daemonSupervisionWarningRemedyCommands) {
		return
	}
	observation := deps.daemonLocalSupervisionObservation()
	if !observation.unsupervised() {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s [PID %d]\n", daemonUnsupervisedWarningText, observation.RecordedPID)
}
