package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"

	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/kit/config"
)

func (deps *Group) restartReachableDaemonForSettingsReload(socketPath string) error {
	return restartReachableDaemonForSettingsReloadWith(
		socketPath,
		daemon.GetStatus,
		deps.restartTakesStandalonePath,
		daemon.Stop,
		daemon.EnsureRunning,
		deps.daemonProcesses.TerminateBossdProcesses,
		deps.waitForCurrentProfileBossdExit,
		waitForSocketGone,
	)
}

func restartReachableDaemonForSettingsReloadWith(
	socketPath string,
	getStatus func() (*daemon.Status, error),
	takesStandalonePath func(*daemon.Status) bool,
	stop func() error,
	ensureRunning func(string) error,
	terminateStandalone func() (int, error),
	waitStandaloneExit func() bool,
	waitSocketGone func(string) bool,
) error {
	st, err := getStatus()
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	// restartTakesStandalonePath is BOS-1181's R4: this helper and `boss daemon
	// restart` must not disagree about what counts as a standalone-served
	// daemon. The extra `!st.Running` term is this helper's alone and is
	// deliberate: an installed service the manager has not started has nothing
	// to stop(), so terminate-then-ensure is the correct sequence. It is
	// evaluated first so a not-running service short-circuits the probe. It widens
	// which statuses take the standalone path; it never narrows them, so it
	// cannot reintroduce the defect.
	// `st == nil` leads, because both named predicates are nil-safe and the
	// inline `!st.Running` term is not: without it this helper is the one
	// spelling of the condition that panics on a status a caller was allowed
	// to hand every other spelling.
	if st == nil || !st.Running || takesStandalonePath(st) {
		n, err := terminateStandalone()
		if err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 && socketPath != "" && !waitSocketGone(socketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		if n > 0 && waitStandaloneExit != nil && !waitStandaloneExit() {
			return fmt.Errorf("timed out waiting for standalone bossd to exit after %s", daemon.LifecycleShutdownTimeout)
		}
		if err := ensureRunning(socketPath); err != nil {
			if !st.Installed {
				return fmt.Errorf("restart standalone bossd failed: %w", err)
			}
			return fmt.Errorf("restart daemon failed: %w", err)
		}
		return nil
	}
	if err := stop(); err != nil {
		return fmt.Errorf("stop daemon failed: %w", err)
	}
	if socketPath != "" && !waitSocketGone(socketPath) {
		return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
	}
	if err := ensureRunning(socketPath); err != nil {
		return fmt.Errorf("restart daemon failed: %w", err)
	}
	return nil
}

// restartTakesStandalonePath is the whole strategy condition every restart path
// decides through, so they cannot disagree (BOS-1181 R4). A profile with no
// service file has never had a service-manager strategy available, and a
// standalone-served daemon must not be replaced by one; everything else
// restarts through the service manager.
func (deps *Group) restartTakesStandalonePath(st *daemon.Status) bool {
	return st == nil || !st.Installed || deps.daemonStandaloneServed(st)
}

// stopTakesStandalonePath is `boss daemon stop`'s counterpart to
// restartTakesStandalonePath, and it consults the same daemonStandaloneServed
// classifier so the two commands cannot drift into disagreeing about who is
// serving (BOS-1218 R3).
//
// It differs from the restart predicate by one term, `!st.Running`, exactly as
// restartReachableDaemonForSettingsReloadWith does and for the same reason: a
// service the manager has not started has nothing to bootout, so the standalone
// route is the only one that can signal anything. That term is also what keeps
// the "Daemon is already stopped." outcome reachable — without it, an installed
// service with nothing serving at all would be classified unserved and sent to
// bootout, which is neither what it used to do nor useful.
//
// The `daemonStandaloneServed` term is the part that is new. `!st.Running` on
// its own already fixes the reported host, because after BOS-1218 a registered
// job launchd never spawned reports Running = false on the FIRST invocation
// rather than the second. But "is the service manager's job running" is a
// weaker question than "who is serving": on a host where launchd genuinely owns
// a PID *and* a detached bossd holds this profile's socket, stop would still
// bootout the one that is not serving.
func (deps *Group) stopTakesStandalonePath(st *daemon.Status) bool {
	return st == nil || !st.Installed || !st.Running || deps.daemonStandaloneServed(st)
}

// observedServingFacts gathers what the serving-mode probe adjudicates: the
// service manager's own view of the job (from st) and the standalone daemon
// this profile recorded the last time bossd started.
func (deps *Group) observedServingFacts(st *daemon.Status, profile daemonProfile) daemon.ServingFacts {
	facts := daemon.ServingFacts{StandaloneSupported: daemon.StandaloneServingSupported()}
	if st != nil {
		facts.Installed = st.Installed
		facts.Running = st.Running
		facts.ServiceManagerPID = st.PID
	}
	metadata, err := daemonstate.Read(profile.AppDataDir)
	if err != nil || metadata.PID <= 0 || !recordServesProfileSocket(metadata, profile) {
		return facts
	}
	facts.StandalonePID = metadata.PID
	// metadataMatchesRunningProcess returns true with NO liveness probe when
	// the record names no executable. That permissive default is right for its
	// original kill-guard caller and inverted here: a legacy or partial record
	// carrying a stale PID would declare a standalone verdict and boot a
	// correctly supervised daemon out of the service manager — the mirror image
	// of BOS-1181. daemon_doctor.go treats the same record shape as
	// unverifiable ("names no running process"), so follow that precedent and
	// let the classifier fail closed.
	if metadata.ExecutablePath == "" {
		return facts
	}
	facts.StandaloneAlive = deps.daemonProcesses.MetadataMatchesRunningProcess(metadata)
	return facts
}

// recordServesProfileSocket reports whether a daemon-state record found under
// this profile's app data dir is bound to this profile's socket. A record
// written under the same app data dir but naming a DIFFERENT socket says
// nothing about what is serving here, so it must not produce a standalone
// verdict. An empty recorded socket predates the field being written and is
// treated as unverifiable-but-not-mismatched, which keeps BOS-1181's fix
// reachable for records older than it.
func recordServesProfileSocket(metadata daemonstate.Metadata, profile daemonProfile) bool {
	if metadata.SocketPath == "" || profile.SocketPath == "" {
		return true
	}
	return filepath.Clean(metadata.SocketPath) == filepath.Clean(profile.SocketPath)
}

// currentServingMode re-probes what is serving the current profile right now.
// ok is false when the probe itself could not be completed, so callers never
// assert a state they did not observe.
func (deps *Group) currentServingMode() (daemon.ServingMode, bool) {
	st, err := deps.daemonGetStatus()
	if err != nil {
		return daemon.ServingModeUnserved, false
	}
	profile, err := deps.currentDaemonProfile()
	if err != nil {
		return daemon.ServingModeUnserved, false
	}
	return daemon.ClassifyServingMode(deps.observedServingFacts(st, profile)), true
}

// restartFallbackAnnouncement names what is actually serving after the
// direct-start fallback produced a socket. daemonEnsureRunning tries the
// service manager FIRST (launchctl load) and returns nil the moment that
// yields a socket, so "started bossd directly" is not something this path may
// assert without looking: the daemon may well still be supervised.
func (deps *Group) restartFallbackAnnouncement() string {
	mode, ok := deps.currentServingMode()
	switch {
	case ok && mode == daemon.ServingModeStandalone:
		return "Daemon restarted by starting bossd directly: the service manager did not produce a reachable socket, so the daemon is no longer under service-manager supervision."
	case ok && mode == daemon.ServingModeSupervised:
		return "Daemon restarted under the service manager: the first attempt did not produce a reachable socket, and a follow-up start did."
	default:
		return "Daemon restarted by a follow-up start: the service manager's first attempt did not produce a reachable socket."
	}
}

// restartRecoveryHint states what is left serving after a restart that ended
// with no reachable socket, and the command that recovers it.
// daemon.RestartRecoveryHint leads with "the daemon is now stopped", which is
// a claim about the host: the launchd path only makes it
// behind a liveness probe (verifiedRestartOutcome), and this mirrors that
// discipline rather than asserting it blind. Every branch names
// `boss daemon start`, which is what makes recovery discoverable (BOS-1181).
func (deps *Group) restartRecoveryHint() string {
	mode, ok := deps.currentServingMode()
	switch {
	case ok && mode == daemon.ServingModeUnserved:
		return daemon.RestartRecoveryHint
	case ok:
		return fmt.Sprintf("a %s daemon is still running but is not serving the socket; run 'boss daemon stop' then 'boss daemon start'", mode)
	default:
		return "the daemon socket is not reachable; run 'boss daemon start' to restore service"
	}
}

func runDaemonInstall(cmd *cobra.Command) error {
	bossdPath, err := daemon.ResolveBossdPath()
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool("force")
	if err := daemon.Install(bossdPath, force); err != nil {
		return fmt.Errorf("install daemon failed: %w", err)
	}
	installedPath, err := daemon.EnsureStaged(bossdPath)
	if err != nil {
		return fmt.Errorf("resolve installed daemon path: %w", err)
	}

	st, _ := daemon.GetStatus()
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Daemon installed and started.")
	_, _ = fmt.Fprint(out, daemonInstallBossdLine(installedPath, bossdPath))
	if st != nil && st.ServicePath != "" {
		_, _ = fmt.Fprintf(out, "  service: %s\n", st.ServicePath)
	}
	_, _ = fmt.Fprint(out, daemonInstallSettingsLine())
	return nil
}

// daemonInstallSettingsLine names the settings file the install baked into
// the service (BOS-1368). It matters most under `sudo`, whose env_reset
// usually strips BOSS_SETTINGS_PATH: "default" here is how an operator sees
// that the profile they exported never reached the install.
func daemonInstallSettingsLine() string {
	settingsPath, err := daemon.ServiceSettingsPath()
	if err != nil {
		// Install already refused an unbakeable value, so this is unreachable
		// in practice; say so rather than guess.
		return fmt.Sprintf("  settings: unknown (%v)\n", err)
	}
	return fmt.Sprintf("  settings: %s\n", daemon.DescribeServiceSettingsPath(settingsPath))
}

func daemonInstallBossdLine(installedPath, sourcePath string) string {
	if filepath.Clean(installedPath) == filepath.Clean(sourcePath) {
		return fmt.Sprintf("  bossd:   %s\n", sourcePath)
	}
	return fmt.Sprintf("  bossd:   %s (staged from %s)\n", installedPath, sourcePath)
}

func runDaemonUninstall(_ *cobra.Command) error {
	if err := daemon.Uninstall(); err != nil {
		return fmt.Errorf("uninstall daemon failed: %w", err)
	}
	fmt.Println("Daemon uninstalled.")
	return nil
}

func (deps *Group) runDaemonStatus(_ *cobra.Command) error {
	st, err := deps.daemonGetStatus()
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}
	profile, profileErr := deps.currentDaemonProfile()

	// BOS-1183: one reachability verdict drives BOTH the header and the
	// `socket reachable:` field below. The header used to be chosen from
	// Installed/Running alone while the socket was probed ten lines lower, so
	// a launchd job that was registered but never spawned printed "Daemon is
	// running." directly above "socket reachable: false" — observed on a real
	// machine on 2026-09-06. Probing once and branching on that single value
	// is what makes the contradiction unrepresentable rather than merely
	// unlikely.
	//
	// Reachability is only knowable when the profile resolved, because the
	// socket path lives in the profile. When it did not, the verdict is
	// UNKNOWN, not "unreachable" and not "running": this surface must never
	// assert health it did not verify.
	socketKnown := profileErr == nil
	socketReachable := false
	if socketKnown {
		socketReachable = deps.daemonSocketReachable(profile.SocketPath)
	}

	// Gathered ONCE and threaded through the header, the substrate line and the
	// supervision line below. The ownership half shells out to launchctl on an
	// unattended host, so gathering it per-consumer would probe the same job
	// three times for one command.
	supervision, ownership := deps.daemonSupervisionInputs()

	switch {
	case daemonUnattendedSubstrateOwnsVerdict(supervision):
		// BOS-1204 AC1. `st.Installed` is the per-user LaunchAgent, which an
		// unattended install deliberately supersedes, so on this substrate it
		// is false on a perfectly healthy host and the ladder below would print
		// "Daemon is not installed." with an install remedy the operator has
		// already carried out.
		//
		// The header therefore states LIVENESS and defers the detail: the
		// `configured supervision substrate` line two lines down already names
		// the watchdog plist and already FAILs on an insecure one, so there is
		// nothing for a second copy here to add.
		switch {
		case socketKnown && socketReachable:
			fmt.Println("Daemon is running.")
		case socketKnown:
			fmt.Println("Daemon is not serving: the unattended supervision watchdog is installed, but the daemon's socket is unreachable.")
			fmt.Println("  Run 'boss daemon doctor' to diagnose.")
		default:
			fmt.Println("The unattended supervision watchdog is installed, but the daemon's socket could not be probed, so whether it is serving is unknown.")
			fmt.Println("  Run 'boss daemon doctor' to diagnose.")
		}
	case !st.Installed:
		fmt.Println("Daemon is not installed.")
		fmt.Println("  Run 'boss daemon install' to set up the daemon.")
	// BOS-1218: st.Running is a live process the service manager owns, not a
	// registration, so these three lines no longer say "registered" — a job
	// launchd registered and never spawned reaches the default arm below and
	// reads "installed but not running", which is what it is.
	case st.Running && socketKnown && socketReachable:
		fmt.Println("Daemon is running.")
		printDaemonStatusPID(st)
	case st.Running && socketKnown:
		fmt.Println("Daemon is not serving: the service manager reports a running process for the job, but its socket is unreachable.")
		fmt.Println("  Run 'boss daemon doctor' to diagnose.")
		printDaemonStatusPID(st)
	case st.Running:
		fmt.Println("The service manager reports a running process for the daemon, but its socket could not be probed, so whether it is serving is unknown.")
		fmt.Println("  Run 'boss daemon doctor' to diagnose.")
		printDaemonStatusPID(st)
	case socketKnown && socketReachable:
		// BOS-1218 narrowed st.Running, and this arm is what keeps the
		// narrowing from producing a header that contradicts the host. On the
		// reported shape — a registered LaunchAgent launchd never spawned,
		// with a detached bossd holding the socket — st.Running is false now,
		// so without this case the ladder falls through to "Daemon is
		// installed but not running." while a daemon is answering on the very
		// socket this command just probed. Reporting the two facts separately
		// is the whole point: `Running` is a statement about the SERVICE
		// MANAGER's job and socket reachability is a statement about what is
		// serving, and the ticket's key decision is not to conflate them.
		fmt.Println("Daemon is serving, but the service manager has not started its job — something other than the service manager is holding the socket.")
		fmt.Println("  Run 'boss daemon doctor' to diagnose.")
	default:
		fmt.Println("Daemon is installed but not running.")
	}
	if st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	// BOS-1184 R4. Printed unconditionally, unlike the `supervision:` line
	// below: which substrate is CONFIGURED is knowable with nothing installed,
	// nothing running and no state record, and it is exactly the host with no
	// daemon whose operator needs to see it. The label says "configured ...
	// substrate" rather than "supervision mode" so the two lines cannot be read
	// as variations of one fact — `supervision:` six lines down is a live
	// ownership observation, not a configuration. The wording comes from
	// describeDaemonSupervisionMode, the single decision `boss daemon doctor`
	// renders too, so the two surfaces cannot name different modes.
	modeDescription, _ := describeDaemonSupervisionMode(supervision)
	fmt.Printf("  configured supervision substrate: %s\n", modeDescription)
	if profileErr == nil {
		fmt.Printf("  settings: %s\n", profile.SettingsPath)
		fmt.Printf("  app data: %s\n", profile.AppDataDir)
		fmt.Printf("  socket:   %s\n", profile.SocketPath)
		fmt.Printf("  socket reachable: %t\n", socketReachable)
		if metadata, err := daemonstate.Read(profile.AppDataDir); err == nil {
			fmt.Printf("  standalone PID: %d\n", metadata.PID)
			if metadata.ExecutablePath != "" {
				fmt.Printf("  standalone executable: %s\n", metadata.ExecutablePath)
			}
			// "standalone" is a misnomer kept for compatibility: bossd writes
			// this record on every start (services/bossd/cmd/main.go), so the
			// two lines above look identical for a supervised daemon and for
			// one spawned detached. BOS-1183: the classification below is the
			// only thing that tells them apart on this surface.
			fmt.Printf("  %s\n", deps.daemonSupervisionLine(st, metadata.PID, supervision, ownership))
			// The staged file being current does not mean the live process is
			// running those bytes (BOS-864). Keep the two facts distinct here
			// too, so status and doctor can never disagree.
			if line := deps.daemonRunningImageLine(metadata.StartedAt); line != "" {
				fmt.Printf("  %s\n", line)
			}
		}
	} else {
		fmt.Printf("  profile: unavailable (%v)\n", profileErr)
	}
	return nil
}

func printDaemonStatusPID(st *daemon.Status) {
	if st.PID > 0 {
		fmt.Printf("  PID:     %d\n", st.PID)
	}
}

// daemonSupervisionLine classifies the daemon in the state record: does the
// service manager own it, or was it spawned detached with no KeepAlive restart
// and no chance of surviving a reboot?
//
// BOS-1183 R4: losing supervision is a legitimate recovery, never a silent
// one, so the fact has to be labelled on the surface that displays it.
//
// The verdict is NOT decided here. It comes from daemonSupervisionOfLiveRecord
// — the single decision `boss daemon doctor` renders too and
// `boss daemon restart` branches on, via daemon.ClassifyServingMode — so the
// three surfaces cannot give one machine three answers. This function owns
// only the guard rungs above that decision and the wording below it.
//
// One divergence from reportDaemonSupervision survives, and
// TestDaemonSupervisionVerdictsMatchDoctor pins it as the only one: a daemon
// recorded while no service is installed is reported unsupervised here and
// unknown by doctor, because doctor has a separate not-installed check with its
// own remedy and would otherwise print two failures for one fact, whereas here
// the "Daemon is not installed." header three lines up already says why.
func (deps *Group) daemonSupervisionLine(
	st *daemon.Status,
	recordedPID int,
	supervision daemon.SupervisionModeStatus,
	ownership daemon.WatchdogOwnership,
) string {
	// Checked BEFORE the service view is read, exactly as doctor does. Under
	// this env var platformGetStatus returns Installed=true, Running=false
	// without asking launchd or systemd at all — byte-identical to the
	// detached shape this line exists to flag, so interpreting it would make
	// every test harness and CI run print a false warning.
	if os.Getenv("BOSS_DAEMON_SKIP_LAUNCHCTL") != "" {
		return "supervision: unknown (service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL)"
	}
	if recordedPID <= 0 {
		return "supervision: unknown (no recorded daemon PID)"
	}
	// BOS-1183 repair pass: status used to skip this probe and describe the
	// RECORD alone, which is what let it disagree with doctor and restart. A
	// supervised machine whose state record outlived its process read
	// "unsupervised (two daemons, or a stale state record)" here, "unknown" in
	// doctor and "supervised" to restart. One signal-0 call — the same probe
	// doctor already makes — removes the whole disagreement.
	alive, aliveErr := deps.daemonProcessAlive(recordedPID)
	switch {
	case aliveErr != nil:
		return fmt.Sprintf("supervision: unknown (recorded PID %d could not be checked: %v)", recordedPID, aliveErr)
	case !alive:
		return fmt.Sprintf("supervision: unknown (recorded PID %d is not running)", recordedPID)
	}

	_, reason := daemonSupervisionOfLiveRecord(st, recordedPID, supervision, ownership)
	switch reason {
	case daemonSupervisionReasonDetached:
		return fmt.Sprintf(
			"supervision: unsupervised (the service manager does not own the recorded daemon PID %d — %s)",
			recordedPID, daemonUnsupervisedConsequences)
	case daemonSupervisionReasonForeignPID:
		return fmt.Sprintf(
			"supervision: unsupervised (the service manager owns PID %d but the recorded daemon is PID %d — two daemons, or a stale state record; either way the recorded daemon is not under service-manager supervision)",
			st.PID, recordedPID)
	case daemonSupervisionReasonNoServicePID:
		return fmt.Sprintf(
			"supervision: unknown (the service manager reports running but did not report a PID; recorded daemon is PID %d)",
			recordedPID)
	case daemonSupervisionReasonManagerOwned:
		return fmt.Sprintf("supervision: supervised (the service manager owns PID %d)", recordedPID)
	case daemonSupervisionReasonWatchdogOwned:
		// BOS-1204 R7: this is NOT the line above with a different noun. It
		// names the two-step chain because the chain is what an operator has to
		// know to debug it — `launchctl print gui/<uid>/com.bossanova.bossd`
		// finds nothing on this host, deliberately, and someone told only
		// "supervised" would read that as the fault.
		return fmt.Sprintf(
			"supervision: supervised (launchd owns the root-owned %s LaunchDaemon and that watchdog spawns bossd through 'launchctl asuser', so PID %d is a grandchild of launchd rather than a launchd job of its own)",
			daemon.WatchdogLabel, recordedPID)
	case daemonSupervisionReasonWatchdogNotLoaded:
		return fmt.Sprintf(
			"supervision: unsupervised (the %s watchdog is installed at %s but launchd does not have %s loaded, so nothing will restart the recorded daemon PID %d)",
			supervision.Mode, supervision.Unattended.PlistPath, ownership.Target, recordedPID)
	case daemonSupervisionReasonWatchdogUnreadable:
		return fmt.Sprintf(
			"supervision: unknown (the %s watchdog is installed, but whether launchd has %s loaded could not be established: %s)",
			supervision.Mode, daemon.WatchdogTarget(), watchdogOwnershipReason(ownership))
	case daemonSupervisionReasonWatchdogInsecure:
		return fmt.Sprintf(
			"supervision: unknown (the %s watchdog is installed but not safe for a root-owned job, so its ownership of PID %d cannot be relied on — the 'configured supervision substrate' line above names the offending path)",
			supervision.Mode, recordedPID)
	default:
		return fmt.Sprintf("supervision: unknown (the service manager's view of PID %d could not be attributed)", recordedPID)
	}
}

// addDaemonStartFlags defines `boss daemon start`'s flags. It is split out so
// tests build a command carrying exactly the flags the real one has.
func addDaemonStartFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("detached", false,
		"Start an unsupervised bossd directly if the installed service manager does not bring the daemon up")
}

func (deps *Group) runDaemonStart(cmd *cobra.Command) error {
	socketPath, err := deps.defaultSocketPath()
	if err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	if deps.daemonSocketReachable(socketPath) {
		fmt.Println("Daemon is already running.")
		return nil
	}
	// BOS-1519: the operator asked for THIS start, so with a service installed
	// the answer is that service or an explanation — never a quietly created
	// unsupervised daemon that the service can then only fail against.
	// --detached is the deliberate opt-in to that shape. A command without the
	// flag defined reads as false, as runDaemonStop does for its own.
	detached := false
	if cmd != nil {
		detached, _ = cmd.Flags().GetBool("detached")
	}
	policy := daemon.RefuseDetachedFallbackWhenServiceInstalled
	if detached {
		policy = daemon.AllowDetachedFallback
	}
	mode, err := deps.daemonEnsureRunningWithPolicy(socketPath, policy)
	if errors.Is(err, daemon.ErrServiceInstalledNotServing) {
		return fmt.Errorf("start daemon refused: %w. "+
			"Run 'boss daemon doctor' to see why the service manager did not serve it, "+
			"'boss daemon restart' to re-bootstrap the service, "+
			"or 'boss daemon start --detached' to start an unsupervised daemon deliberately", err)
	}
	if err != nil {
		return fmt.Errorf("start daemon failed: %w", err)
	}
	// "Daemon started." stays verbatim and stays first: scripts parse this
	// output, so the supervision verdict is ADDED beneath it rather than
	// restructuring what is already there. BOS-1183 R4 — the fallback spawn is
	// a legitimate recovery, but it must never be a silent one.
	fmt.Println("Daemon started.")
	if mode == daemon.StartModeDetached {
		// This is the surface where the operator FIRST learns supervision was
		// lost, so it is the one that must not understate it: the shared
		// consequence sentence names the immediate keychain/gh damage as well
		// as the reboot one.
		fmt.Printf("  Warning: started detached, not under the service manager — %s.\n", daemonUnsupervisedConsequences)
	}
	return nil
}

func (deps *Group) runDaemonStop(cmd *cobra.Command) error {
	st, err := deps.daemonGetStatus()
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	profile, err := deps.currentDaemonProfile()
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	stopAllStandalone := false
	if cmd != nil {
		stopAllStandalone, _ = cmd.Flags().GetBool("all-standalone")
	}

	if stopAllStandalone {
		n, err := deps.terminateAllBossdProcesses()
		if err != nil {
			return fmt.Errorf("stop standalone bossd failed: %w", err)
		}
		// BOS-349: this --all-standalone branch is the ONLY CLI path allowed to
		// signal plugin processes. It matches bossd-plugin-* by binary name/path
		// across profiles BY DESIGN — the explicit crash-cleanup escape hatch for
		// orphaned plugins. Normal stop/restart must never sweep: bossd's
		// Host.Stop (services/bossd/internal/plugin/host.go) owns reaping its own
		// children, and path-matching cannot attribute a plugin to a profile, so a
		// second daemon sharing the same binaries would lose its live plugins
		// (the BOS-349 mass-SIGTERM incident).
		pluginCount, err := deps.terminateAllPluginProcesses(profile)
		if err != nil {
			return fmt.Errorf("stop plugin processes failed: %w", err)
		}
		if n == 0 && pluginCount == 0 {
			fmt.Println("No standalone bossd process is running.")
			return nil
		}
		if n > 0 && !deps.waitForDaemonSocketGone(profile.SocketPath) {
			return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		if n > 0 {
			fmt.Printf("Stopped %d bossd process(es) across all profiles.\n", n)
		}
		printPluginCleanup(pluginCount)
		return nil
	}

	if !st.Installed {
		n, err := deps.terminateStandaloneCurrentProfile(profile)
		if err != nil {
			return fmt.Errorf("stop standalone bossd failed: %w", err)
		}
		if n == 0 {
			fmt.Println("Daemon is not installed and no standalone bossd is running.")
			return nil
		}
		if !deps.waitForDaemonSocketGone(profile.SocketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		fmt.Println("Stopped standalone bossd for current profile.")
		return nil
	}
	// BOS-1218: this was `!st.Running`, which asked whether the service manager
	// had started its job rather than what is holding the socket. On the
	// reported host `launchctl list` exited 0 for a job launchd had merely
	// registered, so stop booted out a registration that owned no process,
	// signalled nothing, and then polled a socket held by a detached bossd for
	// the full LifecycleShutdownTimeout before failing. A second invocation
	// took this branch and succeeded.
	if deps.stopTakesStandalonePath(st) {
		n, err := deps.terminateStandaloneCurrentProfile(profile)
		if err != nil {
			return fmt.Errorf("stop standalone bossd failed: %w", err)
		}
		if n > 0 {
			if !deps.waitForDaemonSocketGone(profile.SocketPath) {
				return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
			}
			fmt.Println("Stopped standalone bossd for current profile.")
			return nil
		}
		// Nothing standalone answered. If the service manager owns a live
		// process this is the narrow race where the recorded daemon exited
		// between the classification and the signal, and "already stopped"
		// would be a false claim about a host with a running daemon — so fall
		// through to the launchd path, which is what the classifier would pick
		// on a re-read. Everywhere else st.Running is false and nothing is
		// serving, which is what that message has always meant.
		if !st.Running {
			fmt.Println("Daemon is already stopped.")
			return nil
		}
	}
	if err := deps.daemonStop(); err != nil {
		return fmt.Errorf("stop daemon failed: %w", err)
	}
	// Confirm the socket actually went away — bootout returns before the
	// process has fully exited on busy systems, so polling avoids misleading
	// "stopped" output while the old bossd is still draining.
	if !deps.waitForDaemonSocketGone(profile.SocketPath) {
		return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
	}
	fmt.Println("Daemon stopped.")
	return nil
}

// daemonRestartJSON is the `boss daemon restart --json` success envelope.
type daemonRestartJSON struct {
	Daemon daemonRestartBody `json:"daemon"`
}

type daemonRestartBody struct {
	// Outcome is one of the stable outcomeDaemon* codes in clijson.go. A
	// lifecycle command can succeed in materially different ways, and this is
	// the discriminator that survives the trip to a driver.
	Outcome string `json:"outcome"`
	// Supervised reports whether a service manager is running the daemon now.
	// It is stated rather than left to be inferred from Outcome, because
	// "restarted" and "still supervised" are different questions: the
	// standalone outcomes are unsupervised by construction (the state the host
	// was already in), while RESTARTED_UNSUPERVISED is supervision LOST.
	Supervised bool   `json:"supervised"`
	SocketPath string `json:"socket_path"`
	// Message is the same sentence the human path prints. Human-only: a driver
	// branches on Outcome, never on this text.
	Message string `json:"message"`
}

// restartFallbackOutcome picks the stable code for the announced fallback. The
// supervision question is PROBED by the caller (currentServingMode), never
// inferred from the fact that the fallback ran: a follow-up start can land
// under the service manager after all, and reporting that as lost supervision
// would be as wrong as reporting a real loss as an ordinary restart.
func restartFallbackOutcome(supervised bool) string {
	if supervised {
		return cliutil.OutcomeDaemonRestartedAfterFallback
	}
	return cliutil.OutcomeDaemonRestartedUnsupervised
}

// emitDaemonRestart reports one successful restart outcome on whichever channel
// was asked for. The human line is byte-identical to what it printed before
// --json existed.
func emitDaemonRestart(cmd *cobra.Command, asJSON bool, outcome string, supervised bool, socketPath, message string) error {
	if asJSON {
		return cliutil.EmitJSON(cmd, daemonRestartJSON{Daemon: daemonRestartBody{
			Outcome:    outcome,
			Supervised: supervised,
			SocketPath: socketPath,
			Message:    message,
		}})
	}
	fmt.Println(message)
	return nil
}

func (deps *Group) runDaemonRestart(cmd *cobra.Command) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	st, err := deps.daemonGetStatus()
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("daemon restart: %w", err))
	}
	profile, err := deps.currentDaemonProfile()
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("daemon restart: %w", err))
	}
	socketPath := profile.SocketPath

	// BOS-1181: `!st.Installed` alone asked whether a plist file exists, which
	// is never a statement about what is serving. A standalone daemon sitting
	// behind an existing (and here unusable) LaunchAgent fell through to the
	// launchd path, which stopped the process that was serving and could not
	// produce a replacement socket. Restart in kind instead. The Installed
	// clause stays so a profile with no service file keeps its previous
	// behaviour even when nothing is recorded as serving.
	if deps.restartTakesStandalonePath(st) {
		n, err := deps.terminateStandaloneCurrentProfile(profile)
		if err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("restart standalone bossd failed: %w", err))
		}
		if n > 0 && !deps.waitForDaemonSocketGone(socketPath) {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout))
		}
		if n > 0 && !deps.waitForStandaloneBossdExit(profile.AppDataDir) {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("timed out waiting for standalone bossd to exit after %s", daemon.LifecycleShutdownTimeout))
		}
		if err := deps.daemonEnsureRunning(socketPath); err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("restart standalone bossd failed: %w", err))
		}
		// Unsupervised by construction on this path: it restarts in kind what
		// was already running without a service manager, so this is the host's
		// existing state rather than a degradation.
		if n > 0 {
			return emitDaemonRestart(cmd, asJSON, cliutil.OutcomeDaemonRestartedStandalone, false, socketPath,
				"Restarted standalone bossd for current profile.")
		}
		return emitDaemonRestart(cmd, asJSON, cliutil.OutcomeDaemonStartedStandalone, false, socketPath,
			"Started standalone bossd.")
	}
	// BOS-1184 U2: refuse a rejected supervision mode BEFORE anything is
	// stopped, not after.
	//
	// platformRestart carries the same fail-closed check, and on its own that
	// was a REGRESSION of exactly the failure BOS-1181 fixed. The launchd path
	// below stops the running daemon first and only then calls restartDaemon();
	// restartDaemon()'s error return exits immediately, and the
	// daemonEnsureRunning fallback further down is reachable only from
	// waitForDaemonRestartReady. So a refused mode stopped a healthy supervised
	// daemon and then declined to replace it — a net loss of service caused by
	// a settings value, which is the one outcome this seam must never produce.
	//
	// Refusing here changes nothing on the host: the daemon keeps running and
	// the operator fixes the key. That dominates both of the alternatives —
	// gating after the stop (an outage) and not gating restart at all (which
	// silently installs and bootstraps the very substrate the configuration
	// refused, because refreshStagedPlist writes the plist when it is absent).
	//
	// Scoped to the launchd path deliberately: the standalone path above has
	// already returned, and it creates no substrate to refuse, so its previous
	// behaviour is untouched. platformRestart keeps its own check as defence in
	// depth for callers that do not come through here.
	if modeStatus := daemon.LoadSupervisionModeStatus(); modeStatus.Err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("daemon restart refused, daemon left running: daemon supervision mode: %w", modeStatus.Err))
	}
	if st.Running {
		if err := deps.daemonStop(); err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("stop daemon failed: %w", err))
		}
		if !deps.waitForDaemonSocketGone(socketPath) {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout))
		}
	} else {
		n, err := deps.terminateCurrentProfileBossd()
		if err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("restart standalone bossd failed: %w", err))
		}
		if n > 0 && !deps.waitForDaemonSocketGone(socketPath) {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout))
		}
	}
	if err := deps.restartDaemon(); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("restart daemon failed: %w", err))
	}
	if err := deps.waitForDaemonRestartReady(socketPath); err != nil {
		// R1: restoring service outranks honouring the requested supervision
		// mode. The service manager reported success and produced no socket,
		// so fall back to the direct start `boss daemon start` uses rather
		// than exiting non-zero having left the host with no daemon at all.
		restoreErr := deps.daemonEnsureRunning(socketPath)
		if restoreErr == nil && deps.daemonSocketReachable(socketPath) {
			// Announced, not silent: the operator's daemon may no longer be
			// under the service manager, and they would otherwise believe it
			// is. What to announce is probed, not assumed — see below.
			//
			// The announcement is prose, which reached a driver only by being
			// scraped. The envelope carries the same outcome as a stable code,
			// and `supervised` is probed the same way the sentence is rather
			// than assumed from the fact that the fallback ran: a follow-up
			// start under the service manager also lands here and is still
			// supervised.
			mode, ok := deps.currentServingMode()
			supervised := ok && mode == daemon.ServingModeSupervised
			return emitDaemonRestart(cmd, asJSON, restartFallbackOutcome(supervised), supervised, socketPath,
				deps.restartFallbackAnnouncement())
		}
		// restoreErr is the only diagnostic the operator gets for why the
		// fallback failed; discarding it left them with nothing to act on.
		if restoreErr != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("daemon restarted but %w; starting bossd directly also failed: %w; %s", err, restoreErr, deps.restartRecoveryHint()))
		}
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("daemon restarted but %w; starting bossd directly reported success but produced no reachable socket; %s", err, deps.restartRecoveryHint()))
	}
	return emitDaemonRestart(cmd, asJSON, cliutil.OutcomeDaemonRestarted, true, socketPath, "Daemon restarted.")
}

// waitForDaemonRestartReady waits for the replacement daemon to accept
// connections so restart callers cannot report success for the draining daemon.
func (deps *Group) waitForDaemonRestartReady(socketPath string) error {
	deadline := time.Now().Add(deps.daemonRestartReadyTimeout)
	for time.Now().Before(deadline) {
		if deps.daemonSocketReachable(socketPath) {
			return nil
		}
		time.Sleep(deps.daemonRestartPollInterval)
	}
	return fmt.Errorf("socket did not become reachable after %s", deps.daemonRestartReadyTimeout)
}

func restartSocketPath(socketPath string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("daemon restart: %w", err)
	}
	if socketPath == "" {
		return "", fmt.Errorf("daemon restart: could not resolve daemon socket path")
	}
	return socketPath, nil
}

// waitForSocketGone blocks until the unix socket at path stops accepting
// connections or the timeout elapses. Used after stop/restart so we don't
// print "stopped" while the old bossd is still draining.
func waitForSocketGone(path string) bool {
	// Let the user know we may block: bossd drains cron and shuts down its
	// server before removing the socket, which can take several seconds, and
	// silence for that long reads like a hang.
	if daemon.IsSocketReachable(path) {
		fmt.Println("Waiting for bossd to shut down…")
	}
	deadline := time.Now().Add(daemon.LifecycleShutdownTimeout)
	for time.Now().Before(deadline) {
		if !daemon.IsSocketReachable(path) {
			return true
		}
		time.Sleep(daemon.LifecyclePollInterval)
	}
	return false
}

func (deps *Group) waitForCurrentProfileBossdExit() bool {
	profile, err := deps.currentDaemonProfile()
	if err != nil {
		return false
	}
	return deps.waitForStandaloneBossdExit(profile.AppDataDir)
}

func waitForDaemonLockRelease(appDataDir string) bool {
	deadline := time.Now().Add(daemon.LifecycleShutdownTimeout)
	lockPath := filepath.Join(appDataDir, "bossd.lock")
	for time.Now().Before(deadline) {
		// #nosec G304 -- fixed singleton-lock filename beneath the active profile's configured app-data directory; non-secret local state.
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return false
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = lock.Close()
		if err == nil {
			return true
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return false
		}
		time.Sleep(daemon.LifecyclePollInterval)
	}
	return false
}

type daemonProfile struct {
	SettingsPath string
	AppDataDir   string
	SocketPath   string
}

func (deps *Group) currentDaemonProfile() (daemonProfile, error) {
	settingsPath, err := config.Path()
	if err != nil {
		return daemonProfile{}, fmt.Errorf("settings path: %w", err)
	}
	settings, err := config.LoadFrom(settingsPath)
	if err != nil {
		return daemonProfile{}, fmt.Errorf("load settings: %w", err)
	}
	appDataDir, err := appDataDirForSettings(settings)
	if err != nil {
		return daemonProfile{}, err
	}
	socketPath, err := deps.defaultSocketPath()
	if err != nil {
		return daemonProfile{}, fmt.Errorf("socket path: %w", err)
	}
	return daemonProfile{
		SettingsPath: settingsPath,
		AppDataDir:   appDataDir,
		SocketPath:   socketPath,
	}, nil
}

func appDataDirForSettings(settings config.Settings) (string, error) {
	if dir, ok, err := config.ConfiguredAppDataDir(settings); err != nil {
		return "", err
	} else if ok {
		return dir, nil
	}
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("resolve app data dir: %w", err)
	}
	return dir, nil
}

func printPluginCleanup(n int) {
	if n > 0 {
		fmt.Printf("Stopped %d plugin process(es).\n", n)
	}
}
