package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/boss/internal/daemon"
	"github.com/spf13/cobra"
)

// Test seams, following the daemonGetStatus/daemonStop/... convention
// established in handlers.go: route runMcpStop and runMcpStatus through
// package vars so tests can stub daemon/process interactions without
// touching the real service manager or spawning real processes.
var (
	mcpGetStatus     = daemon.McpGetStatus
	mcpStop          = daemon.McpStop
	mcpResolvePath   = daemon.ResolveMcpPath
	mcpFindInstances = daemon.FindMcpInstances
	mcpStopInstances = daemon.StopMcpInstances

	// mcpFetchRunningBuildInfo / mcpFetchOnDiskBuildInfo let tests avoid the
	// real HTTP/exec calls (and their 2s timeouts) that
	// fetchRunningMcpBuildInfo and fetchOnDiskMcpBuildInfo make when `boss mcp
	// status` reports the service as running.
	mcpFetchRunningBuildInfo = fetchRunningMcpBuildInfo
	mcpFetchOnDiskBuildInfo  = fetchOnDiskMcpBuildInfo
)

// mcpCmd builds the `boss mcp` command tree, which manages the local MCP server
// as an auto-starting service via the platform service manager (launchd on
// macOS, systemd on Linux).
func mcpCmd() *cobra.Command {
	m := &cobra.Command{
		Use:   "mcp",
		Short: "Manage the local MCP server",
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install the MCP server as an auto-starting service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMcpInstall(cmd)
		},
	}
	install.Flags().Bool("force", false, "Overwrite existing service file")
	install.Flags().Int("port", daemon.DefaultMcpPort, "Loopback port for the MCP HTTP server")

	m.AddCommand(
		install,
		&cobra.Command{
			Use:   "uninstall",
			Short: "Uninstall the MCP server service",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runMcpUninstall(cmd)
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show MCP server status",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runMcpStatus(cmd)
			},
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start the MCP server",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runMcpStart(cmd)
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop the MCP server",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runMcpStop(cmd)
			},
		},
	)

	return m
}

func runMcpInstall(cmd *cobra.Command) error {
	mcpPath, err := daemon.ResolveMcpPath()
	if err != nil {
		return err
	}

	port, _ := cmd.Flags().GetInt("port")
	force, _ := cmd.Flags().GetBool("force")
	if err := daemon.McpInstall(mcpPath, port, force); err != nil {
		return fmt.Errorf("install mcp failed: %w", err)
	}

	st, _ := daemon.McpGetStatus()
	fmt.Println("MCP server installed and started.")
	fmt.Printf("  mcp:     %s\n", mcpPath)
	fmt.Printf("  address: http://127.0.0.1:%d/mcp\n", port)
	if st != nil && st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	return nil
}

func runMcpUninstall(_ *cobra.Command) error {
	if err := daemon.McpUninstall(); err != nil {
		return fmt.Errorf("uninstall mcp failed: %w", err)
	}
	fmt.Println("MCP server uninstalled.")
	return nil
}

func runMcpStatus(_ *cobra.Command) error {
	st, err := mcpGetStatus()
	if err != nil {
		return fmt.Errorf("mcp status: %w", err)
	}

	switch {
	case !st.Installed:
		fmt.Println("MCP server is not installed.")
		fmt.Println("  Run 'boss mcp install' to set it up.")
	case st.Running:
		fmt.Println("MCP server is running.")
		if st.PID > 0 {
			fmt.Printf("  PID:     %d\n", st.PID)
		}
		fmt.Println(mcpBuildDriftLine(mcpFetchRunningBuildInfo(daemon.DefaultMcpPort), mcpFetchOnDiskBuildInfo()))
	default:
		fmt.Println("MCP server is installed but not running.")
	}
	if st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	fmt.Println(mcpInstancesLine(st.Installed, st.PID))
	return nil
}

// mcpInstancesLine builds the `  instances: …` status line from a fresh
// process inventory (BOS-627), so `boss mcp status` reports every boss-mcp
// process a user would actually see in `ps` -- not just the single
// service-manager-owned instance the rest of this function describes.
func mcpInstancesLine(installed bool, servicePID int) string {
	mcpPath, err := mcpResolvePath()
	if err != nil {
		return fmt.Sprintf("  instances: unavailable (could not resolve the boss-mcp binary path: %s)", err)
	}
	inv, err := mcpFindInstances(mcpPath, daemon.McpServiceRef{Installed: installed, PID: servicePID})
	if err != nil {
		return fmt.Sprintf("  instances: unavailable (could not read the process inventory: %s)", err)
	}

	serviceCount := 0
	if inv.Service != nil {
		serviceCount = 1
	}
	sessionOwned := len(inv.StdioLive) + len(inv.StdioOrphan)
	line := fmt.Sprintf("  instances: %d service, %d stray HTTP, %d session-owned (%d orphaned)",
		serviceCount, len(inv.StrayHTTP), sessionOwned, len(inv.StdioOrphan))
	if len(inv.UnattributableHTTP) > 0 {
		// BOS-627 I1: these rows are neither confirmed stray nor session-owned
		// -- the service is installed but its PID is unknown, so one of them
		// may be the managed instance. Say that plainly rather than folding
		// them into "stray" (which would claim they were reaped) or
		// "session-owned" (which would claim they exit with a chat).
		line += fmt.Sprintf(", %d unattributable HTTP (installed service PID unknown; left alone, PIDs: %s)",
			len(inv.UnattributableHTTP), mcpProcessPIDs(inv.UnattributableHTTP))
	}
	return line
}

// mcpBuildInfo mirrors serve.BuildInfo (the /buildinfo JSON body). The type is
// duplicated rather than imported: services/boss must not depend on the mcp
// service's internal packages.
type mcpBuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Summary string `json:"summary"`
}

// mcpBuildDriftLine formats the running-vs-on-disk buildinfo comparison for
// `boss mcp status`, naming drift when the running service is built from a
// different binary than the one now on disk. An empty side means that build
// could not be determined (service unreachable, or binary not found).
func mcpBuildDriftLine(running, onDisk string) string {
	switch {
	case running == "" && onDisk == "":
		return "  build:   unavailable (could not read running or on-disk build info)"
	case running == "":
		return fmt.Sprintf("  build:   on-disk %s (running build unavailable)", onDisk)
	case onDisk == "":
		return fmt.Sprintf("  build:   running %s (on-disk build unavailable)", running)
	case running == onDisk:
		return fmt.Sprintf("  build:   %s (running matches on-disk)", running)
	default:
		return fmt.Sprintf("  build:   ⚠ drift — running %s but on-disk %s; restart the MCP service to load the rebuilt binary", running, onDisk)
	}
}

// fetchRunningMcpBuildInfo reads the running HTTP service's build metadata from
// its loopback /buildinfo endpoint. Best-effort: any failure yields "".
func fetchRunningMcpBuildInfo(port int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/buildinfo", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var bi mcpBuildInfo
	if err := json.NewDecoder(resp.Body).Decode(&bi); err != nil {
		return ""
	}
	return strings.TrimSpace(bi.Summary)
}

// fetchOnDiskMcpBuildInfo runs the on-disk mcp binary with --version to read its
// compiled-in build metadata. Best-effort: any failure yields "".
func fetchOnDiskMcpBuildInfo() string {
	mcpPath, err := daemon.ResolveMcpPath()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// #nosec G204 -- <mcpPath> --version on a resolved local binary (daemon.ResolveMcpPath, next-to-boss/PATH); literal args, local-trust.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.CommandContext(ctx, mcpPath, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runMcpStart(_ *cobra.Command) error {
	if err := daemon.McpStart(); err != nil {
		return fmt.Errorf("start mcp failed: %w", err)
	}
	fmt.Println("MCP server started.")
	return nil
}

// runMcpStop stops the MCP server (BOS-627). It is status-gated -- the
// service manager is invoked only when the service is actually installed
// (Running is deliberately not part of the gate; see below) -- and it also
// sweeps for stray/orphaned boss-mcp processes the
// service manager never knew about, since bossd writes a per-chat stdio
// boss-mcp server into each agent's MCP config (see
// services/bossd/internal/session/mcp_config.go) and those can outlive their
// chat, or even the managed service, as orphans. Modeled on runDaemonStop
// (handlers.go).
func runMcpStop(_ *cobra.Command) error {
	st, err := mcpGetStatus()
	if err != nil {
		return fmt.Errorf("mcp stop: %w", err)
	}

	// Capture the service's PID before stopping it: once the service exits,
	// its PID vanishes from `ps`, but this is exactly the value that must be
	// excluded from the sweep below (FindMcpInstances) so a KeepAlive-managed
	// instance is never mistaken for a stray and SIGTERMed (which would just
	// respawn it).
	servicePID := st.PID

	printedSomething := false
	// BOS-627 I1: gate on Installed alone, not Installed && Running. Both
	// platformMcpStop implementations are idempotent and verified-end-state,
	// so calling stop on an installed-but-reported-stopped service costs
	// nothing -- and skipping it is unsafe: the status probe can under-report
	// Running (a Linux unit mid-`activating`/`reloading` reports
	// Installed=true, Running=false) while the managed, Restart=always process
	// is genuinely alive.
	//
	// Known gap this gate does NOT cover, on BOTH platforms: if the plist/unit
	// file is deleted while the launchd job or systemd unit is still loaded,
	// McpGetStatus stats the service file first and so reports Installed=false
	// (launchd.go platformMcpGetStatus / systemd.go platformMcpGetStatus are
	// identical here). The service-manager stop is therefore skipped -- the
	// label-form `launchctl bootout` on macOS, `systemctl --user stop` on Linux
	// -- AND FindMcpInstances sees the managed --http row as stray rather than
	// unattributable (that class keys on Installed && PID==0), so the sweep
	// SIGTERMs it and KeepAlive/Restart=always respawns it under a new PID.
	// Closing this would mean probing the service manager by label
	// independently of the service file -- a platform-specific capability
	// beyond BOS-627's scope. Re-creating the service file and re-loading it
	// (`boss mcp install --force`, then `boss mcp start`) restores the managed
	// stop path. Documented in docs/mcp.md and the MCP guide.
	if st.Installed {
		if err := mcpStop(); err != nil {
			return fmt.Errorf("stop mcp failed: %w", err)
		}
		fmt.Println("MCP server stopped.")
		printedSomething = true
	}

	mcpPath, pathErr := mcpResolvePath()
	if pathErr != nil {
		// The argv[0] anchor against the resolved binary path is the only
		// thing preventing a mis-kill of e.g. a tmux session leader or a
		// codex process whose command line merely mentions boss-mcp -- so
		// without it, do not sweep, and do not fall back to a looser match.
		fmt.Printf("Stray/orphan MCP process sweep skipped: could not resolve the boss-mcp binary path: %s\n", pathErr)
		return nil
	}

	inv, invErr := mcpFindInstances(mcpPath, daemon.McpServiceRef{Installed: st.Installed, PID: servicePID})
	if invErr != nil {
		fmt.Printf("Could not read the MCP process inventory, so the stray/orphan sweep was skipped: %s\n", invErr)
		return nil
	}

	_, survivors, stopErr := mcpStopInstances(inv)

	// BOS-627 I2: report what actually stopped, not the target counts.
	// mcpStopInstances' own stopped count doesn't distinguish "we terminated
	// it" from "it was already gone (ESRCH)" -- either way it is true that
	// the process is no longer among the survivors, so subtracting survivor
	// PIDs per class is honest under both. A survivor is attributable to its
	// class from the inventory, so a fully-surviving sweep prints no
	// "Stopped …"/"Reaped …" line at all, instead of contradicting the
	// "did not stop in time" warning below.
	survivorPIDs := make(map[int]bool, len(survivors))
	for _, pid := range survivors {
		survivorPIDs[pid] = true
	}
	strayStopped := 0
	for _, p := range inv.StrayHTTP {
		if !survivorPIDs[p.PID] {
			strayStopped++
		}
	}
	orphanStopped := 0
	for _, p := range inv.StdioOrphan {
		if !survivorPIDs[p.PID] {
			orphanStopped++
		}
	}

	if strayStopped > 0 {
		fmt.Printf("Stopped %d stray MCP HTTP daemon(s).\n", strayStopped)
		printedSomething = true
	}
	if orphanStopped > 0 {
		fmt.Printf("Reaped %d orphaned session MCP server(s).\n", orphanStopped)
		printedSomething = true
	}
	if len(inv.StdioLive) > 0 {
		fmt.Printf("%d session-owned MCP server(s) left running; each exits with its chat (PIDs: %s).\n",
			len(inv.StdioLive), mcpProcessPIDs(inv.StdioLive))
		printedSomething = true
	}
	if len(inv.UnattributableHTTP) > 0 {
		// BOS-627 I1: never signalled -- the service is installed but its PID
		// is unknown, so these candidate-stray daemons could not be
		// distinguished from the managed instance and were left alone.
		fmt.Printf("%d MCP HTTP process(es) left running because they could not be distinguished from the installed service (PID unknown, PIDs: %s).\n",
			len(inv.UnattributableHTTP), mcpProcessPIDs(inv.UnattributableHTTP))
		printedSomething = true
	}
	if len(survivors) > 0 {
		fmt.Printf("Warning: %d MCP process(es) did not stop in time (PIDs: %s).\n", len(survivors), mcpIntPIDs(survivors))
		printedSomething = true
	}

	// M6: check stopErr before the early return so a signal error on a
	// class that reported nothing printed (e.g. every target was already
	// gone via ESRCH except for a real signal failure) is never silently
	// swallowed by "No MCP server instances are running."
	if stopErr != nil {
		return fmt.Errorf("stop mcp failed: %w", stopErr)
	}
	if !printedSomething {
		fmt.Println("No MCP server instances are running.")
	}
	return nil
}

// mcpProcessPIDs renders a comma-separated PID list from McpProcess rows, for
// output lines that name the specific processes involved. M8: delegates to
// mcpIntPIDs rather than duplicating the join logic.
func mcpProcessPIDs(procs []daemon.McpProcess) string {
	pids := make([]int, len(procs))
	for i, p := range procs {
		pids[i] = p.PID
	}
	return mcpIntPIDs(pids)
}

// mcpIntPIDs renders a comma-separated PID list from raw pids (e.g.
// StopMcpInstances' survivors).
func mcpIntPIDs(pids []int) string {
	strs := make([]string, len(pids))
	for i, pid := range pids {
		strs[i] = strconv.Itoa(pid)
	}
	return strings.Join(strs, ", ")
}
