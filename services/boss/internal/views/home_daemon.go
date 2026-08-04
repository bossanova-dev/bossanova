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

func daemonDownRemediation() string {
	return "Try:\n\n" +
		"  boss daemon restart   # start or restart the daemon\n" +
		"  boss daemon status    # inspect daemon health\n" +
		"  boss daemon install   # set up automatic startup\n" +
		"  bossd                 # start the daemon manually"
}
