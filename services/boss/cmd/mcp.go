package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/recurser/boss/internal/daemon"
	"github.com/spf13/cobra"
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
	st, err := daemon.McpGetStatus()
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
		fmt.Println(mcpBuildDriftLine(fetchRunningMcpBuildInfo(daemon.DefaultMcpPort), fetchOnDiskMcpBuildInfo()))
	default:
		fmt.Println("MCP server is installed but not running.")
	}
	if st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	return nil
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

func runMcpStop(_ *cobra.Command) error {
	if err := daemon.McpStop(); err != nil {
		return fmt.Errorf("stop mcp failed: %w", err)
	}
	fmt.Println("MCP server stopped.")
	return nil
}
