package main

import (
	"fmt"

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
	default:
		fmt.Println("MCP server is installed but not running.")
	}
	if st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	return nil
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
