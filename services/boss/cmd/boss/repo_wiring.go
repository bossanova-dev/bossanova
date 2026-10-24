package main

import (
	repocli "github.com/recurser/boss/api/cli/repo"
	settingscli "github.com/recurser/boss/api/cli/settings"
	"github.com/recurser/boss/api/tui"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/spf13/cobra"
)

func newRepoGroup() *repocli.Group {
	return repocli.New(func(cmd *cobra.Command) (repocli.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, repocli.Options{
		Profile: func() (repocli.DaemonProfile, error) {
			p, err := currentDaemonProfile()
			return repocli.DaemonProfile{SettingsPath: p.SettingsPath, AppDataDir: p.AppDataDir, SocketPath: p.SocketPath}, err
		},
		CloudRemote: settingscli.NewCloudRemote,
		LaunchRepoAdd: func(cmd *cobra.Command) error {
			return launchTUI(cmd, func(app *tui.App) { app.SetInitialView(ui.ViewRepoAdd) })
		},
	})
}
