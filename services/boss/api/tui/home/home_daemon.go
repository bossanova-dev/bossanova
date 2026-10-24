package home

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"

	daemon "github.com/recurser/boss/internal/homeprocess"
)

var runBossDaemonRestart = func() error {
	executable, err := bossDaemonRestartExecutable()
	if err != nil {
		return err
	}

	cmd := System.Command(executable, "daemon", "restart")
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
	if executable, err := System.LookPath("boss"); err == nil {
		return executable, nil
	}
	return os.Executable()
}

var defaultSocketPath = System.DefaultSocketPath

var daemonSocketReachable = System.SocketReachable

var daemonGetStatus = System.DaemonStatus

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

func restartDaemonCmd(services ...Runtime) tea.Cmd {
	socketPathFor := defaultSocketPath
	reachable := daemonSocketReachable
	getStatus := daemonGetStatus
	restart := runBossDaemonRestart
	if len(services) > 0 && services[0] != nil {
		svc := services[0]
		socketPathFor = svc.DefaultSocketPath
		reachable = svc.SocketReachable
		getStatus = svc.DaemonStatus
		restart = func() error { return runDaemonRestart(svc) }
	}
	return func() tea.Msg {
		socketPath, err := socketPathFor()
		if err != nil {
			return daemonRestartMsg{Err: fmt.Errorf("daemon restart: %w", err)}
		}
		if socketPath == "" {
			return daemonRestartMsg{Err: errors.New("daemon restart: could not resolve daemon socket path")}
		}
		socketReachableBeforeRestart := reachable(socketPath)
		readiness, err := restartDaemonForStatusWith(socketReachableBeforeRestart, getStatus, restart)
		if err != nil {
			return daemonRestartMsg{Err: err}
		}
		deadline := time.Now().Add(restartWaitTimeout)
		socketGone := !readiness.waitForSocketGone
		for {
			reachable := reachable(socketPath)
			if !socketGone {
				socketGone = !reachable || daemonRestartedWithNewPIDWith(readiness.oldPID, getStatus)
			}
			if socketGone && reachable {
				return daemonRestartMsg{}
			}
			if !time.Now().Before(deadline) {
				if !socketGone {
					return daemonRestartMsg{Err: errors.New("daemon restart timed out waiting for old socket to stop")}
				}
				return daemonRestartMsg{Err: errors.New("daemon restarted but socket did not become reachable")}
			}
			time.Sleep(restartPollInterval)
		}
	}
}

func restartDaemonForStatus(socketReachableBeforeRestart bool) (daemonRestartReadiness, error) {
	return restartDaemonForStatusWith(socketReachableBeforeRestart, daemonGetStatus, runBossDaemonRestart)
}
func restartDaemonForStatusWith(socketReachableBeforeRestart bool, getStatus func() (*daemon.Status, error), restart func() error) (daemonRestartReadiness, error) {
	st, err := getStatus()
	if err != nil {
		return daemonRestartReadiness{}, fmt.Errorf("daemon restart: %w", err)
	}
	readiness := daemonRestartReadiness{waitForSocketGone: socketReachableBeforeRestart}
	if st != nil {
		readiness.oldPID = st.PID
	}

	if err := restart(); err != nil {
		if st != nil && !st.Installed {
			return daemonRestartReadiness{}, fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		return daemonRestartReadiness{}, fmt.Errorf("restart daemon failed: %w", err)
	}
	if st != nil && !st.Installed {
		return daemonRestartReadiness{}, nil
	}
	if st != nil && st.PID <= 0 {

		return daemonRestartReadiness{}, nil
	}
	return readiness, nil
}

func daemonRestartedWithNewPIDWith(oldPID int, getStatus func() (*daemon.Status, error)) bool {
	if oldPID <= 0 {
		return false
	}
	st, err := getStatus()
	return err == nil && st != nil && st.Running && st.PID > 0 && st.PID != oldPID
}

func (h Model) handleDaemonRestart(msg daemonRestartMsg) (tea.Model, tea.Cmd) {
	h.Restarting = false

	probe := h.startDaemonSupervisionProbe()
	if msg.Err != nil {
		h.upgradeError = msg.Err.Error()
		return h, probe
	}
	h.upgradeError = ""
	h.restartPrompt = false
	h.upgradeAvailable = false
	h.PollFailures = 0
	h.Err = nil
	h.daemonRemediation = ""
	return h, probe
}

func daemonDownRemediation() string {
	if ui.IsRemoteHost() {
		return ui.HostDownRemediation(ui.RemoteHostDestination())
	}
	return "Try:\n\n" +
		"  boss daemon restart   # start or restart the daemon\n" +
		"  boss daemon status    # inspect daemon health\n" +
		"  boss daemon install   # set up automatic startup\n" +
		"  bossd                 # start the daemon manually"
}

func runDaemonRestart(svc Runtime) error {
	executable, err := svc.LookPath("boss")
	if err != nil {
		executable, err = os.Executable()
	}
	if err != nil {
		return err
	}
	output, err := svc.Command(executable, "daemon", "restart").CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
	}
	return err
}
