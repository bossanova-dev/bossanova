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

const (
	// ServiceName is the systemd unit name.
	ServiceName = "bossd.service"

	unitTemplate = `[Unit]
Description=Bossanova Daemon
After=network.target

[Service]
ExecStart={{.BossdPath}}
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`
)

type unitData struct {
	BossdPath string
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
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create systemd user dir: %w", err)
	}

	// Write the unit file.
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	if skipLaunchctl() {
		return nil
	}

	// Reload systemd daemon.
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Enable and start the service.
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", ServiceName).CombinedOutput(); err != nil {
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
		_ = exec.Command("systemctl", "--user", "disable", "--now", ServiceName).Run()
	}

	// Remove the unit file.
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}

	// Reload systemd daemon.
	if !skipLaunchctl() {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
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
	cmd := exec.Command("systemctl", "--user", "is-active", ServiceName)
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		st.Running = true
	}

	// Get PID if running.
	if st.Running {
		cmd := exec.Command("systemctl", "--user", "show", "--property=MainPID", ServiceName)
		out, err := cmd.Output()
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

// platformEnsureRunning attempts to start the daemon if it's not reachable.
func platformEnsureRunning(socketPath string) error {
	// Try the systemd service first (if installed).
	st, err := platformGetStatus()
	if err == nil && st.Installed && !st.Running {
		if cmd := exec.Command("systemctl", "--user", "start", ServiceName); cmd.Run() == nil {
			if waitForSocket(socketPath, 3*time.Second) {
				return nil
			}
		}
	}

	// Fall back to starting bossd directly as a background process.
	bossdPath, err := ResolveBossdPath()
	if err != nil {
		return fmt.Errorf("cannot auto-start daemon: %w", err)
	}

	cmd := exec.Command(bossdPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bossd: %w", err)
	}

	// Release the child process so it runs independently.
	_ = cmd.Process.Release()

	if !waitForSocket(socketPath, 3*time.Second) {
		return fmt.Errorf("daemon started but socket not ready at %s", socketPath)
	}

	return nil
}
