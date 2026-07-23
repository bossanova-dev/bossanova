package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestGetWorkflowStatus pins the status switch the daemon polls to learn
// whether repair monitoring is running, paused, or cancelled. The branches are
// ordered stopped > paused > running, so a cancelled workflow must report
// CANCELLED even while paused is still set — a regression that reversed the
// precedence would let a cancelled workflow look merely paused and be resumed.
func TestGetWorkflowStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stopped bool
		paused  bool
		want    bossanovav1.WorkflowStatus
	}{
		{"running by default", false, false, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING},
		{"paused", false, true, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED},
		{"cancelled", true, false, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
		{"stopped takes precedence over paused", true, true, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rm := newTestMonitor(newTestMock())
			rm.stopped = tt.stopped
			rm.paused = tt.paused

			resp, err := rm.GetWorkflowStatus(
				context.Background(),
				&bossanovav1.GetWorkflowStatusRequest{WorkflowId: "wf-1"},
			)
			require.NoError(t, err)
			require.NotNil(t, resp.GetStatus())
			assert.Equal(t, tt.want, resp.GetStatus().GetStatus())
		})
	}
}

// TestWorkflowLifecycle walks the Pause -> Resume -> Cancel transitions and the
// status each produces, and pins that CancelWorkflow invokes the workflow
// cancel func exactly once and then clears it so a second Cancel is a safe
// no-op rather than a double-cancel.
func TestWorkflowLifecycle(t *testing.T) {
	t.Parallel()

	rm := newTestMonitor(newTestMock())
	ctx := context.Background()

	var cancelCalls int
	rm.cancel = func() { cancelCalls++ }

	status := func() bossanovav1.WorkflowStatus {
		resp, err := rm.GetWorkflowStatus(ctx, &bossanovav1.GetWorkflowStatusRequest{WorkflowId: "wf"})
		require.NoError(t, err)
		return resp.GetStatus().GetStatus()
	}

	// A freshly started monitor reports RUNNING.
	assert.Equal(t, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING, status())

	// Pause -> PAUSED.
	_, err := rm.PauseWorkflow(ctx, &bossanovav1.PauseWorkflowRequest{WorkflowId: "wf"})
	require.NoError(t, err)
	assert.Equal(t, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED, status())

	// Resume -> RUNNING again.
	_, err = rm.ResumeWorkflow(ctx, &bossanovav1.ResumeWorkflowRequest{WorkflowId: "wf"})
	require.NoError(t, err)
	assert.Equal(t, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING, status())

	// Cancel -> CANCELLED, with the cancel func invoked and cleared.
	_, err = rm.CancelWorkflow(ctx, &bossanovav1.CancelWorkflowRequest{WorkflowId: "wf"})
	require.NoError(t, err)
	assert.Equal(t, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED, status())
	assert.Equal(t, 1, cancelCalls)

	rm.mu.Lock()
	clearedCancel := rm.cancel == nil
	rm.mu.Unlock()
	assert.True(t, clearedCancel, "CancelWorkflow must clear the cancel func")

	// A second Cancel must not invoke a cancel func again (already nil).
	_, err = rm.CancelWorkflow(ctx, &bossanovav1.CancelWorkflowRequest{WorkflowId: "wf"})
	require.NoError(t, err)
	assert.Equal(t, 1, cancelCalls, "cancel func must not be invoked twice")
	assert.Equal(t, bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED, status())
}
