package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTUI_AutopilotView_EmptyState(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "No workflows") ||
			strings.Contains(screen, "Autopilot Workflows")
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_AutopilotView_ShowsWorkflows(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithWorkflows(testWorkflows()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}

	// Should show the workflow.
	if err := h.Driver.WaitForText(waitTimeout, "wf-001-a"); err != nil {
		t.Fatalf("expected workflow ID on screen; screen:\n%s", h.Driver.Screen())
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "running") {
		t.Fatalf("expected 'running' status on screen:\n%s", screen)
	}
}

func TestTUI_AutopilotView_PauseWorkflow(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithWorkflows(testWorkflows()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "running"); err != nil {
		t.Fatal(err)
	}

	// Press 'p' to pause.
	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "paused"); err != nil {
		t.Fatalf("expected 'paused' status after pause; screen:\n%s", h.Driver.Screen())
	}

	// Verify daemon state.
	workflows := h.Daemon.Workflows()
	if len(workflows) == 0 {
		t.Fatal("expected workflows in daemon")
	}
	if workflows[0].Status != pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED {
		t.Fatalf("expected paused status, got %v", workflows[0].Status)
	}
}

func TestTUI_AutopilotView_ResumeWorkflow(t *testing.T) {
	// Start with a paused workflow.
	workflows := []*pb.AutopilotWorkflow{
		{
			Id:          "wf-001-aaa",
			Status:      pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
			CurrentStep: pb.WorkflowStep_WORKFLOW_STEP_IMPLEMENT,
			FlightLeg:   1,
			MaxLegs:     3,
			PlanPath:    "/tmp/plans/feature.md",
			StartedAt:   timestamppb.Now(),
			UpdatedAt:   timestamppb.Now(),
		},
	}

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithWorkflows(workflows...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "paused"); err != nil {
		t.Fatal(err)
	}

	// Press 'r' to resume.
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "running"); err != nil {
		t.Fatalf("expected 'running' status after resume; screen:\n%s", h.Driver.Screen())
	}

	wfs := h.Daemon.Workflows()
	if len(wfs) == 0 {
		t.Fatal("expected workflows in daemon")
	}
	if wfs[0].Status != pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Fatalf("expected running status, got %v", wfs[0].Status)
	}
}

func TestTUI_AutopilotView_CancelConfirm(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithWorkflows(testWorkflows()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "running"); err != nil {
		t.Fatal(err)
	}

	// Press 'c' to cancel.
	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}

	// Wait for confirmation.
	if err := h.Driver.WaitForText(waitTimeout, "Cancel workflow"); err != nil {
		t.Fatalf("expected cancel confirmation; screen:\n%s", h.Driver.Screen())
	}

	// Confirm with 'y'.
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "cancelled"); err != nil {
		t.Fatalf("expected 'cancelled' status; screen:\n%s", h.Driver.Screen())
	}

	wfs := h.Daemon.Workflows()
	if len(wfs) == 0 {
		t.Fatal("expected workflows in daemon")
	}
	if wfs[0].Status != pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED {
		t.Fatalf("expected cancelled status, got %v", wfs[0].Status)
	}
}

func TestTUI_AutopilotView_CancelDeny(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithWorkflows(testWorkflows()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "running"); err != nil {
		t.Fatal(err)
	}

	// Press 'c' to cancel.
	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Cancel workflow"); err != nil {
		t.Fatal(err)
	}

	// Deny with 'n'.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	// Workflow should still be running.
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "running") {
		t.Fatalf("expected workflow still running after cancel deny; screen:\n%s", screen)
	}

	wfs := h.Daemon.Workflows()
	if len(wfs) == 0 {
		t.Fatal("expected workflows in daemon")
	}
	if wfs[0].Status != pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Fatalf("expected running status after deny, got %v", wfs[0].Status)
	}
}
