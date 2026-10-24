package main

import (
	"github.com/recurser/boss/api/cli/daemon"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/spf13/cobra"
)

func newDaemonGroup() *daemon.Group {
	return daemon.New(func(*cobra.Command) (daemon.Client, error) {
		socket, err := daemonrpc.DefaultSocketPath()
		if err != nil {
			return nil, err
		}
		return daemonrpc.NewLocal(socket), nil
	})
}
func daemonInstallSettingsLine() string { return daemon.InstallSettingsLine() }
func restartReachableDaemonForSettingsReload(socket string) error {
	return newDaemonGroup().RestartReachableForSettingsReload(socket)
}
func restartDaemonAfterUpgrade() error              { return newDaemonGroup().RestartAfterUpgrade() }
func currentDaemonProfile() (daemon.Profile, error) { return newDaemonGroup().CurrentProfile() }
