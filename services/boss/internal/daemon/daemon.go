// Package daemon manages the bossd daemon lifecycle across platforms.
// This file contains platform-independent types and functions.
package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// LifecycleStartupTimeout bounds waits for bossd to finish startup work and
	// begin accepting socket connections.
	LifecycleStartupTimeout = 30 * time.Second
	// LifecycleShutdownTimeout covers bossd's graceful shutdown path, including
	// cron drain and server shutdown, before the socket is removed.
	LifecycleShutdownTimeout = 20 * time.Second
	// LifecyclePollInterval is the shared cadence for daemon socket lifecycle
	// probes.
	LifecyclePollInterval = 100 * time.Millisecond
)

// Status represents the daemon's current state.
type Status struct {
	Installed   bool   // Whether daemon is registered with the system
	Running     bool   // Whether daemon process is currently running
	PID         int    // Process ID (if running)
	ServicePath string // plist path on macOS, unit path on Linux
}

// Install registers the daemon with the system service manager.
// bossdPath is the absolute path to the bossd binary. If force is false and
// the service file already exists, Install returns an error to avoid
// overwriting an existing installation.
func Install(bossdPath string, force bool) error {
	if err := validatePath(bossdPath); err != nil {
		return err
	}
	return platformInstall(bossdPath, force)
}

// skipLaunchctl reports whether service-manager invocations (launchctl on
// macOS, systemctl on Linux) should be skipped. Set via the
// BOSS_DAEMON_SKIP_LAUNCHCTL env var so tests can exercise file-writing
// behaviour without touching the host's service manager.
func skipLaunchctl() bool {
	return os.Getenv("BOSS_DAEMON_SKIP_LAUNCHCTL") != ""
}

// validatePath checks that a path is safe to use in service templates.
// Prevents template injection via newlines or other control characters.
func validatePath(p string) error {
	if strings.ContainsAny(p, "\n\r\x00") {
		return fmt.Errorf("path contains invalid characters: %q", p)
	}
	return nil
}

// Uninstall removes the daemon from the system service manager.
func Uninstall() error {
	return platformUninstall()
}

// Restart restarts the installed daemon through the platform service manager.
func Restart() error {
	return platformRestart()
}

// Stop asks the platform service manager to terminate the running daemon.
// It is safe to call when the daemon is already stopped — the platform
// implementation swallows "not loaded" errors.
func Stop() error {
	return platformStop()
}

// GetStatus returns the current daemon status.
func GetStatus() (*Status, error) {
	return platformGetStatus()
}

// McpInstall registers the local MCP server with the system service manager so
// it auto-starts and serves Streamable HTTP on 127.0.0.1:port. mcpBinPath is
// the absolute path to the mcp binary.
func McpInstall(mcpBinPath string, port int, force bool) error {
	if err := validatePath(mcpBinPath); err != nil {
		return err
	}
	return platformMcpInstall(mcpBinPath, port, force)
}

// McpUninstall removes the MCP server from the system service manager.
func McpUninstall() error {
	return platformMcpUninstall()
}

// McpStart starts (or restarts) the installed MCP LaunchAgent/service.
func McpStart() error {
	return platformMcpStart()
}

// McpStop stops the running MCP server, leaving its service file in place.
func McpStop() error {
	return platformMcpStop()
}

// McpGetStatus returns the current MCP server status.
func McpGetStatus() (*Status, error) {
	return platformMcpGetStatus()
}

// EnsureRunning checks if the daemon socket is reachable. If not, it attempts
// to start bossd and waits for the socket to become available.
func EnsureRunning(socketPath string) error {
	// Try to connect to the existing socket.
	if isSocketReachable(socketPath) {
		return nil
	}

	return platformEnsureRunning(socketPath)
}

// ResolveBossdPath finds the bossd binary. It checks:
// 1. Next to the boss binary (same directory)
// 2. In $PATH
func ResolveBossdPath() (string, error) {
	// Check next to the current executable.
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		candidate := filepath.Join(exeDir, "bossd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	// Check $PATH.
	path, err := exec.LookPath("bossd")
	if err == nil {
		return filepath.Abs(path)
	}

	return "", fmt.Errorf("bossd not found (install it next to boss or add it to PATH)")
}

// ResolveMcpPath finds the MCP binary, preferring the distributed name
// "boss-mcp" over the local-dev name "mcp". It checks next to the current
// executable first, then $PATH.
func ResolveMcpPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		for _, name := range []string{"boss-mcp", "mcp"} {
			candidate := filepath.Join(filepath.Dir(exe), name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}

	for _, name := range []string{"boss-mcp", "mcp"} {
		if path, err := exec.LookPath(name); err == nil {
			return filepath.Abs(path)
		}
	}

	return "", fmt.Errorf("mcp not found (install boss-mcp or mcp next to boss or add it to PATH)")
}

// isSocketReachable checks if a Unix socket is connectable.
func isSocketReachable(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// IsSocketReachable is the exported probe used by callers (e.g. the TUI's
// daemon-wait screen) that need to poll for the daemon coming back online
// without triggering platformEnsureRunning's launchctl/systemd dance.
func IsSocketReachable(socketPath string) bool {
	return isSocketReachable(socketPath)
}

// waitForSocket polls for the socket to become reachable.
func waitForSocket(socketPath string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(LifecyclePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if isSocketReachable(socketPath) {
				return true
			}
		}
	}
}
