//go:build linux

// Package daemon manages the bossd daemon lifecycle via systemd user service.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

var validUsernameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// isValidUsername checks that a username contains only safe characters.
func isValidUsername(name string) bool {
	return len(name) > 0 && len(name) <= 256 && validUsernameRe.MatchString(name)
}

// runSystemctl invokes systemctl and returns its combined output. It is a
// package var so tests can inject a fake without a real systemd user session.
var runSystemctl = func(args ...string) ([]byte, error) {
	// #nosec G204 -- systemctl; const argv verbs plus derived unit names; no shell
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	return exec.Command("systemctl", args...).CombinedOutput()
}

// mcpStopVerifyTimeout bounds the post-stop re-probe. systemctl can return
// before the unit has finished stopping, so a single read can still see it
// active; polling avoids reporting a spurious failure.
var mcpStopVerifyTimeout = 2 * time.Second

const (
	// ServiceName is the systemd unit name.
	ServiceName = "bossd.service"

	// McpServiceName is the systemd unit name for the local MCP server.
	McpServiceName = "bossanova-mcp.service"

	// McpLabel is a stable identifier for the MCP service across platforms.
	McpLabel = "bossanova-mcp"

	// DefaultMcpPort is the loopback port the MCP HTTP daemon listens on.
	DefaultMcpPort = 8765

	unitTemplate = `[Unit]
Description=Bossanova Daemon
After=network.target

[Service]
ExecStart={{.BossdPath}}
LimitNOFILE=65536
Restart=always
RestartSec=5
Environment=LC_CTYPE=C.UTF-8

[Install]
WantedBy=default.target
`

	// mcpUnitTemplate runs `mcp --http 127.0.0.1:<port>`. Its PATH includes the
	// agent-runner shim directories (~/.nodenv/shims, ~/.local/bin) so the MCP
	// server (and any agent CLI it shells out to) can find node/agent binaries.
	mcpUnitTemplate = `[Unit]
Description=Bossanova MCP Server
After=network.target

[Service]
ExecStart={{.McpPath}} --http {{.Addr}}
Restart=always
RestartSec=5
Environment=PATH={{.Path}}
Environment=LC_CTYPE=C.UTF-8

[Install]
WantedBy=default.target
`
)

type unitData struct {
	BossdPath string
}

type mcpUnitData struct {
	McpPath string
	Addr    string
	Path    string
}

// mcpPath builds the PATH for the MCP service, prepending the agent-runner shim
// directories (~/.nodenv/shims and ~/.local/bin) to the inherited PATH.
func mcpPath() string {
	entries := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		entries = append(entries,
			filepath.Join(home, ".nodenv", "shims"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	if p := os.Getenv("PATH"); p != "" {
		entries = append(entries, p)
	} else {
		entries = append(entries, "/usr/local/bin", "/usr/bin", "/bin")
	}
	return strings.Join(entries, ":")
}

// platformServicePath returns the path to the systemd user unit file.
func platformServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", ServiceName), nil
}

// generateUnit renders the systemd unit file for bossd.
func generateUnit(bossdPath string) (string, error) {
	tmpl, err := template.New("unit").Parse(unitTemplate)
	if err != nil {
		return "", fmt.Errorf("parse unit template: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, unitData{BossdPath: bossdPath}); err != nil {
		return "", fmt.Errorf("render unit: %w", err)
	}

	return buf.String(), nil
}

// platformInstall writes the systemd user unit file and enables/starts it.
// When force is false and the unit file already exists, it refuses to overwrite.
func platformInstall(bossdPath string, force bool) error {
	unitPath, err := platformServicePath()
	if err != nil {
		return err
	}

	if !force {
		if _, err := os.Stat(unitPath); err == nil {
			return fmt.Errorf("unit file already exists at %s (use --force to overwrite)", unitPath)
		}
	}

	// Check that systemctl is available before attempting install (skipped in test mode).
	if !skipLaunchctl() {
		if _, err := exec.LookPath("systemctl"); err != nil {
			return fmt.Errorf("systemctl not found: systemd is required for daemon management on Linux")
		}
	}

	unit, err := generateUnit(bossdPath)
	if err != nil {
		return err
	}

	// Ensure systemd user directory exists.
	// #nosec G301 -- ~/.config/systemd/user unit dir; 0o755 is the XDG/systemd-conventional mode for a non-secret user-config dir.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create systemd user dir: %w", err)
	}

	// Write the unit file.
	// #nosec G306 -- non-secret systemd user unit file (holds the bossd exec path, already visible via ps); 0o644 is the conventional unit-file mode.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	if skipLaunchctl() {
		return nil
	}

	// Reload systemd daemon.
	if out, err := runSystemctl("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Enable and start the service.
	if out, err := runSystemctl("--user", "enable", "--now", ServiceName); err != nil {
		return fmt.Errorf("systemctl enable --now: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Attempt to enable linger (allow service to run without user login).
	// Pass the current username explicitly for compatibility with older systemd.
	// This may fail if polkit is not available — warn but don't error.
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	lingerArgs := []string{"enable-linger"}
	if username != "" && isValidUsername(username) {
		lingerArgs = append(lingerArgs, username)
	}
	if out, err := exec.Command("loginctl", lingerArgs...).CombinedOutput(); err != nil { //nolint:gosec // args are validated above
		fmt.Fprintf(os.Stderr, "Warning: loginctl enable-linger failed (service may not start on boot): %v\n%s\n",
			err, strings.TrimSpace(string(out)))
	}

	return nil
}

// platformUninstall stops and disables the systemd user service, then removes the unit file.
func platformUninstall() error {
	unitPath, err := platformServicePath()
	if err != nil {
		return err
	}

	// Stop and disable the service (ignore errors if not running/enabled).
	if !skipLaunchctl() {
		_, _ = runSystemctl("--user", "disable", "--now", ServiceName)
	}

	// Remove the unit file.
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}

	// Reload systemd daemon.
	if !skipLaunchctl() {
		_, _ = runSystemctl("--user", "daemon-reload")
	}

	return nil
}

func platformRestart() error {
	if skipLaunchctl() {
		return nil
	}

	out, err := runSystemctl("--user", "restart", ServiceName)
	if err != nil {
		return fmt.Errorf("systemctl --user restart %s: %w: %s", ServiceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformStop tells systemd to stop the user-scoped bossd unit. `systemctl
// stop` is already idempotent (returns 0 when the unit is inactive), so no
// extra suppression is needed.
func platformStop() error {
	if skipLaunchctl() {
		return nil
	}

	out, err := runSystemctl("--user", "stop", ServiceName)
	if err != nil {
		return fmt.Errorf("systemctl --user stop %s: %w: %s", ServiceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformGetStatus returns the current daemon status via systemctl.
func platformGetStatus() (*Status, error) {
	unitPath, err := platformServicePath()
	if err != nil {
		return nil, err
	}

	st := &Status{ServicePath: unitPath}

	// Check if unit file exists.
	if _, err := os.Stat(unitPath); err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("check unit file: %w", err)
	}
	st.Installed = true

	if skipLaunchctl() {
		return st, nil
	}

	// Check if service is active.
	out, err := runSystemctl("--user", "is-active", ServiceName)
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		st.Running = true
	}

	// Get PID if running.
	if st.Running {
		out, err := runSystemctl("--user", "show", "--property=MainPID", ServiceName)
		if err == nil {
			// Output format: "MainPID=12345"
			line := strings.TrimSpace(string(out))
			if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
				_, _ = fmt.Sscanf(parts[1], "%d", &st.PID)
			}
		}
	}

	return st, nil
}

// mcpServicePath returns the path to the MCP systemd user unit file.
func mcpServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", McpServiceName), nil
}

// generateMcpUnit renders the systemd unit file for the local MCP server.
func generateMcpUnit(mcpBinPath string, port int) (string, error) {
	tmpl, err := template.New("mcpUnit").Parse(mcpUnitTemplate)
	if err != nil {
		return "", fmt.Errorf("parse mcp unit template: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, mcpUnitData{
		McpPath: mcpBinPath,
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Path:    mcpPath(),
	}); err != nil {
		return "", fmt.Errorf("render mcp unit: %w", err)
	}
	return buf.String(), nil
}

func platformMcpInstall(mcpBinPath string, port int, force bool) error {
	unitPath, err := mcpServicePath()
	if err != nil {
		return err
	}

	if !force {
		if _, err := os.Stat(unitPath); err == nil {
			return fmt.Errorf("unit file already exists at %s (use --force to overwrite)", unitPath)
		}
	}

	if !skipLaunchctl() {
		if _, err := exec.LookPath("systemctl"); err != nil {
			return fmt.Errorf("systemctl not found: systemd is required for service management on Linux")
		}
	}

	unit, err := generateMcpUnit(mcpBinPath, port)
	if err != nil {
		return err
	}

	// #nosec G301 -- ~/.config/systemd/user unit dir; 0o755 is the XDG/systemd-conventional mode for a non-secret user-config dir.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create systemd user dir: %w", err)
	}
	// #nosec G306 -- non-secret systemd user unit file (holds the mcp binary path); 0o644 is the conventional unit-file mode.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	if skipLaunchctl() {
		return nil
	}

	if out, err := runSystemctl("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runSystemctl("--user", "enable", "--now", McpServiceName); err != nil {
		return fmt.Errorf("systemctl enable --now: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func platformMcpUninstall() error {
	unitPath, err := mcpServicePath()
	if err != nil {
		return err
	}

	if !skipLaunchctl() {
		_, _ = runSystemctl("--user", "disable", "--now", McpServiceName)
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}
	if !skipLaunchctl() {
		_, _ = runSystemctl("--user", "daemon-reload")
	}
	return nil
}

func platformMcpStart() error {
	if skipLaunchctl() {
		return nil
	}
	out, err := runSystemctl("--user", "restart", McpServiceName)
	if err != nil {
		return fmt.Errorf("systemctl --user restart %s: %w: %s", McpServiceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mcpUnitActiveState returns the systemd ActiveState word `systemctl --user
// is-active` reported for the MCP unit, or "" when systemctl gave no
// recognizable state at all.
//
// The exit code cannot be used to tell "the unit is stopped" apart from "the
// probe itself failed": is-active exits NON-ZERO for every not-active state
// (3 for inactive/failed/activating). The reported STATE WORD can, though --
// systemd prints one of a known ActiveState set, whereas a probe failure (no
// user bus reachable, systemctl absent) prints a diagnostic instead. Because
// runSystemctl uses CombinedOutput, that diagnostic can arrive on either
// stream, so this matches against the known set rather than assuming the
// output is a state word.
func mcpUnitActiveState() string {
	out, _ := runSystemctl("--user", "is-active", McpServiceName)
	switch state := strings.TrimSpace(string(out)); state {
	case "active", "reloading", "activating", "deactivating", "inactive", "failed":
		return state
	}
	return ""
}

// isMcpUnitActive reports whether systemctl currently considers the MCP unit
// active. Used by platformMcpGetStatus, where an unreadable probe must not be
// reported as running.
func isMcpUnitActive() bool {
	return mcpUnitActiveState() == "active"
}

// mcpUnitStillRunning is platformMcpStop's post-stop verification probe. Unlike
// isMcpUnitActive it fails CLOSED, mirroring launchd's mcpStillRunningProbe
// (launchd.go): a state systemd did not report is "cannot tell", not "stopped",
// and accepting it as stopped would turn an unverifiable end state into a
// silent success after a `stop` that genuinely failed.
//
// `inactive` and `failed` are real stopped states and are reported as such, so
// the ordinary already-stopped case still verifies cleanly and the BOS-627 bug
// (`stop` erroring when there is nothing to stop) does not return on Linux.
func mcpUnitStillRunning() bool {
	switch mcpUnitActiveState() {
	case "inactive", "failed":
		return false
	default:
		// "" (unreadable probe), plus active/reloading/activating/deactivating.
		return true
	}
}

// platformMcpStop stops the MCP systemd user unit and treats a verified
// stopped end state as success, rather than trusting systemctl's exit code
// alone.
//
// BOS-627: the prior body returned an error for any non-zero `systemctl
// --user stop` exit, including the routine case where the unit was never
// installed (no unit file) — there being nothing to stop is not a failure,
// so that case now short-circuits before systemctl is invoked at all. When
// the unit is installed, `stop` is attempted; systemctl can report before
// the unit has actually finished stopping, so any error from `stop` is
// followed by polling the unit state (via mcpUnitStillRunning, which fails
// closed on an unreadable probe) for up to mcpStopVerifyTimeout before the
// failure is treated as real and reported.
func platformMcpStop() error {
	if skipLaunchctl() {
		return nil
	}

	if st, err := platformMcpGetStatus(); err == nil && !st.Installed {
		return nil
	}

	out, err := runSystemctl("--user", "stop", McpServiceName)
	if err == nil {
		return nil
	}

	deadline := time.Now().Add(mcpStopVerifyTimeout)
	for mcpUnitStillRunning() {
		if time.Now().After(deadline) {
			return fmt.Errorf("systemctl --user stop %s: %w: %s", McpServiceName, err, strings.TrimSpace(string(out)))
		}
		time.Sleep(LifecyclePollInterval)
	}
	return nil
}

func platformMcpGetStatus() (*Status, error) {
	unitPath, err := mcpServicePath()
	if err != nil {
		return nil, err
	}

	st := &Status{ServicePath: unitPath}

	if _, err := os.Stat(unitPath); err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("check unit file: %w", err)
	}
	st.Installed = true

	if skipLaunchctl() {
		return st, nil
	}

	st.Running = isMcpUnitActive()
	if st.Running {
		out, err := runSystemctl("--user", "show", "--property=MainPID", McpServiceName)
		if err == nil {
			line := strings.TrimSpace(string(out))
			if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
				_, _ = fmt.Sscanf(parts[1], "%d", &st.PID)
			}
		}
	}
	return st, nil
}

// platformEnsureRunning attempts to start the daemon if it's not reachable.
func platformEnsureRunning(socketPath string) error {
	// Re-probe: a daemon may have come up (or another caller started one) since
	// EnsureRunning's initial check. Spawning a duplicate bossd here is the root
	// cause of the socket-stealing storm, so never start one if the socket is
	// already being served.
	if isSocketReachable(socketPath) {
		return nil
	}

	// Try the systemd service first (if installed).
	st, err := platformGetStatus()
	if err == nil && st.Installed && !st.Running {
		if _, err := runSystemctl("--user", "start", ServiceName); err == nil {
			if waitForSocket(socketPath, LifecycleStartupTimeout) {
				return nil
			}
		}
	}

	// Fall back to starting bossd directly as a background process.
	bossdPath, err := ResolveBossdPath()
	if err != nil {
		return fmt.Errorf("cannot auto-start daemon because start failed: %w", err)
	}

	// Final guard before spawning: don't race a daemon that just came up.
	if isSocketReachable(socketPath) {
		return nil
	}

	// #nosec G204 -- self-spawn of the discovered bossd binary (bossdPath); literal args, no shell; local-trust, not attacker-controlled.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command(bossdPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bossd: %w", err)
	}

	// Release the child process so it runs independently.
	_ = cmd.Process.Release()

	if !waitForSocket(socketPath, LifecycleStartupTimeout) {
		return fmt.Errorf("daemon started but socket not ready after %s at %s", LifecycleStartupTimeout, socketPath)
	}

	return nil
}
