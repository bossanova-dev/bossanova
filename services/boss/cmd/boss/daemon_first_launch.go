package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/spf13/cobra"
)

const firstLaunchOptOutEnv = "BOSS_NO_AUTO_DAEMON_INSTALL"

type firstLaunchRegistration struct {
	goos              string
	getenv            func(string) string
	settingsProfile   func() (string, error)
	socketPath        func() (string, error)
	socketReachable   func(string) bool
	serviceRegistered func() (bool, error)
	supervision       func() daemon.SupervisionModeStatus
	resolveBossd      func() (string, error)
	install           func(string, bool) error
	uninstall         func() error
	startWait         time.Duration
	pollEvery         time.Duration
	now               func() time.Time
	sleep             func(time.Duration)
	out               io.Writer
}

func newFirstLaunchRegistration(out io.Writer) firstLaunchRegistration {
	return firstLaunchRegistration{
		goos:              runtime.GOOS,
		getenv:            os.Getenv,
		settingsProfile:   daemon.ServiceSettingsPath,
		socketPath:        daemonrpc.DefaultSocketPath,
		socketReachable:   daemon.IsSocketReachable,
		serviceRegistered: daemon.ServiceRegistered,
		supervision:       daemon.LoadSupervisionModeStatus,
		resolveBossd:      daemon.ResolveBossdPath,
		install:           daemon.Install,
		uninstall:         daemon.Uninstall,
		startWait:         daemon.LifecycleStartupTimeout,
		pollEvery:         daemon.LifecyclePollInterval,
		now:               time.Now,
		sleep:             time.Sleep,
		out:               out,
	}
}

func (r firstLaunchRegistration) run() error {
	if r.goos != "darwin" {
		return nil
	}
	if r.getenv(firstLaunchOptOutEnv) != "" {
		return nil
	}
	if profile, err := r.settingsProfile(); err != nil || profile != "" {
		return nil
	}
	sock, err := r.socketPath()
	if err != nil || r.socketReachable(sock) {
		return nil
	}
	registered, err := r.serviceRegistered()
	if err != nil || registered {
		return nil
	}
	sup := r.supervision()
	if sup.Err != nil || sup.Mode != daemon.SupervisionModeLaunchAgent {
		return nil
	}
	bossdPath, err := r.resolveBossd()
	if err != nil {
		r.printf("Warning: could not register bossd service (%v); run 'boss daemon install' to set up automatic startup.\n", err)
		return nil
	}
	if err := r.install(bossdPath, false); err != nil {
		if cleanupErr := r.cleanupFailedInstall(err); cleanupErr != nil {
			r.printf("Warning: could not register bossd service (%v; cleanup failed: %v); run 'boss daemon install --force' to set up automatic startup.\n", err, cleanupErr)
			return nil
		}
		r.printf("Warning: could not register bossd service (%v); run 'boss daemon install' to set up automatic startup.\n", err)
		return nil
	}
	r.printf("Registered bossd with launchd so it starts at login (set %s=1 to disable)…\n", firstLaunchOptOutEnv)
	return r.waitForSocket(sock)
}

func (r firstLaunchRegistration) cleanupFailedInstall(installErr error) error {
	if strings.HasPrefix(installErr.Error(), "plist already exists") {
		return nil
	}
	registered, err := r.serviceRegistered()
	if err != nil {
		return err
	}
	if !registered {
		return nil
	}
	return r.uninstall()
}

func (r firstLaunchRegistration) waitForSocket(sock string) error {
	deadline := r.now().Add(r.startWait)
	for {
		if r.socketReachable(sock) {
			return nil
		}
		if !r.now().Before(deadline) {
			return fmt.Errorf("%w: registered bossd LaunchAgent did not serve %s within %s", daemon.ErrServiceInstalledNotServing, sock, r.startWait)
		}
		r.sleep(r.pollEvery)
	}
}

func (r firstLaunchRegistration) printf(format string, args ...any) {
	if r.out != nil {
		_, _ = fmt.Fprintf(r.out, format, args...)
	}
}

var registerDaemonOnFirstLaunch = func() error {
	return newFirstLaunchRegistration(os.Stderr).run()
}

var dialTUIDaemonClient = newDaemonClient

func newDaemonClientRegisteringDaemon(cmd *cobra.Command) (*daemonrpc.LocalClient, error) {
	if needsLocalDaemonStartup(cmd) {
		if err := registerDaemonOnFirstLaunch(); err != nil {
			return nil, err
		}
	}
	return dialTUIDaemonClient(cmd)
}
