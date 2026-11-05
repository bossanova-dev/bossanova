package main

import (
	"sort"

	"github.com/recurser/boss/api/cli/diagnostics"
	"github.com/recurser/boss/cmd/skillgen"
	"github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
)

func osGetenv(key string) string { return config.EnvOr(key, "") }
func newDiagnosticsGroup() *diagnostics.Group {
	return diagnostics.New(func(cmd *cobra.Command) (diagnostics.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, diagnostics.Options{GitHubReadCredentialClient: func(cmd *cobra.Command) (diagnostics.GitHubReadCredentialClient, error) {
		return newGitHubReadCredentialClient(cmd)
	}, CLICommandPaths: cliCommandPaths, RevisionDrift: newDaemonGroup().RevisionDrift, TelemetryClient: commandTelemetryClient, CaptureRepairStarted: captureRepairStarted, CaptureRepairCompleted: captureRepairCompleted, SessionCommand: sessionCmd})
}
func cliCommandPaths() []string {
	groups, err := skillgen.Extract(rootCmd())
	if err != nil {
		return nil
	}
	var paths []string
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.IsGroup {
				continue
			}
			paths = append(paths, c.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

// newGitHubReadCredentialClient deliberately bypasses newDaemonClient: this
// wrapper helper must neither start a daemon nor select a remote transport.
func newGitHubReadCredentialClient(*cobra.Command) (*daemonrpc.LocalClient, error) {
	socketPath, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	return daemonrpc.NewLocal(socketPath), nil
}
