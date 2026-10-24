package cron

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"
)

func (g *group) cronCmd() *cobra.Command {
	cron := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled cron jobs",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List cron jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runCronLS(cmd)
		},
	}
	ls.Flags().String("repo", "", "Filter by repo ID")
	ls.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")

	show := &cobra.Command{
		Use:   "show <cron-id>",
		Short: "Show cron job details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runCronShow(cmd, args[0])
		},
	}
	show.Flags().Bool("json", false, "Emit a stable JSON schema instead of text")

	add := &cobra.Command{
		Use:   "add",
		Short: "Create a cron job",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runCronAdd(cmd)
		},
	}
	add.Flags().String("repo", "", "Repository ID (required)")
	add.Flags().String("name", "", "Job name (required)")
	add.Flags().String("schedule", "", "5-field cron expression or @daily/@hourly/etc (required)")
	add.Flags().String("prompt", "", "Prompt / plan for each run")
	add.Flags().String("prompt-file", "", "Read the prompt from a file (or '-' for stdin)")
	add.Flags().String("agent", "", "Agent runner plugin name (empty = claude)")
	add.Flags().String("gate", "", "Gate command run before each fire (empty = no gate)")
	add.Flags().String("model", "", "Agent model id (empty = plugin default)")
	add.Flags().String("tz", "", "IANA timezone name (empty = daemon-local)")
	add.Flags().Bool("enabled", true, "Whether the job is enabled")
	add.Flags().Bool("run-setup", true, "Run the repo setup script before the agent")
	// Unlike --enabled/--run-setup, this defaults to false: a zero-output job
	// runs with no worktree, branch or PR, which is never a safe default.
	add.Flags().Bool("zero-output", false, "Run with no worktree, branch, or PR (for jobs that change nothing in this repo)")
	// Empty default: an omitted flag leaves the request field unset, which the
	// daemon resolves to skip.
	add.Flags().String("concurrency", "", "What a fire does while the previous run is still working: skip (default), cancel-in-progress, or allow-concurrent")

	update := &cobra.Command{
		Use:   "update <cron-id>",
		Short: "Update cron job settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runCronUpdate(cmd, args[0])
		},
	}
	update.Flags().String("name", "", "Set job name")
	update.Flags().String("schedule", "", "Set the cron schedule")
	update.Flags().String("prompt", "", "Set the prompt / plan")
	update.Flags().String("prompt-file", "", "Read a new prompt from a file (or '-' for stdin)")
	update.Flags().String("agent", "", "Set the agent runner plugin name")
	update.Flags().String("gate", "", "Set the gate command (empty string clears it)")
	update.Flags().String("model", "", "Set the agent model id (empty string clears it)")
	update.Flags().String("tz", "", "Set the IANA timezone (empty string clears it)")
	// Defaults are false (not true as on `add`) because g.runCronUpdate only reads
	// these via Flags().Changed() — an omitted flag preserves the current value,
	// so a "true" default would never apply and would render a misleading
	// "(default: true)" in the generated skill reference. false is elided there.
	update.Flags().Bool("enabled", false, "Enable or disable the job (unset preserves current)")
	update.Flags().Bool("run-setup", false, "Run the repo setup script before the agent (unset preserves current)")
	update.Flags().Bool("zero-output", false, "Run with no worktree, branch, or PR (unset preserves current)")
	update.Flags().String("concurrency", "", "Set what a fire does while the previous run is still working: skip, cancel-in-progress, or allow-concurrent (unset preserves current)")

	cron.AddCommand(
		ls,
		show,
		add,
		update,
		&cobra.Command{
			Use:   "remove <cron-id>",
			Short: "Remove a cron job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runCronRemove(cmd, args[0])
			},
		},
		&cobra.Command{
			Use:   "run-now <cron-id>",
			Short: "Fire a cron job immediately",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runCronRunNow(cmd, args[0])
			},
		},
		&cobra.Command{
			Use:   "enable <cron-id>",
			Short: "Enable a cron job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runCronEnable(cmd, args[0])
			},
		},
		&cobra.Command{
			Use:   "disable <cron-id>",
			Short: "Disable a cron job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runCronDisable(cmd, args[0])
			},
		},
	)

	return cron
}

// Client is the daemon surface consumed by this command group.
type Client interface {
	CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error)
	GetCronJob(context.Context, string) (*pb.CronJob, error)
	ListCronJobs(context.Context, string) ([]*pb.CronJob, error)
	UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJob(context.Context, string) error
	RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error)
}
type group struct{ newClient cliutil.ClientFactory[Client] }

// New constructs an independent command group.
func New(factory cliutil.ClientFactory[Client]) *cobra.Command {
	g := &group{newClient: factory}
	return g.cronCmd()
}
