// Home's daemon restart flow and the indirection vars tests stub. Split out of
// home.go (BOS-526); the declarations are unchanged.

package views

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
)

var runBossDaemonRestart = func() error {
	executable, err := bossDaemonRestartExecutable()
	if err != nil {
		return err
	}
	// #nosec G204 -- resolved installed CLI or the running direct-path executable; literal args; no shell
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command(executable, "daemon", "restart")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
		return err
	}
	return nil
}

func bossDaemonRestartExecutable() (string, error) {
	if executable, err := exec.LookPath("boss"); err == nil {
		return executable, nil
	}
	return os.Executable()
}

var defaultSocketPath = client.DefaultSocketPath

var daemonSocketReachable = daemon.IsSocketReachable

var daemonGetStatus = daemon.GetStatus

var restartPollInterval = daemon.LifecyclePollInterval

// restartWaitTimeout bounds the whole restart loop, which first waits for the
// old socket to go away (graceful shutdown) and then for the new socket to
// become reachable (startup). It therefore spans both lifecycle budgets so a
// slow cron drain no longer produces a false "timed out" error.
var restartWaitTimeout = daemon.LifecycleShutdownTimeout + daemon.LifecycleStartupTimeout

type daemonRestartReadiness struct {
	waitForSocketGone bool
	oldPID            int
}

func restartDaemonCmd() tea.Cmd {
	return func() tea.Msg {
		socketPath, err := defaultSocketPath()
		if err != nil {
			return daemonRestartMsg{err: fmt.Errorf("daemon restart: %w", err)}
		}
		if socketPath == "" {
			return daemonRestartMsg{err: errors.New("daemon restart: could not resolve daemon socket path")}
		}
		socketReachableBeforeRestart := daemonSocketReachable(socketPath)
		readiness, err := restartDaemonForStatus(socketReachableBeforeRestart)
		if err != nil {
			return daemonRestartMsg{err: err}
		}
		deadline := time.Now().Add(restartWaitTimeout)
		socketGone := !readiness.waitForSocketGone
		for {
			reachable := daemonSocketReachable(socketPath)
			if !socketGone {
				socketGone = !reachable || daemonRestartedWithNewPID(readiness.oldPID)
			}
			if socketGone && reachable {
				return daemonRestartMsg{}
			}
			if !time.Now().Before(deadline) {
				if !socketGone {
					return daemonRestartMsg{err: errors.New("daemon restart timed out waiting for old socket to stop; check 'boss daemon status'")}
				}
				return daemonRestartMsg{err: errors.New("daemon restarted but socket did not become reachable; check 'boss daemon status'")}
			}
			time.Sleep(restartPollInterval)
		}
	}
}

func restartDaemonForStatus(socketReachableBeforeRestart bool) (daemonRestartReadiness, error) {
	st, err := daemonGetStatus()
	if err != nil {
		return daemonRestartReadiness{}, fmt.Errorf("daemon restart: %w", err)
	}
	readiness := daemonRestartReadiness{waitForSocketGone: socketReachableBeforeRestart}
	if st != nil {
		readiness.oldPID = st.PID
	}

	if err := runBossDaemonRestart(); err != nil {
		if st != nil && !st.Installed {
			return daemonRestartReadiness{}, fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		return daemonRestartReadiness{}, fmt.Errorf("restart daemon failed: %w", err)
	}
	if st != nil && !st.Installed {
		return daemonRestartReadiness{}, nil
	}
	if st != nil && st.PID <= 0 {
		// `boss daemon restart` has already waited until its replacement is
		// ready. Without a pre-restart PID there is no reliable way for the
		// TUI to observe the old socket disappearing, so do not repeat that
		// completed handoff here.
		return daemonRestartReadiness{}, nil
	}
	return readiness, nil
}

func daemonRestartedWithNewPID(oldPID int) bool {
	if oldPID <= 0 {
		return false
	}
	st, err := daemonGetStatus()
	return err == nil && st != nil && st.Running && st.PID > 0 && st.PID != oldPID
}

func (h HomeModel) handleDaemonRestart(msg daemonRestartMsg) (tea.Model, tea.Cmd) {
	h.restarting = false
	if msg.err != nil {
		h.upgradeError = msg.err.Error()
		return h, nil
	}
	h.upgradeError = ""
	h.restartPrompt = false
	h.upgradeAvailable = false
	h.pollFailures = 0
	h.err = nil
	h.daemonRemediation = ""
	return h, nil
}

// The --host destination and its accessors live in hostcontext.go: a dropped
// tunnel is only one of several places the remote context changes what boss may
// do locally, so the switch is shared rather than owned here.

func daemonDownRemediation() string {
	if isRemoteHost() {
		return hostDownRemediation(remoteHostDestination())
	}
	return "Try:\n\n" +
		"  boss daemon restart   # start or restart the daemon\n" +
		"  boss daemon status    # inspect daemon health\n" +
		"  boss daemon install   # set up automatic startup\n" +
		"  bossd                 # start the daemon manually"
}

// hostDownRemediation is the --host wording for a connection the supervisor is
// already repairing on its own: say so, and point at the machine that actually
// stopped answering rather than at the local daemon boss is not talking to.
// The wording mirrors the startup wait screen (cmd/host.go hostReconnectIssue)
// so a drop reads the same wherever it is caught.
func hostDownRemediation(destination string) string {
	// Consuming Start's done channel bought a second fact besides the reason:
	// whether anything is still redialling. Promising an automatic recovery once
	// the supervisor has unwound is the same silence BOS-724 was filed for,
	// dressed as reassurance — the user would wait for a reconnect that is not
	// coming.
	remediation := "Reconnecting to " + destination + ".\n\n" +
		"The ssh tunnel dropped. Boss keeps redialling and resumes automatically.\n\n"
	if remoteHostSupervisorStopped() {
		remediation = "Disconnected from " + destination + ".\n\n" +
			"The ssh tunnel dropped and its supervisor stopped, so boss is not redialling. Restart boss to reconnect.\n\n"
	}
	// The supervisor classifies every ssh exit; before BOS-724 that diagnosis
	// only reached a Debug line, so a user watching a stalled TUI had no way to
	// learn why. It is absent on the very first drop, when the RPC fails before
	// the child has exited, so the affordance reads correctly without it.
	if reason := remoteHostFailureReason(); reason != "" {
		remediation += "Last tunnel failure: " + reason + "\n\n"
	}
	return remediation +
		"If it does not come back:\n\n" +
		"  ssh " + destination + "                    # check the connection itself\n" +
		"  ssh " + destination + " boss daemon status # check bossd on that host\n" +
		// `boss tail` with no source reads the *bossd* log, which under --host is
		// a local daemon boss is not talking to and never carries a line about
		// the ssh child. The supervisor logs through the boss process's own
		// logger, so the source has to be named or this pointer sends the user
		// to the same empty log BOS-724 already wasted an hour on.
		"  boss tail boss                      # every reconnect attempt, on this machine"
}
