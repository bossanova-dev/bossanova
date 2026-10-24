package main

import (
	"github.com/recurser/boss/api/cli/chat"
	"github.com/spf13/cobra"
)

func chatCmd() *cobra.Command {
	return chat.New(func(cmd *cobra.Command) (chat.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	})
}
