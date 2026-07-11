package plugin_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// startArmedRepairHost builds the real bossd-plugin-repair binary, starts a
// fully-wired Host on a fast health tick, declares the repair workflow desired,
// and arms it via EnsureWorkflowRunning. It returns the host wrapper. Shared by
// the SIGTERM-restart and zombie-heal tests.
func startArmedRepairHost(t *testing.T, tick time.Duration) *discoveryHost {
	t.Helper()

	// Fast tick so the restart/watchdog paths converge within the test window.
	// Set before Start (the interval is read when the ticker is constructed).
	restore := pluginpkg.SetHealthCheckInterval(tick)
	t.Cleanup(restore)

	pluginDir := t.TempDir()
	binPath := pluginharness.BuildPluginInto(t, pluginDir, "bossd-plugin-repair")
	cfgs := []config.PluginConfig{{Name: "repair", Path: binPath, Enabled: true}}

	d := newDiscoveryHost(t)
	if err := d.host.Start(t.Context(), cfgs, config.Settings{}); err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	req := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":5}`}
	d.host.SetWorkflowDesired("repair", req)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Arm explicitly. The live watchdog tick can win the arm first, in which case
	// this call re-probes and observes RUNNING (started=false) — that IS the armed
	// state we want, so don't assert `started` here (a false-negative flake).
	// Poll for RUNNING below to establish the armed precondition deterministically.
	if _, _, err := d.host.EnsureWorkflowRunning(ctx, "repair"); err != nil {
		t.Fatalf("initial EnsureWorkflowRunning: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if repairWorkflowStatus(t, d) == bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("repair workflow never reached RUNNING after arm")
	return d
}

// repairWorkflowStatus reads the current WorkflowStatus from the live repair
// plugin (re-resolving the service each call so it survives a respawn).
func repairWorkflowStatus(t *testing.T, d *discoveryHost) bossanovav1.WorkflowStatus {
	t.Helper()
	svcs := d.host.GetWorkflowServices()
	if len(svcs) != 1 {
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	info, err := svcs[0].GetWorkflowStatus(ctx, "")
	if err != nil {
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
	return info.GetStatus()
}

// TestRepairPluginSigtermRestartsAndRearms is the end-to-end proof of the
// BOS-346 incident fix: an external SIGTERM of the repair plugin subprocess
// must (1) make the process exit (Task 1 — the old binary stayed alive as a
// zombie), (2) get the process respawned by the health loop under a new pid,
// and (3) leave the workflow re-armed to RUNNING from desired state — with no
// daemon restart.
func TestRepairPluginSigtermRestartsAndRearms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repair SIGTERM restart integration test in short mode")
	}

	d := startArmedRepairHost(t, 50*time.Millisecond)

	oldPID := d.host.PluginPIDs()["repair"]
	if oldPID <= 0 {
		t.Fatalf("no pid recorded for repair: %v", d.host.PluginPIDs())
	}
	if got := repairWorkflowStatus(t, d); got != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Fatalf("baseline workflow status = %s, want RUNNING", got)
	}

	// SIGTERM the plugin. With Task 1's fix the plugin drains and EXITS, so the
	// health loop restarts it and re-arms the workflow from desired state.
	proc, err := os.FindProcess(oldPID)
	if err != nil {
		t.Fatalf("find repair process %d: %v", oldPID, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM repair (pid %d): %v", oldPID, err)
	}

	// The health loop must relaunch repair under a NEW pid.
	var newPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p := d.host.PluginPIDs()["repair"]; p > 0 && p != oldPID {
			newPID = p
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if newPID == 0 {
		t.Fatalf("repair was not relaunched under a new pid (still %d) after SIGTERM", oldPID)
	}

	// And the workflow must be re-armed to RUNNING over the fresh subprocess.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if repairWorkflowStatus(t, d) == bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
			return // re-armed
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("repair workflow was not re-armed to RUNNING after SIGTERM restart (status=%s)", repairWorkflowStatus(t, d))
}

// TestWatchdogRearmsAliveCancelledWorkflow proves the zombie-heal path: forcing
// the alive-but-CANCELLED state (a CancelWorkflow RPC on a live plugin, without
// killing the process) is healed by the health-tick watchdog within one tick —
// GetWorkflowStatus returns to RUNNING and the process pid is unchanged (healed
// in place, not restarted).
func TestWatchdogRearmsAliveCancelledWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watchdog zombie-heal integration test in short mode")
	}

	d := startArmedRepairHost(t, 50*time.Millisecond)

	pid := d.host.PluginPIDs()["repair"]
	if pid <= 0 {
		t.Fatalf("no pid recorded for repair: %v", d.host.PluginPIDs())
	}

	// Force the incident state: cancel the workflow on the still-alive plugin.
	svcs := d.host.GetWorkflowServices()
	if len(svcs) != 1 {
		t.Fatalf("GetWorkflowServices len = %d, want 1", len(svcs))
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := svcs[0].CancelWorkflow(cancelCtx, ""); err != nil {
		t.Fatalf("CancelWorkflow: %v", err)
	}
	// The workflow was cancelled on a still-alive plugin; a live watchdog tick may
	// already have re-armed it to RUNNING before this read (that heal is exactly
	// what we prove below), so accept CANCELLED (zombie established) or RUNNING
	// (already healed) — anything else means the cancel/heal path misbehaved.
	if got := repairWorkflowStatus(t, d); got != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED &&
		got != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Fatalf("after CancelWorkflow status = %s, want CANCELLED or already-healed RUNNING", got)
	}

	// The watchdog must re-arm it to RUNNING within a few health ticks.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if repairWorkflowStatus(t, d) == bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
			// Healed in place: the process must NOT have been restarted.
			if got := d.host.PluginPIDs()["repair"]; got != pid {
				t.Fatalf("watchdog restarted the process (pid %d -> %d); it should heal in place", pid, got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("watchdog did not re-arm the CANCELLED workflow to RUNNING (status=%s)", repairWorkflowStatus(t, d))
}
