package main

import (
	"context"
	"fmt"

	"github.com/recurser/boss/internal/adapter/daemonrpc"
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
		// Deliberately bypass newDaemonClient on the local path: `boss init check`
		// is strictly read-only and must report an unreachable daemon rather than
		// auto-starting bossd.
		NewSetupCheckClient: func(cmd *cobra.Command) (skills.Client, error) {
			if remote := remoteURL(cmd); remote != "" {
				return newRemoteClient(cmd, remote)
			}
			return newLocalSetupCheckClient(cmd)
		},
		SetupCheckSocketPath: daemonrpc.DefaultSocketPath,
	})
}

func newLocalSetupCheckClient(cmd *cobra.Command) (*daemonrpc.LocalClient, error) {
	if host := hostDestination(cmd); host != "" {
		return newHostClient(cmd, host)
	}
	socketPath, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}
	return daemonrpc.NewLocal(socketPath), nil
}

func loadInitProject(ctx context.Context, dir string) (*skills.Project, error) {
	return newRepoGroup().LoadInitProject(ctx, dir)
}
