package main

import (
	"sort"

	"github.com/recurser/boss/api/cli/diagnostics"
	"github.com/recurser/boss/cmd/skillgen"
	"github.com/recurser/bossalib/config"
	"github.com/spf13/cobra"
)

func osGetenv(key string) string { return config.EnvOr(key, "") }
func newDiagnosticsGroup() *diagnostics.Group {
	return diagnostics.New(func(cmd *cobra.Command) (diagnostics.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, diagnostics.Options{CLICommandPaths: cliCommandPaths, RevisionDrift: newDaemonGroup().RevisionDrift, TelemetryClient: commandTelemetryClient, CaptureRepairStarted: captureRepairStarted, CaptureRepairCompleted: captureRepairCompleted, SessionCommand: sessionCmd})
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
