package main

import (
	sessioncli "github.com/recurser/boss/api/cli/session"
	"github.com/recurser/boss/api/tui"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/spf13/cobra"
)

func sessionGroup() *sessioncli.Group {
	return sessioncli.New(func(cmd *cobra.Command) (sessioncli.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, sessioncli.Options{
		NewRemoteClient: func(cmd *cobra.Command, url string) (sessioncli.Client, error) { return newRemoteClient(cmd, url) },
		LaunchTUI: func(cmd *cobra.Command, opts sessioncli.LaunchOptions) error {
			return launchTUIWithOptions(cmd, launchTUIOptions{attachSessionID: opts.AttachSessionID, configure: func(app *tui.App) {
				if opts.NewSession {
					app.SetInitialView(ui.ViewNewSession)
					if opts.Agent != "" {
						app.SetInitialAgent(opts.Agent)
					}
					if opts.Account != nil {
						app.SetInitialAgentAccount(*opts.Account)
					}
				} else {
					app.SetInitialView(ui.ViewAttach)
					app.SetAttachSession(opts.AttachSessionID, "")
				}
			}})
		},
	})
}
func sessionCmd() *cobra.Command { return sessionGroup().SessionCmd() }
func lsCmd() *cobra.Command      { return sessionGroup().LSCmd() }
func showCmd() *cobra.Command    { return sessionGroup().ShowCmd() }
func chatsCmd() *cobra.Command   { return sessionGroup().ChatsCmd() }
func newCmd() *cobra.Command     { return sessionGroup().NewCmd() }
func attachCmd() *cobra.Command  { return sessionGroup().AttachCmd() }
func archiveCmd() *cobra.Command { return sessionGroup().ArchiveCmd() }
func mergeCmd() *cobra.Command   { return sessionGroup().MergeCmd() }
func renameCmd() *cobra.Command  { return sessionGroup().RenameCmd() }
func prCmd() *cobra.Command      { return sessionGroup().PRCmd() }
