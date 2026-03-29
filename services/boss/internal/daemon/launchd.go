//go:build darwin

// Package daemon manages the bossd daemon lifecycle via macOS LaunchAgent.
package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

const (
	// Label is the macOS LaunchAgent label.
	Label = "com.bossanova.bossd"

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

// platformInstall writes the LaunchAgent plist and loads it via launchctl.
func platformInstall(bossdPath string) error {
	plist, err := generatePlist(bossdPath)
	if err != nil {
		return err
	}

	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}

	// Ensure LaunchAgents directory exists.
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}

	// Ensure log directory exists.
	ld, err := logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ld, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	// Write the plist file.
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
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
	cmd := exec.Command("launchctl", "unload", plistPath)
	_ = cmd.Run()

	// Remove the plist file.
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}

	return nil
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

	// Check launchctl for running state.
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
	// Try the LaunchAgent first (if installed).
	st, err := platformGetStatus()
	if err == nil && st.Installed && !st.Running {
		plistPath, _ := platformServicePath()
		if cmd := exec.Command("launchctl", "load", plistPath); cmd.Run() == nil {
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
	// Detach from the parent process.
	cmd.SysProcAttr = nil
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
