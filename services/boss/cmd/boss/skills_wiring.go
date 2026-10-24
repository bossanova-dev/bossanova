package main

import (
	"context"

	"github.com/recurser/boss/internal/adapter/sshhost"

	"github.com/recurser/boss/api/cli/skills"
	"github.com/spf13/cobra"
)

func newSkillsGroup() *skills.Group {
	return skills.New(func(cmd *cobra.Command) (skills.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, skills.Config{
		LoadProject:        loadInitProject,
		ShutdownHostTunnel: sshhost.Shutdown,
	})
}

func loadInitProject(ctx context.Context, dir string) (*skills.Project, error) {
	return newRepoGroup().LoadInitProject(ctx, dir)
}
