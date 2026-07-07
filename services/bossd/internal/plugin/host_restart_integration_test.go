package plugin_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// TestE2E_PluginRestart_RespawnsAfterSubprocessDeath reproduces the incident that
// motivated plugin supervision: a plugin subprocess is SIGTERM'd out from under
// the daemon. Before the fix the host merely logged "plugin process exited"
// every health tick and left the plugin dead until a full daemon restart. Now
// the health loop must relaunch it under a new subprocess and re-dispense its
// service interface.
func TestE2E_PluginRestart_RespawnsAfterSubprocessDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping plugin restart integration test in short mode")
	}

	// Drive the restart path on a fast tick. Set before Start (the interval is
	// read when the health loop's ticker is constructed) and restore at end.
	restore := pluginpkg.SetHealthCheckInterval(50 * time.Millisecond)
	defer restore()

	pluginDir := t.TempDir()
	binPath := pluginharness.BuildPluginInto(t, pluginDir, "bossd-plugin-repair")
	cfgs := []config.PluginConfig{{Name: "repair", Path: binPath, Enabled: true}}

	d := newDiscoveryHost(t)
	if err := d.host.Start(t.Context(), cfgs, config.Settings{}); err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// Baseline: repair loaded, workflow interface dispensed, capture its pid.
	if svcs := d.host.GetWorkflowServices(); len(svcs) != 1 {
		t.Fatalf("baseline GetWorkflowServices len = %d, want 1", len(svcs))
	}
	oldPID := d.host.PluginPIDs()["repair"]
	if oldPID <= 0 {
		t.Fatalf("no pid recorded for repair: %v", d.host.PluginPIDs())
	}

	// Kill the subprocess hard. SIGKILL (which go-plugin's Serve cannot catch)
	// forces the exact end state of the incident — a dead plugin subprocess and
	// a "connection refused" socket — regardless of how the death was caused
	// (external SIGTERM, crash, OOM).
	proc, err := os.FindProcess(oldPID)
	if err != nil {
		t.Fatalf("find repair process %d: %v", oldPID, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL repair (pid %d): %v", oldPID, err)
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
		t.Fatalf("repair was not relaunched under a new pid (still %d)", oldPID)
	}

	// The workflow interface must be re-dispensed over the new subprocess —
	// this is the attestation that GetWorkflowServices/GetTaskSources consumers
	// recover automatically after a respawn.
	svcs := d.host.GetWorkflowServices()
	if len(svcs) != 1 {
		t.Fatalf("after restart GetWorkflowServices len = %d, want 1", len(svcs))
	}
	info, err := svcs[0].GetInfo(ctx)
	if err != nil {
		t.Fatalf("after restart workflow GetInfo: %v", err)
	}
	if info.GetName() == "" {
		t.Error("after restart workflow GetInfo returned empty name")
	}
}
