package mcp

import (
	"context"
	"io/fs"
	"net"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/contract/plugin/skillinstall"
)

// Config supplies shared CLI behavior without coupling command groups to the composition root.
type Config struct {
	SkillPayload        func() (fs.FS, skillinstall.PayloadRecord, error)
	ShellQuote          func(string) string
	InstallSettingsLine func() string
}

// Group owns the MCP and Hermes command dependencies.
type Group struct {
	config                   Config
	mcpSocketPath            func() (string, error)
	mcpGetStatus             func() (*daemon.Status, error)
	mcpStop                  func() error
	mcpStart                 func() error
	mcpResolvePath           func() (string, error)
	mcpFindInstances         func(string, daemon.McpServiceRef) (daemon.McpInventory, error)
	mcpStopInstances         func(daemon.McpInventory) (int, []int, error)
	mcpFetchRunningBuildInfo func(int) string
	mcpFetchOnDiskBuildInfo  func() string
	hermesLookPath           func(string) (string, error)
	hermesRun                hermesRunner
	hermesResolveMcpPath     func() (string, error)
	hermesDaemonStatus       func() (*daemon.Status, error)
	hermesSocketReachable    func(string) bool
	hermesToolDefinitions    func(context.Context, bossmcp.Options) ([]*mcp.Tool, error)
}

// New constructs independent command trees with production host dependencies.
func New(cfg Config) *Group {
	return &Group{
		config:        cfg,
		mcpSocketPath: config.ResolveSocketPath,
		mcpGetStatus:  daemon.McpGetStatus, mcpStop: daemon.McpStop, mcpStart: daemon.McpStart,
		mcpResolvePath: daemon.ResolveMcpPath, mcpFindInstances: daemon.FindMcpInstances, mcpStopInstances: daemon.StopMcpInstances,
		mcpFetchRunningBuildInfo: fetchRunningMcpBuildInfo, mcpFetchOnDiskBuildInfo: fetchOnDiskMcpBuildInfo,
		hermesLookPath: exec.LookPath, hermesRun: runHermesCommand, hermesResolveMcpPath: daemon.ResolveMcpPath,
		hermesDaemonStatus: daemon.GetStatus, hermesToolDefinitions: bossmcp.ToolDefinitions,
		hermesSocketReachable: func(socketPath string) bool {
			conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		},
	}
}
