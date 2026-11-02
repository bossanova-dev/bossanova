package daemon

import (
	"fmt"
	"io"
	"time"

	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/spf13/cobra"
)

// daemonStartupGrace is how long a supervised bossd that has not bound its
// socket still reads as starting rather than wedged (BOS-2168).
//
// It is twice daemon.LifecycleStartupTimeout because `boss daemon start` itself
// waits one full startup budget before refusing: a grace equal to that budget
// would misclassify the exact moment start gives up (the incident daemon was
// about 57s old then). Twice the budget covers a slow but healthy boot while
// still reporting a minute-plus wedge as the failure it is.
const daemonStartupGrace = 2 * daemon.LifecycleStartupTimeout

// daemonStartupObservation says whether a supervised bossd is still starting:
// alive, owned by the service manager or the watchdog, recently started, and
// not yet serving its socket. `boss daemon start`, `boss daemon doctor` and
// `boss daemon restart` all decide from it, so the three commands cannot
// disagree about one host.
type daemonStartupObservation struct {
	Starting bool
	PID      int
	// Age is daemonNow minus the state record's StartedAt.
	Age time.Duration
	// ProbesRecorded is corroboration only, never a gate: bossd records its
	// startup probe results before binding the socket, and only on darwin.
	ProbesRecorded bool
}

// observeDaemonStartup classifies the daemon behind socketPath. Any rung it
// cannot establish leaves Starting false, so every caller keeps its existing
// failure wording wherever "starting" is not proven.
func (deps *Group) observeDaemonStartup(socketPath string) daemonStartupObservation {
	if deps.daemonSocketReachable(socketPath) {
		return daemonStartupObservation{}
	}
	return deps.observeUnservedDaemonStartup()
}

// observeUnservedDaemonStartup is observeDaemonStartup for a caller that has
// already established the socket does not answer, so it is not probed twice.
//
// The age check runs before the service-manager probe on purpose: a state
// record older than the grace (or one left by a dead daemon long ago) settles
// the answer without shelling out to launchctl or systemctl.
func (deps *Group) observeUnservedDaemonStartup() daemonStartupObservation {
	metadata, err := deps.daemonMetadataForDoctor()
	if err != nil || metadata.PID <= 0 {
		return daemonStartupObservation{}
	}
	obs := daemonStartupObservation{
		PID:            metadata.PID,
		Age:            deps.daemonNow().Sub(metadata.StartedAt),
		ProbesRecorded: metadata.TCCProbeCompleted || len(metadata.TCCProbeResults) > 0,
	}
	if obs.Age < 0 || obs.Age > daemonStartupGrace {
		return obs
	}
	supervision, ownership := deps.daemonSupervisionInputs()
	supervised := deps.observeDaemonSupervision(metadata, nil, supervision, ownership)
	obs.Starting = supervised.Stop == daemonSupervisionStopNone && supervised.Verdict == daemonSupervisionSupervised
	return obs
}

// describe is the one phrase the three commands use for a starting daemon.
func (o daemonStartupObservation) describe() string {
	return fmt.Sprintf("bossd PID %d is still starting (%s since it started; it has not bound the daemon socket yet)",
		o.PID, o.Age.Round(time.Second))
}

// daemonSocketVerdict is what doctor's socket check established.
//
// NotServing keeps its exact historical meaning — false for "reachable",
// "unknown" AND "starting" — because the foreground directive and remedy are
// gated on it. Known/Reachable are the separate pair the candidate-cause report
// needs: "the socket answers" is reassurance, "we could not resolve a profile
// to ask" is not.
type daemonSocketVerdict struct {
	Known      bool
	Reachable  bool
	NotServing bool
	// Starting is the BOS-2168 still-starting state, so the remediation
	// ladder says wait rather than falling through to restart.
	Starting bool
}

// reportDaemonSocket is doctor's socket verdict, lifted out of runDaemonDoctor
// so the starting case did not grow that function's complexity budget.
//
// The socket, not the process table, is the definition of "serving". The two
// flags are pointers for the reason reportDaemonLaunchAgentPlist gives: they
// are accumulators runDaemonDoctor keeps building after this returns.
func (deps *Group) reportDaemonSocket(out io.Writer, profile daemonProfile, profileErr error, unhealthyNonAuth, startRemediation *bool) daemonSocketVerdict {
	switch {
	case profileErr != nil:
		_, _ = fmt.Fprintf(out, "daemon socket: unknown (%v)\n", profileErr)
		return daemonSocketVerdict{}
	case deps.daemonSocketReachable(profile.SocketPath):
		_, _ = fmt.Fprintf(out, "daemon socket: %s — reachable\n", profile.SocketPath)
		return daemonSocketVerdict{Known: true, Reachable: true}
	}
	if obs := deps.observeUnservedDaemonStartup(); obs.Starting {
		// BOS-2168: the service manager owns a live bossd that has not bound
		// the socket yet. Reporting FAIL and "run 'boss daemon start'" here
		// sent the incident operator to restart a healthy, booting daemon.
		probes := ""
		if !obs.ProbesRecorded {
			probes = "; startup probe results not yet recorded"
		}
		_, _ = fmt.Fprintf(out, "daemon socket: %s — starting: %s%s\n", profile.SocketPath, obs.describe(), probes)
		_, _ = fmt.Fprintln(out, "  wait for bossd to finish starting, then re-run 'boss daemon doctor'")
		return daemonSocketVerdict{Known: true, Starting: true}
	}
	// Serving is what the daemon is FOR, so a socket known not to answer is a
	// failure verdict rather than a note; without it the startup directive
	// would print recovery instructions on a run that exits 0.
	*unhealthyNonAuth = true
	*startRemediation = true
	_, _ = fmt.Fprintf(out, "FAIL daemon socket: %s — not reachable, so bossd is not serving\n", profile.SocketPath)
	return daemonSocketVerdict{Known: true, NotServing: true}
}

// noteRestartDuringStartup tells the operator, on stderr so `--json` stdout
// stays one envelope, that the restart they asked for discards a boot in
// progress. Restart still proceeds: refusing would be a behaviour change.
func (deps *Group) noteRestartDuringStartup(cmd *cobra.Command, socketPath string) {
	if obs := deps.observeDaemonStartup(socketPath); obs.Starting {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Note: %s; restarting stops that startup and begins a new one.\n", obs.describe())
	}
}

// daemonStartRefusal words `boss daemon start`'s refusal when the installed
// service did not serve the socket in time. A supervised daemon that is still
// starting is not that condition, so its error deliberately does not wrap
// daemon.ErrServiceInstalledNotServing: a caller matching it would re-advise
// restart.
func (deps *Group) daemonStartRefusal(socketPath string, err error) error {
	if obs := deps.observeDaemonStartup(socketPath); obs.Starting {
		return fmt.Errorf("start daemon: %s. The service manager owns it; wait for it to finish starting, then run 'boss daemon status'", obs.describe())
	}
	return fmt.Errorf("start daemon refused: %w. "+
		"Run 'boss daemon doctor' to see why the service manager did not serve it, "+
		"'boss daemon restart' to re-bootstrap the service, "+
		"or 'boss daemon start --detached' to start an unsupervised daemon deliberately", err)
}
