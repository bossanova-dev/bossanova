package daemon

import (
	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"
)

func (deps *Group) daemonCmd() *cobra.Command {
	d := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the bossd daemon",
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install bossd as a background service (launchd on macOS, systemd on Linux)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonInstall(cmd)
		},
	}
	install.Flags().Bool("force", false, "Overwrite existing service file")

	start := &cobra.Command{
		Use:   "start",
		Short: "Start the bossd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return deps.runDaemonStart(cmd)
		},
	}
	addDaemonStartFlags(start)

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the bossd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return deps.runDaemonStop(cmd)
		},
	}
	stop.Flags().Bool("all-standalone", false, "Stop all user-owned bossd processes instead of only the current profile")

	restart := &cobra.Command{
		Use:   "restart",
		Short: "Restart the bossd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return deps.runDaemonRestart(cmd)
		},
	}
	restart.Flags().Bool(cliutil.JSONFlagName, false,
		"Emit the restart outcome as a stable JSON schema, so a driver can tell a "+
			"supervised restart from one that fell back to an unsupervised direct start")

	d.AddCommand(
		install,
		&cobra.Command{
			Use:   "doctor",
			Short: "Diagnose the bossd daemon: staged binary, LaunchAgent path, macOS folder permissions, and live upstream auth/registration state",
			RunE: func(cmd *cobra.Command, args []string) error {
				return deps.runDaemonDoctor(cmd)
			},
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Uninstall the bossd LaunchAgent",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonUninstall(cmd)
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show bossd daemon status",
			RunE: func(cmd *cobra.Command, args []string) error {
				return deps.runDaemonStatus(cmd)
			},
		},
		start,
		stop,
		restart,
		&cobra.Command{
			Use:   "rotate-token",
			Short: "Rotate the daemon socket auth token (regenerated on next daemon start)",
			RunE: func(cmd *cobra.Command, args []string) error {
				return deps.runDaemonRotateToken(cmd)
			},
		},
	)

	return d
}
