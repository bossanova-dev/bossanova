package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/daemon"
)

// saveMcpSeams snapshots every mcp.go package-var seam and restores it after
// the test, per the package convention (see saveDaemonSeams-style helpers in
// handlers_test.go). Tests using it must not call t.Parallel(): the seams are
// shared mutable package state.
func saveMcpSeams(t *testing.T) {
	t.Helper()
	oldGetStatus := mcpGetStatus
	oldStop := mcpStop
	oldResolvePath := mcpResolvePath
	oldFindInstances := mcpFindInstances
	oldStopInstances := mcpStopInstances
	oldFetchRunning := mcpFetchRunningBuildInfo
	oldFetchOnDisk := mcpFetchOnDiskBuildInfo
	t.Cleanup(func() {
		mcpGetStatus = oldGetStatus
		mcpStop = oldStop
		mcpResolvePath = oldResolvePath
		mcpFindInstances = oldFindInstances
		mcpStopInstances = oldStopInstances
		mcpFetchRunningBuildInfo = oldFetchRunning
		mcpFetchOnDiskBuildInfo = oldFetchOnDisk
	})
}

func TestRunMcpStop_NotInstalledNothingRunning(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpStop = func() error {
		t.Fatal("mcpStop must not be called when the service is not installed")
		return nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		return 0, nil, nil
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runMcpStop(nil)
	})

	if runErr != nil {
		t.Fatalf("runMcpStop returned %v, want nil", runErr)
	}
	const want = "No MCP server instances are running.\n"
	if out != want {
		t.Errorf("output = %q, want exactly %q", out, want)
	}
}

func TestRunMcpStop_InstalledRunning(t *testing.T) {
	saveMcpSeams(t)

	stopCalls := 0
	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	mcpStop = func() error {
		stopCalls++
		return nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, service daemon.McpServiceRef) (daemon.McpInventory, error) {
		if service.PID != 4242 {
			t.Errorf("service.PID = %d, want 4242 (captured before mcpStop)", service.PID)
		}
		return daemon.McpInventory{}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		return 0, nil, nil
	}

	out := captureStdout(t, func() {
		if err := runMcpStop(nil); err != nil {
			t.Fatalf("runMcpStop: %v", err)
		}
	})

	if stopCalls != 1 {
		t.Errorf("mcpStop called %d times, want 1", stopCalls)
	}
	if !strings.Contains(out, "MCP server stopped.") {
		t.Errorf("output = %q, want to contain %q", out, "MCP server stopped.")
	}
}

// TestRunMcpStop_InstalledNotRunning_StillStopsService is BOS-627 finding I1's
// regression test for the gate half of the bug: platformMcpStop is idempotent
// and verified-end-state, so the service-manager stop must run whenever the
// service is installed, even when the status probe under-reports Running
// (Linux `activating`/`reloading`, or a macOS plist removed out from under a
// still-loaded job). Gating on `Installed && Running` skips the stop exactly
// when the probe is wrong, leaving a live KeepAlive/Restart=always process
// unstopped.
func TestRunMcpStop_InstalledNotRunning_StillStopsService(t *testing.T) {
	saveMcpSeams(t)

	stopCalls := 0
	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: false, PID: 0}, nil
	}
	mcpStop = func() error {
		stopCalls++
		return nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		return 0, nil, nil
	}

	out := captureStdout(t, func() {
		if err := runMcpStop(nil); err != nil {
			t.Fatalf("runMcpStop: %v", err)
		}
	})

	if stopCalls != 1 {
		t.Errorf("mcpStop called %d times, want 1 (installed service must be stopped even when the probe under-reports Running)", stopCalls)
	}
	if !strings.Contains(out, "MCP server stopped.") {
		t.Errorf("output = %q, want to contain %q", out, "MCP server stopped.")
	}
}

func TestRunMcpStop_StraysAndOrphansOnly(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpStop = func() error {
		t.Fatal("mcpStop must not be called when the service is not installed")
		return nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{
			StrayHTTP:   []daemon.McpProcess{{PID: 111}, {PID: 112}},
			StdioOrphan: []daemon.McpProcess{{PID: 201}, {PID: 202}, {PID: 203}},
		}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		return 5, nil, nil
	}

	out := captureStdout(t, func() {
		if err := runMcpStop(nil); err != nil {
			t.Fatalf("runMcpStop: %v", err)
		}
	})

	if !strings.Contains(out, "Stopped 2 stray MCP HTTP daemon(s).") {
		t.Errorf("output = %q, missing stray-HTTP count line", out)
	}
	if !strings.Contains(out, "Reaped 3 orphaned session MCP server(s).") {
		t.Errorf("output = %q, missing orphan count line", out)
	}
}

func TestRunMcpStop_LiveStdioPresent(t *testing.T) {
	saveMcpSeams(t)

	const servicePID = 999
	const liveStdioPID = 777

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: servicePID}, nil
	}
	mcpStop = func() error { return nil }
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	// M4: the real guard that StdioLive/service PIDs are never handed to
	// StopMcpInstances as targets is TestStopMcpInstances_SignalsOnlyStrayHTTPAndStdioOrphan
	// in mcpprocess_test.go. This test only needs to check what runMcpStop
	// itself decides to print about a live stdio instance.
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{
			Service:     &daemon.McpProcess{PID: servicePID},
			StrayHTTP:   []daemon.McpProcess{{PID: 501}},
			StdioOrphan: []daemon.McpProcess{{PID: 502}},
			StdioLive:   []daemon.McpProcess{{PID: liveStdioPID}},
		}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		return 2, nil, nil
	}

	out := captureStdout(t, func() {
		if err := runMcpStop(nil); err != nil {
			t.Fatalf("runMcpStop: %v", err)
		}
	})

	if !strings.Contains(out, "session-owned") {
		t.Errorf("output = %q, want to contain %q", out, "session-owned")
	}
	if !strings.Contains(out, fmt.Sprintf("%d", liveStdioPID)) {
		t.Errorf("output = %q, want to name the live stdio PID %d so ps still showing it is explained", out, liveStdioPID)
	}
}

func TestRunMcpStop_ResolvePathFails(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpStop = func() error {
		t.Fatal("mcpStop must not be called")
		return nil
	}
	mcpResolvePath = func() (string, error) { return "", errors.New("boss-mcp binary not found") }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		t.Fatal("mcpFindInstances must not be called when the path cannot be resolved")
		return daemon.McpInventory{}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		t.Fatal("mcpStopInstances must not be called when the sweep is skipped")
		return 0, nil, nil
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runMcpStop(nil)
	})

	if runErr != nil {
		t.Fatalf("runMcpStop returned %v, want nil", runErr)
	}
	if !strings.Contains(out, "boss-mcp binary not found") {
		t.Errorf("output = %q, want to explain the skip reason", out)
	}
	if strings.Contains(out, "No MCP server instances are running.") {
		t.Errorf("output = %q, must not claim nothing is running when the sweep was skipped", out)
	}
}

func TestRunMcpStop_FindInstancesFails(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpStop = func() error {
		t.Fatal("mcpStop must not be called")
		return nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{}, errors.New("ps snapshot: exec failed")
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		t.Fatal("mcpStopInstances must not be called when the inventory could not be read")
		return 0, nil, nil
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runMcpStop(nil)
	})

	if runErr != nil {
		t.Fatalf("runMcpStop returned %v, want nil", runErr)
	}
	if !strings.Contains(out, "ps snapshot: exec failed") {
		t.Errorf("output = %q, want to explain the inventory read failure", out)
	}
	if strings.Contains(out, "No MCP server instances are running.") {
		t.Errorf("output = %q, must not claim nothing was running when the probe failed", out)
	}
}

func TestRunMcpStop_SurvivorsReturned(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpStop = func() error {
		t.Fatal("mcpStop must not be called")
		return nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{StrayHTTP: []daemon.McpProcess{{PID: 4321}}}, nil
	}
	mcpStopInstances = func(_ daemon.McpInventory) (int, []int, error) {
		return 0, []int{4321}, nil
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runMcpStop(nil)
	})

	if runErr != nil {
		t.Fatalf("runMcpStop returned %v, want nil (a survivor list alone is a warning, not a failure)", runErr)
	}
	if !strings.Contains(out, "4321") {
		t.Errorf("output = %q, want to name the survivor PID 4321", out)
	}
	if !strings.Contains(out, "did not stop in time") {
		t.Errorf("output = %q, want the survivor warning", out)
	}
	// BOS-627 I2: the sole StrayHTTP process is also the sole survivor, so
	// honest counting must report zero stopped -- printing "Stopped 1 stray"
	// here would contradict the survivor warning about the very same PID.
	if strings.Contains(out, "Stopped 1 stray") {
		t.Errorf("output = %q, must not claim a survivor was stopped", out)
	}
}

func TestRunMcpStatus_InstancesLine(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpResolvePath = func() (string, error) { return "/opt/boss-mcp", nil }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		return daemon.McpInventory{
			Service:     &daemon.McpProcess{PID: 1},
			StrayHTTP:   []daemon.McpProcess{{PID: 2}, {PID: 3}},
			StdioOrphan: []daemon.McpProcess{{PID: 4}, {PID: 5}, {PID: 6}},
			StdioLive:   make([]daemon.McpProcess, 6),
		}, nil
	}

	out := captureStdout(t, func() {
		if err := runMcpStatus(nil); err != nil {
			t.Fatalf("runMcpStatus: %v", err)
		}
	})

	const want = "  instances: 1 service, 2 stray HTTP, 9 session-owned (3 orphaned)"
	if !strings.Contains(out, want) {
		t.Errorf("output = %q, want to contain %q", out, want)
	}
}

func TestRunMcpStatus_InstancesLineUnavailable(t *testing.T) {
	saveMcpSeams(t)

	mcpGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	mcpResolvePath = func() (string, error) { return "", errors.New("boss-mcp binary not found") }
	mcpFindInstances = func(_ string, _ daemon.McpServiceRef) (daemon.McpInventory, error) {
		t.Fatal("mcpFindInstances must not be called when the path cannot be resolved")
		return daemon.McpInventory{}, nil
	}

	out := captureStdout(t, func() {
		if err := runMcpStatus(nil); err != nil {
			t.Fatalf("runMcpStatus: %v", err)
		}
	})

	if !strings.Contains(out, "instances: unavailable") {
		t.Errorf("output = %q, want an unavailable instances line", out)
	}
	if !strings.Contains(out, "boss-mcp binary not found") {
		t.Errorf("output = %q, want to explain why", out)
	}
}

func TestMcpBuildDriftLine(t *testing.T) {
	t.Parallel()

	const running = "v1.2.3 (aaa) built 2026-07-01"
	const onDisk = "v1.2.4 (bbb) built 2026-07-04"

	cases := []struct {
		name        string
		running     string
		onDisk      string
		wantSubstrs []string
		wantNoDrift bool // must NOT contain the drift marker
	}{
		{
			name:        "match",
			running:     running,
			onDisk:      running,
			wantSubstrs: []string{running, "running matches on-disk"},
			wantNoDrift: true,
		},
		{
			name:        "drift",
			running:     running,
			onDisk:      onDisk,
			wantSubstrs: []string{"drift", running, onDisk, "restart the MCP service"},
		},
		{
			name:        "running unavailable",
			running:     "",
			onDisk:      onDisk,
			wantSubstrs: []string{onDisk, "running build unavailable"},
			wantNoDrift: true,
		},
		{
			name:        "on-disk unavailable",
			running:     running,
			onDisk:      "",
			wantSubstrs: []string{running, "on-disk build unavailable"},
			wantNoDrift: true,
		},
		{
			name:        "both unavailable",
			running:     "",
			onDisk:      "",
			wantSubstrs: []string{"unavailable"},
			wantNoDrift: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpBuildDriftLine(tc.running, tc.onDisk)
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("mcpBuildDriftLine(%q, %q) = %q; missing %q", tc.running, tc.onDisk, got, want)
				}
			}
			if tc.wantNoDrift && strings.Contains(got, "⚠ drift") {
				t.Errorf("mcpBuildDriftLine(%q, %q) = %q; must not report drift", tc.running, tc.onDisk, got)
			}
		})
	}
}
