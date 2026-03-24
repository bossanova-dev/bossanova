package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func autopilotCmd() *cobra.Command {
	ap := &cobra.Command{
		Use:     "autopilot",
		Aliases: []string{"ap"},
		Short:   "Manage autopilot workflows",
	}

	ap.AddCommand(
		autopilotStartCmd(),
		autopilotStatusCmd(),
		autopilotListCmd(),
		autopilotPauseCmd(),
		autopilotResumeCmd(),
		autopilotCancelCmd(),
	)

	return ap
}

func autopilotStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start <plan-file>",
		Short: "Start an autopilot workflow from a plan file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutopilotStart(cmd, args[0])
		},
	}
	cmd.Flags().Int32("max-legs", 0, "Maximum flight legs (0 = use default)")
	cmd.Flags().Bool("confirm-land", false, "Pause for confirmation before landing")
	return cmd
}

func autopilotStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [workflow-id]",
		Short: "Show autopilot workflow status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			return runAutopilotStatus(cmd, id)
		},
	}
	cmd.Flags().BoolP("follow", "f", false, "Stream output in real-time")
	return cmd
}

func autopilotListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List autopilot workflows",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutopilotList(cmd)
		},
	}
	cmd.Flags().Bool("all", false, "Include completed and cancelled workflows")
	return cmd
}

func autopilotPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause [workflow-id]",
		Short: "Pause an autopilot workflow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			return runAutopilotPause(cmd, id)
		},
	}
}

func autopilotResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume [workflow-id]",
		Short: "Resume a paused autopilot workflow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			return runAutopilotResume(cmd, id)
		},
	}
}

func autopilotCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel [workflow-id]",
		Short: "Cancel an autopilot workflow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			return runAutopilotCancel(cmd, id)
		},
	}
}

// --- Handlers ---

func runAutopilotStart(cmd *cobra.Command, planFile string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	maxLegs, _ := cmd.Flags().GetInt32("max-legs")
	confirmLand, _ := cmd.Flags().GetBool("confirm-land")

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	ctx := context.Background()
	w, err := c.StartAutopilot(ctx, &pb.StartAutopilotRequest{
		PlanPath:         planFile,
		MaxLegs:          maxLegs,
		ConfirmLand:      confirmLand,
		WorkingDirectory: wd,
	})
	if err != nil {
		return fmt.Errorf("start autopilot: %w", err)
	}

	fmt.Printf("Autopilot workflow started.\n")
	printWorkflowSummary(w)
	return nil
}

func runAutopilotStatus(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if id == "" {
		id, err = resolveActiveWorkflow(ctx, c)
		if err != nil {
			return err
		}
	}

	follow, _ := cmd.Flags().GetBool("follow")
	if follow {
		return streamAutopilotOutput(ctx, c, id)
	}

	w, err := c.GetAutopilotStatus(ctx, id)
	if err != nil {
		return fmt.Errorf("get autopilot status: %w", err)
	}

	printWorkflowSummary(w)
	return nil
}

func runAutopilotList(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	all, _ := cmd.Flags().GetBool("all")

	ctx := context.Background()
	workflows, err := c.ListAutopilotWorkflows(ctx, &pb.ListAutopilotWorkflowsRequest{
		IncludeAll: all,
	})
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}

	if len(workflows) == 0 {
		fmt.Println("No autopilot workflows found.")
		return nil
	}

	printWorkflowTable(cmd, workflows)
	return nil
}

func runAutopilotPause(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if id == "" {
		id, err = resolveActiveWorkflow(ctx, c)
		if err != nil {
			return err
		}
	}

	w, err := c.PauseAutopilot(ctx, id)
	if err != nil {
		return fmt.Errorf("pause autopilot: %w", err)
	}

	fmt.Printf("Workflow %s paused.\n", w.Id)
	return nil
}

func runAutopilotResume(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if id == "" {
		id, err = resolveActiveWorkflow(ctx, c)
		if err != nil {
			return err
		}
	}

	w, err := c.ResumeAutopilot(ctx, id)
	if err != nil {
		return fmt.Errorf("resume autopilot: %w", err)
	}

	fmt.Printf("Workflow %s resumed.\n", w.Id)
	return nil
}

func runAutopilotCancel(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if id == "" {
		id, err = resolveActiveWorkflow(ctx, c)
		if err != nil {
			return err
		}
	}

	w, err := c.CancelAutopilot(ctx, id)
	if err != nil {
		return fmt.Errorf("cancel autopilot: %w", err)
	}

	fmt.Printf("Workflow %s cancelled.\n", w.Id)
	return nil
}

// --- Helpers ---

// resolveActiveWorkflow finds the most recent active workflow when no ID is provided.
func resolveActiveWorkflow(ctx context.Context, c client.BossClient) (string, error) {
	workflows, err := c.ListAutopilotWorkflows(ctx, &pb.ListAutopilotWorkflowsRequest{})
	if err != nil {
		return "", fmt.Errorf("list workflows: %w", err)
	}

	// Filter to running or paused.
	var active []*pb.AutopilotWorkflow
	for _, w := range workflows {
		if w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING ||
			w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED ||
			w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PENDING {
			active = append(active, w)
		}
	}

	if len(active) == 0 {
		return "", fmt.Errorf("no active autopilot workflows found")
	}
	if len(active) > 1 {
		return "", fmt.Errorf("multiple active workflows found — specify a workflow ID:\n%s", formatWorkflowIDs(active))
	}
	return active[0].Id, nil
}

func formatWorkflowIDs(workflows []*pb.AutopilotWorkflow) string {
	lines := make([]string, 0, len(workflows))
	for _, w := range workflows {
		lines = append(lines, fmt.Sprintf("  %s  %s  %s",
			w.Id,
			views.WorkflowStatusLabel(w.Status),
			w.PlanPath,
		))
	}
	return strings.Join(lines, "\n")
}

func streamAutopilotOutput(ctx context.Context, c client.BossClient, workflowID string) error {
	stream, err := c.StreamAutopilotOutput(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("stream autopilot output: %w", err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Receive() {
		msg := stream.Msg()
		switch e := msg.Event.(type) {
		case *pb.StreamAutopilotOutputResponse_OutputLine:
			fmt.Print(e.OutputLine.Text)
		case *pb.StreamAutopilotOutputResponse_StatusUpdate:
			fmt.Printf("\n--- Status: %s (step: %s, leg: %d/%d) ---\n",
				views.WorkflowStatusLabel(e.StatusUpdate.Status),
				views.WorkflowStepLabel(e.StatusUpdate.CurrentStep),
				e.StatusUpdate.FlightLeg,
				e.StatusUpdate.MaxLegs,
			)
		}
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("stream error: %w", err)
	}
	return nil
}

func printWorkflowSummary(w *pb.AutopilotWorkflow) {
	fmt.Printf("  ID:      %s\n", w.Id)
	fmt.Printf("  Status:  %s\n", views.WorkflowStatusLabel(w.Status))
	fmt.Printf("  Step:    %s\n", views.WorkflowStepLabel(w.CurrentStep))
	legStr := fmt.Sprintf("%d/%d", w.FlightLeg, w.MaxLegs)
	if w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED && w.FlightLeg < w.MaxLegs {
		legStr += " (incomplete)"
	}
	fmt.Printf("  Leg:     %s\n", legStr)
	fmt.Printf("  Plan:    %s\n", w.PlanPath)
	if w.LastError != "" {
		fmt.Printf("  Error:   %s\n", w.LastError)
	}
	if w.StartedAt != nil {
		elapsed := time.Since(w.StartedAt.AsTime()).Truncate(time.Second)
		fmt.Printf("  Elapsed: %s\n", elapsed)
	}
}

func printWorkflowTable(cmd *cobra.Command, workflows []*pb.AutopilotWorkflow) {
	ids := make([]string, len(workflows))
	statuses := make([]string, len(workflows))
	steps := make([]string, len(workflows))
	legs := make([]string, len(workflows))
	plans := make([]string, len(workflows))
	durations := make([]string, len(workflows))

	for i, w := range workflows {
		ids[i] = w.Id
		statuses[i] = views.WorkflowStatusLabel(w.Status)
		steps[i] = views.WorkflowStepLabel(w.CurrentStep)
		legStr := fmt.Sprintf("%d/%d", w.FlightLeg, w.MaxLegs)
		if w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED && w.FlightLeg < w.MaxLegs {
			legStr += " (incomplete)"
		}
		legs[i] = legStr

		plan := w.PlanPath
		runes := []rune(plan)
		if len(runes) > 40 {
			plan = "..." + string(runes[len(runes)-37:])
		}
		plans[i] = plan

		if w.StartedAt != nil {
			elapsed := time.Since(w.StartedAt.AsTime()).Truncate(time.Second)
			durations[i] = elapsed.String()
		} else {
			durations[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 18)},
		{Title: "STATUS", Width: views.MaxColWidth("STATUS", statuses, 12)},
		{Title: "STEP", Width: views.MaxColWidth("STEP", steps, 12)},
		{Title: "LEG", Width: views.MaxColWidth("LEG", legs, 8)},
		{Title: "PLAN", Width: views.MaxColWidth("PLAN", plans, 40)},
		{Title: "ELAPSED", Width: views.MaxColWidth("ELAPSED", durations, 12)},
	}

	rows := make([]table.Row, len(workflows))
	for i := range workflows {
		rows[i] = table.Row{ids[i], statuses[i], steps[i], legs[i], plans[i], durations[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
}
