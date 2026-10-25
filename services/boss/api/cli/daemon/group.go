package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/api/cli/skills"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/adapter/revisiondrift"
	daemon "github.com/recurser/boss/internal/daemonlife"
	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/spf13/cobra"
)

type Group struct {
	newClient cliutil.ClientFactory[Client]

	bossExecutablePath                func() (string, error)
	bossRevisionDriftCheckoutRoot     func() string
	bossRevisionDriftProbe            func(ctx context.Context) revisiondrift.Drift
	bossdFileRevisionDriftProbe       func(ctx context.Context) revisiondrift.Drift
	daemonAuthStateProbe              func(ctx context.Context) (*pb.GetAuthStateResponse, error)
	daemonConsoleOwnerUID             func() (int, error)
	daemonCurrentUID                  func() int
	daemonDoctorBossRevision          func() (revision string, version string)
	daemonDoctorBossdRevision         func(ctx context.Context, path string) (revision string, version string, err error)
	daemonDoctorGOOS                  string
	daemonDoctorRevisionGit           revisiondrift.GitRunner
	daemonDoctorTrustCheckout         func(root string) (bool, error)
	daemonEnsureRunning               func(socketPath string) error
	daemonEnsureRunningWithPolicy     func(socketPath string, policy daemon.DetachedFallbackPolicy) (daemon.StartMode, error)
	daemonGetJobDisabled              func() (daemon.JobDisabled, error)
	daemonGetSpawnHistory             func() (daemon.SpawnHistory, error)
	daemonGetStatus                   func() (*daemon.Status, error)
	daemonLoadSupervisionMode         func() daemon.SupervisionModeStatus
	daemonLocalSupervisionObservation func() daemonSupervisionObservation
	daemonObserveWatchdogOwnership    func() daemon.WatchdogOwnership
	daemonProcesses                   *daemon.ProcessManager
	daemonRestartPollInterval         time.Duration
	daemonRestartReadyTimeout         time.Duration
	daemonSocketReachable             func(socketPath string) bool
	daemonStalenessGOOS               string
	daemonStandaloneServed            func(st *daemon.Status) bool
	daemonStop                        func() error
	defaultSocketPath                 func() (result0 string, resultErr error)
	findDaemonProcess                 func(pid int) (daemon.ProcessSignaler, error)
	restartDaemon                     func() error
	restartDaemonAfterUpgrade         func() error
	terminateAllBossdProcesses        func() (int, error)
	terminateAllPluginProcesses       func(profile daemonProfile) (int, error)
	terminateCurrentProfileBossd      func() (int, error)
	terminateStandaloneCurrentProfile func(profile daemonProfile) (int, error)
	waitForDaemonSocketGone           func(path string) bool
	waitForStandaloneBossdExit        func(appDataDir string) bool
}

func defaultGroup() *Group {
	deps := &Group{}
	deps.daemonDoctorGOOS = runtime.GOOS
	deps.findDaemonProcess = func(pid int) (daemon.ProcessSignaler, error) {
		return os.FindProcess(pid)
	}
	deps.daemonGetSpawnHistory = daemon.GetSpawnHistory
	deps.daemonGetJobDisabled = daemon.GetJobDisabled
	deps.daemonConsoleOwnerUID = func() (int, error) {
		info, err := os.Stat(daemonConsoleDevicePath)
		if err != nil {
			return 0, err
		}
		// Comma-ok, never a bare assertion: a platform whose FileInfo carries no
		// Stat_t must return an error so the verdict fails closed to unknown. A
		// guessed UID here would be reported as an established fact.
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, fmt.Errorf("%s: file info carries no owner UID on this platform", daemonConsoleDevicePath)
		}
		return int(stat.Uid), nil
	}
	deps.daemonCurrentUID = os.Getuid
	deps.daemonStalenessGOOS = runtime.GOOS
	deps.bossExecutablePath = os.Executable
	deps.bossRevisionDriftCheckoutRoot = func() string {
		startDir, err := os.Getwd()
		if err != nil {
			return ""
		}
		root, ok := revisiondrift.FindCheckoutRoot(startDir)
		if !ok {
			return ""
		}
		return root
	}
	deps.daemonDoctorRevisionGit = revisiondrift.ExecGit
	deps.daemonDoctorBossRevision = func() (revision, version string) {
		return buildinfo.Commit, buildinfo.Version
	}
	deps.daemonDoctorBossdRevision = readBossdFileRevision
	deps.daemonDoctorTrustCheckout = func(root string) (bool, error) {
		return skills.TrustedSkillSourceRoot(filepath.Join(root, libskillinstall.SourceRelPath))
	}
	deps.bossRevisionDriftProbe = deps.inspectBossRevisionDrift
	deps.bossdFileRevisionDriftProbe = deps.inspectBossdFileRevisionDrift
	deps.daemonLoadSupervisionMode = daemon.LoadSupervisionModeStatus
	deps.daemonObserveWatchdogOwnership = daemon.ObserveWatchdogOwnership
	deps.daemonLocalSupervisionObservation = deps.observeLocalDaemonSupervision
	deps.restartDaemon = daemon.Restart
	deps.restartDaemonAfterUpgrade = func() error {
		socketPath, err := deps.defaultSocketPath()
		if err != nil {
			return fmt.Errorf("restart daemon after upgrade: socket path: %w", err)
		}
		st, err := deps.daemonGetStatus()
		if err != nil {
			return fmt.Errorf("restart daemon after upgrade: daemon status: %w", err)
		}
		start := func(string) error { return deps.restartDaemon() }
		// Evaluated ONCE and reused below. The predicate is not pure — it re-reads
		// the daemon state record and re-probes process liveness on every call — so
		// deciding twice lets the two halves of one restart disagree, which is the
		// same split-brain class as the defect being fixed (BOS-1181).
		standalone := deps.restartTakesStandalonePath(st)
		if standalone {
			// A standalone daemon must keep its service mode across an upgrade.
			// Restart would create a LaunchAgent on macOS, changing it. Installed
			// alone missed the case where a plist exists but a standalone process
			// is what is actually serving (BOS-1181), so both are checked here.
			start = deps.daemonEnsureRunning
		}
		if err := restartReachableDaemonForSettingsReloadWith(
			socketPath,
			func() (*daemon.Status, error) { return st, nil },
			func(*daemon.Status) bool { return standalone },
			deps.daemonStop,
			start,
			deps.terminateCurrentProfileBossd,
			deps.waitForCurrentProfileBossdExit,
			deps.waitForDaemonSocketGone,
		); err != nil {
			return fmt.Errorf("restart daemon after upgrade: %w", err)
		}
		if err := deps.waitForDaemonRestartReady(socketPath); err != nil {
			// `boss daemon status` names no remediation; the shared hint does, and
			// it is probe-derived, so it does not misreport a daemon this path's
			// own fallback may have left running (BOS-1181).
			return fmt.Errorf("restart daemon after upgrade: %w; %s", err, deps.restartRecoveryHint())
		}
		return nil
	}
	deps.defaultSocketPath = daemonrpc.DefaultSocketPath
	deps.daemonSocketReachable = daemon.IsSocketReachable
	deps.daemonGetStatus = daemon.GetStatus
	deps.daemonEnsureRunning = daemon.EnsureRunning
	deps.daemonEnsureRunningWithPolicy = daemon.EnsureRunningWithPolicy
	deps.daemonStop = daemon.Stop
	deps.daemonStandaloneServed = func(st *daemon.Status) bool {
		profile, err := deps.currentDaemonProfile()
		if err != nil {
			// Without a profile there is no daemon state to attribute the socket
			// to, so fail closed: every caller keeps its previous behaviour.
			return false
		}
		return daemon.ClassifyServingMode(deps.observedServingFacts(st, profile)) == daemon.ServingModeStandalone
	}
	deps.daemonProcesses = daemon.NewProcessManager()
	deps.terminateAllBossdProcesses = deps.daemonProcesses.TerminateBossdProcesses
	deps.terminateCurrentProfileBossd = func() (int, error) {
		profile, err := deps.currentDaemonProfile()
		if err != nil {
			return 0, err
		}
		return deps.terminateStandaloneCurrentProfile(profile)
	}
	deps.terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
		return deps.daemonProcesses.TerminateProfileBossdProcess(profile.AppDataDir, func(pid int) (daemon.ProcessSignaler, error) {
			return os.FindProcess(pid)
		})
	}
	deps.terminateAllPluginProcesses = func(profile daemonProfile) (int, error) {
		return deps.daemonProcesses.TerminateAllBossdPluginProcesses(profile.SettingsPath)
	}
	deps.waitForDaemonSocketGone = waitForSocketGone
	deps.waitForStandaloneBossdExit = waitForDaemonLockRelease
	deps.daemonRestartReadyTimeout = daemon.LifecycleStartupTimeout
	deps.daemonRestartPollInterval = daemon.LifecyclePollInterval
	return deps
}

// Client is the local daemon authentication surface read by doctor.
type Client interface {
	GetAuthState(context.Context) (*pb.GetAuthStateResponse, error)
}

func New(factory cliutil.ClientFactory[Client]) *Group {
	deps := defaultGroup()
	deps.newClient = factory
	return deps
}
func (deps *Group) Command() *cobra.Command                    { return deps.daemonCmd() }
func (deps *Group) WarnIfDaemonBinaryStale(cmd *cobra.Command) { deps.warnIfDaemonBinaryStale(cmd) }
func (deps *Group) WarnIfBossBinaryBehindCheckout(cmd *cobra.Command) {
	deps.warnIfBossBinaryBehindCheckout(cmd)
}
func (deps *Group) WarnIfDaemonUnsupervised(cmd *cobra.Command) { deps.warnIfDaemonUnsupervised(cmd) }
func (deps *Group) RestartAfterUpgrade() error                  { return deps.restartDaemonAfterUpgrade() }
func (deps *Group) RestartReachableForSettingsReload(socket string) error {
	return deps.restartReachableDaemonForSettingsReload(socket)
}
func (deps *Group) CurrentProfile() (Profile, error) { return deps.currentDaemonProfile() }
func (deps *Group) RevisionDrift(ctx context.Context) revisiondrift.Drift {
	return deps.bossRevisionDriftProbe(ctx)
}
func (deps *Group) Supervision() (unsupervised bool, pid int) {
	o := deps.daemonLocalSupervisionObservation()
	return o.unsupervised(), o.RecordedPID
}
func InstallSettingsLine() string { return daemonInstallSettingsLine() }

type Profile = daemonProfile
