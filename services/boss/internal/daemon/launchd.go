//go:build darwin

// Package daemon manages the bossd daemon lifecycle via the macOS launchd agent.
package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

const (
	// Label is the macOS launchd agent label.
	Label = "com.bossanova.bossd"

	// McpLabel is the macOS launchd agent label for the local MCP server.
	McpLabel = "com.bossanova.mcp"

	// DefaultMcpPort is the loopback port the MCP HTTP daemon listens on.
	DefaultMcpPort = 8765

	// mcpPlistTemplate runs `mcp --http 127.0.0.1:<port>`. Unlike the bossd
	// plist, its PATH includes ~/.nodenv/shims and ~/.local/bin so the MCP
	// server (and any agent CLI it shells out to) can find node/agent binaries.
	mcpPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.McpPath}}</string>
		<string>--http</string>
		<string>{{.Addr}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogDir}}/mcp.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogDir}}/mcp.stderr.log</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
	</dict>
</dict>
</plist>
`

	plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BossdPath}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogDir}}/bossd.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogDir}}/bossd.stderr.log</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string>
	</dict>
</dict>
</plist>
`
)

type plistData struct {
	Label     string
	BossdPath string
	LogDir    string
}

type mcpPlistData struct {
	Label   string
	McpPath string
	Addr    string
	LogDir  string
	Path    string
}

// mcpPath builds the PATH for the MCP LaunchAgent. It mirrors the bossd plist
// PATH but additionally includes the agent-runner shim directories
// (~/.nodenv/shims and ~/.local/bin), which the Homebrew launchd PATH omits.
func mcpPath() string {
	entries := []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		entries = append([]string{
			filepath.Join(home, ".nodenv", "shims"),
			filepath.Join(home, ".local", "bin"),
		}, entries...)
	}
	return strings.Join(entries, ":")
}

// platformServicePath returns the path to the LaunchAgent plist file.
func platformServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// logDir returns the log directory for bossd.
func logDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "bossanova"), nil
}

// generatePlist renders the LaunchAgent plist XML for bossd.
func generatePlist(bossdPath string) (string, error) {
	ld, err := logDir()
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return "", fmt.Errorf("parse plist template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, plistData{
		Label:     Label,
		BossdPath: bossdPath,
		LogDir:    ld,
	}); err != nil {
		return "", fmt.Errorf("render plist: %w", err)
	}

	return buf.String(), nil
}

// mcpServicePath returns the path to the MCP LaunchAgent plist file.
func mcpServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", McpLabel+".plist"), nil
}

// generateMcpPlist renders the LaunchAgent plist XML for the local MCP server.
func generateMcpPlist(mcpBinPath string, port int) (string, error) {
	ld, err := logDir()
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("mcpPlist").Parse(mcpPlistTemplate)
	if err != nil {
		return "", fmt.Errorf("parse mcp plist template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, mcpPlistData{
		Label:   McpLabel,
		McpPath: mcpBinPath,
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		LogDir:  ld,
		Path:    mcpPath(),
	}); err != nil {
		return "", fmt.Errorf("render mcp plist: %w", err)
	}

	return buf.String(), nil
}

// platformMcpInstall writes the MCP LaunchAgent plist and loads it via launchctl. When
// force is false and the plist already exists, it refuses to overwrite.
func platformMcpInstall(mcpBinPath string, port int, force bool) error {
	if err := validatePath(mcpBinPath); err != nil {
		return err
	}

	plist, err := generateMcpPlist(mcpBinPath, port)
	if err != nil {
		return err
	}

	plistPath, err := mcpServicePath()
	if err != nil {
		return err
	}

	if !force {
		if _, err := os.Stat(plistPath); err == nil {
			return fmt.Errorf("plist already exists at %s (use --force to overwrite)", plistPath)
		}
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}

	ld, err := logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ld, 0o700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	if skipLaunchctl() {
		return nil
	}

	cmd := exec.Command("launchctl", "load", plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformMcpUninstall unloads the MCP LaunchAgent and removes the plist file.
func platformMcpUninstall() error {
	plistPath, err := mcpServicePath()
	if err != nil {
		return err
	}

	if !skipLaunchctl() {
		_ = exec.Command("launchctl", "unload", plistPath).Run()
	}

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	return nil
}

// platformMcpStart bootstraps the MCP LaunchAgent (loading it if installed).
func platformMcpStart() error {
	if skipLaunchctl() {
		return nil
	}

	plistPath, err := mcpServicePath()
	if err != nil {
		return err
	}

	target := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", target, plistPath).Run()

	cmd := exec.Command("launchctl", "bootstrap", target, plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformMcpStop bootouts the MCP LaunchAgent, leaving the plist in place. It is
// idempotent: launchctl exits 113 when the service isn't loaded, which we treat
// as the desired end state.
func platformMcpStop() error {
	if skipLaunchctl() {
		return nil
	}

	plistPath, err := mcpServicePath()
	if err != nil {
		return err
	}

	target := "gui/" + strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootout", target, plistPath).CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 113 {
		return nil
	}
	return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
}

// platformMcpGetStatus returns the current MCP LaunchAgent status.
func platformMcpGetStatus() (*Status, error) {
	plistPath, err := mcpServicePath()
	if err != nil {
		return nil, err
	}

	st := &Status{ServicePath: plistPath}

	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("check plist file: %w", err)
	}
	st.Installed = true

	if skipLaunchctl() {
		return st, nil
	}

	out, err := exec.Command("launchctl", "list", McpLabel).Output()
	if err != nil {
		return st, nil
	}
	st.Running = true

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "\"PID\"") || strings.HasPrefix(line, "\"pid\"") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				pidStr := strings.TrimSpace(strings.Trim(parts[1], "\";"))
				_, _ = fmt.Sscanf(pidStr, "%d", &st.PID)
			}
		}
	}
	return st, nil
}

// platformInstall writes the LaunchAgent plist and loads it via launchctl.
// When force is false and the plist already exists, it refuses to overwrite.
func platformInstall(bossdPath string, force bool) error {
	plist, err := generatePlist(bossdPath)
	if err != nil {
		return err
	}

	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}

	if !force {
		if _, err := os.Stat(plistPath); err == nil {
			return fmt.Errorf("plist already exists at %s (use --force to overwrite)", plistPath)
		}
	}

	// Ensure LaunchAgents directory exists.
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}

	// Ensure log directory exists.
	ld, err := logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ld, 0o700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	// Write the plist file.
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	if skipLaunchctl() {
		return nil
	}

	// Load the agent.
	cmd := exec.Command("launchctl", "load", plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// platformUninstall unloads the LaunchAgent and removes the plist file.
func platformUninstall() error {
	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}

	// Unload the agent (ignore error if not loaded).
	if !skipLaunchctl() {
		cmd := exec.Command("launchctl", "unload", plistPath)
		_ = cmd.Run()
	}

	// Remove the plist file.
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}

	return nil
}

func platformRestart() error {
	if skipLaunchctl() {
		return nil
	}

	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}

	target := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", target, plistPath).Run()

	cmd := exec.Command("launchctl", "bootstrap", target, plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformStop bootouts the LaunchAgent so the running bossd terminates but
// the plist is left in place. A subsequent `start` (or restart) re-bootstraps
// it. bootout exits non-zero when the agent isn't loaded; we treat that as
// the desired end state and return nil.
func platformStop() error {
	if skipLaunchctl() {
		return nil
	}

	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}

	target := "gui/" + strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootout", target, plistPath).CombinedOutput()
	if err == nil {
		return nil
	}
	// launchctl exits 113 when the specified service isn't loaded. Treat as a
	// no-op so `stop` is idempotent. Match on exit code rather than message
	// text, which varies across macOS versions and locales.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 113 {
		return nil
	}
	return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
}

// platformGetStatus returns the current daemon status.
func platformGetStatus() (*Status, error) {
	plistPath, err := platformServicePath()
	if err != nil {
		return nil, err
	}

	st := &Status{ServicePath: plistPath}

	// Check if plist exists.
	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("check plist file: %w", err)
	}
	st.Installed = true

	// Check launchctl for running state (skipped in test mode).
	if skipLaunchctl() {
		return st, nil
	}

	cmd := exec.Command("launchctl", "list", Label)
	out, err := cmd.Output()
	if err != nil {
		// Not loaded / not running.
		return st, nil
	}

	st.Running = true

	// Parse PID from launchctl list output.
	// Format: "PID" \t "Status" \t "Label" or similar key-value pairs.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "\"PID\"") || strings.HasPrefix(line, "\"pid\"") {
			// launchctl list <label> outputs key-value pairs.
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				pidStr := strings.TrimSpace(strings.Trim(parts[1], "\";"))
				_, _ = fmt.Sscanf(pidStr, "%d", &st.PID)
			}
		}
		// Also try tab-separated format from `launchctl list | grep`.
		if strings.Contains(line, Label) {
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				_, _ = fmt.Sscanf(parts[0], "%d", &st.PID)
			}
		}
	}

	return st, nil
}

// platformEnsureRunning attempts to start the daemon via LaunchAgent or fallback.
func platformEnsureRunning(socketPath string) error {
	// Re-probe: a daemon may have come up (or another caller started one) since
	// EnsureRunning's initial check. Spawning a duplicate bossd here is the root
	// cause of the socket-stealing storm, so never start one if the socket is
	// already being served.
	if isSocketReachable(socketPath) {
		return nil
	}

	// Try the LaunchAgent first (if installed).
	st, err := platformGetStatus()
	if err == nil && st.Installed && !st.Running {
		plistPath, _ := platformServicePath()
		if cmd := exec.Command("launchctl", "load", plistPath); cmd.Run() == nil {
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

	cmd := exec.Command(bossdPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Detach from the parent process.
	cmd.SysProcAttr = nil
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
