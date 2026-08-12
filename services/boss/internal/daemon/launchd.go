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
	"time"
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
		<key>LC_CTYPE</key>
		<string>UTF-8</string>
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
	<key>SoftResourceLimits</key>
	<dict>
		<key>NumberOfFiles</key>
		<integer>65536</integer>
	</dict>
	<key>HardResourceLimits</key>
	<dict>
		<key>NumberOfFiles</key>
		<integer>65536</integer>
	</dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string>
		<key>LC_CTYPE</key>
		<string>UTF-8</string>
	</dict>
</dict>
</plist>
`
)

// runLaunchctl invokes launchctl and returns its combined output. It is a
// package var so tests can inject a fake without a real launchd domain (CI has
// none, and BOSS_DAEMON_SKIP_LAUNCHCTL short-circuits the code under test).
var runLaunchctl = func(args ...string) ([]byte, error) {
	// #nosec G204 -- launchctl; const argv verbs plus derived $HOME plist paths and int uid targets; no shell
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	return exec.Command("launchctl", args...).CombinedOutput()
}

// startDetachedBossd starts bossd without tying its lifecycle to the caller.
// It is indirected for fallback-start regression coverage without launching a
// real daemon from the test process.
var startDetachedBossd = func(bossdPath string) error {
	// #nosec G204 -- self-spawn of staged bossd binary; literal args; local-trust
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
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
	return nil
}

// bootoutVerifyTimeout bounds the post-bootout re-probe. Deliberately short:
// this is the error path, and a service that is still loaded after this long is
// a real failure worth reporting.
var bootoutVerifyTimeout = 2 * time.Second

// bootoutLaunchdService bootouts a launchd service by label and verifies the
// job is actually gone before reporting an error, rather than trusting
// launchctl's exit code alone.
//
// BOS-627: on this macOS build, `launchctl bootout gui/<uid>/<label>` exits 3
// ("No such process") when the service is already unloaded, and a separate,
// unrelated `launchctl list <label>` exits 113 ("Could not find service").
// bootout never returns 113. The prior code special-cased only 113, so every
// bootout of an already-stopped service (which returns 3, or sometimes 5)
// fell through to a hard error. 3 and 113 (kept defensively across macOS
// versions) are treated as already-stopped. Any other non-zero exit —
// notably 5, a generic EIO that can also mean "the job exists but could not
// be removed" — is not trusted at face value: launchd can return before it
// has finished tearing the job down, so stillRunning is polled until it
// reports false or bootoutVerifyTimeout elapses.
func bootoutLaunchdService(label string, stillRunning func() bool) error {
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	out, err := runLaunchctl("bootout", target)
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 3, 113:
			return nil
		}
	}

	// M7: no production caller passes a nil probe. Without one there is no way
	// to verify the job is actually gone, so fail closed -- surface the
	// launchctl error rather than silently reporting success for an exit code
	// (e.g. 5, a generic EIO) that can also mean the job still exists.
	if stillRunning == nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}

	deadline := time.Now().Add(bootoutVerifyTimeout)
	for stillRunning() {
		if time.Now().After(deadline) {
			return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(LifecyclePollInterval)
	}
	return nil
}

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

	out, err := runLaunchctl("load", plistPath)
	if err != nil {
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
		_, _ = runLaunchctl("unload", plistPath)
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

	domainTarget := "gui/" + strconv.Itoa(os.Getuid())
	// Best-effort clear of any stale load before bootstrapping; errors are
	// ignored since bootstrap below surfaces any real problem. Uses the label
	// form (BOS-627), matching the other bootout call sites.
	_, _ = runLaunchctl("bootout", domainTarget+"/"+McpLabel)

	out, err := runLaunchctl("bootstrap", domainTarget, plistPath)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformMcpStop bootouts the MCP LaunchAgent, leaving the plist in place,
// and verifies the launchd job is actually gone before returning success.
//
// BOS-627: launchctl bootout exits 3 ("No such process") for an
// already-unloaded label — not 113, which belongs to `launchctl list` and is
// never returned by bootout at all. The previous exit-113-only check was
// therefore vacuous and every already-stopped `boss mcp stop` returned a
// hard error. This build also observed bootout exit 5 (a generic EIO) for
// the same already-stopped case, indistinguishable by exit code alone from a
// job that genuinely failed to unload, so 5 is verified against actual
// status via bootoutLaunchdService rather than trusted or rejected outright.
func platformMcpStop() error {
	if skipLaunchctl() {
		return nil
	}
	return bootoutLaunchdService(McpLabel, mcpStillRunningProbe)
}

// mcpStillRunningProbe / bossdStillRunningProbe are the stillRunning callbacks
// bootoutLaunchdService polls after a non-{0,3,113} bootout exit.
//
// They fail CLOSED on a probe error, for the same reason bootoutLaunchdService
// rejects a nil probe: a status read that itself failed (e.g. os.Stat on
// ~/Library/LaunchAgents returning something other than ENOENT) is "cannot
// tell", not "stopped". Reporting "not running" there would turn an
// unverifiable end state into a success and hand `boss mcp stop` /
// `boss daemon stop` a silent false pass after a bootout that actually failed.
// Note a NOT-loaded label is not an error on this path: platformGetStatus
// swallows launchctl list's non-zero exit and returns (st, nil) with
// Running=false, so the ordinary already-stopped case still verifies cleanly.
func mcpStillRunningProbe() bool {
	st, err := platformMcpGetStatus()
	if err != nil {
		return true
	}
	return st.Running
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

	out, err := runLaunchctl("list", McpLabel)
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
	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}

	if !force {
		if _, err := os.Stat(plistPath); err == nil {
			return fmt.Errorf("plist already exists at %s (use --force to overwrite)", plistPath)
		}
	}

	stagedPath, err := EnsureStaged(bossdPath)
	if err != nil {
		return err
	}

	plist, err := generatePlist(stagedPath)
	if err != nil {
		return err
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
	out, err := runLaunchctl("load", plistPath)
	if err != nil {
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
		_, _ = runLaunchctl("unload", plistPath)
	}

	// Remove the plist file.
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}

	return nil
}

func platformRestart() error {
	plistPath, err := platformServicePath()
	if err != nil {
		return err
	}
	sourcePath, refreshErr := ResolveBossdPath()

	domainTarget := "gui/" + strconv.Itoa(os.Getuid())
	// Best-effort clear of any stale load before bootstrapping; errors are
	// ignored since bootstrap below surfaces any real problem. Uses the label
	// form (BOS-627), matching the other bootout call sites.
	if !skipLaunchctl() {
		_, _ = runLaunchctl("bootout", domainTarget+"/"+Label)
	}

	if refreshErr == nil {
		var stagedPath string
		stagedPath, refreshErr = EnsureStaged(sourcePath)
		if refreshErr == nil {
			var plist string
			plist, refreshErr = generatePlist(stagedPath)
			if refreshErr == nil {
				// #nosec G304 -- platformServicePath returns the fixed per-user LaunchAgents plist; non-secret local service state.
				// owner=@recurser review-by=2027-01-18 issue=BOS-28
				currentPlist, readErr := os.ReadFile(plistPath)
				switch {
				case readErr == nil && bytes.Equal(currentPlist, []byte(plist)):
					// Preserve the existing file when its content is current.
				case readErr == nil || os.IsNotExist(readErr):
					if writeErr := os.WriteFile(plistPath, []byte(plist), 0o600); writeErr != nil {
						refreshErr = fmt.Errorf("rewrite plist: %w", writeErr)
					}
				default:
					refreshErr = fmt.Errorf("read plist before rewrite: %w", readErr)
				}
			}
		}
	}

	if skipLaunchctl() {
		// Preserve the test-mode contract: service-manager operations and their
		// errors are suppressed. Successful resolution still exercises the
		// file-refresh path above for launchd regression coverage.
		return nil
	}

	// BOS-864: the bootout above has already happened by the time we get here.
	// The observed `exit status 5: Input/output error` immediately after a
	// bootout is a launchd transition race that succeeded on a plain retry
	// moments later, so retry a bounded number of times before giving up.
	//
	// The FIRST failure is the one kept, with the output bytes captured on that
	// same attempt. When attempt 1 loses the transition race but launchd
	// registers the job anyway, later attempts fail with "already loaded"
	// noise; reporting the last error would hide the real cause behind it and
	// leave the retry diagnosing worse than the single attempt it replaced.
	var (
		firstOut  []byte
		firstErr  error
		succeeded bool
	)
	for attempt := 1; attempt <= launchdBootstrapAttempts; attempt++ {
		out, err := runLaunchctl("bootstrap", domainTarget, plistPath)
		if err == nil {
			succeeded = true
			break
		}
		if firstErr == nil {
			firstErr, firstOut = err, out
		}
		if launchctlAlreadyBootstrapped(err) {
			// The job is already registered in the domain, so no further
			// attempt can change the outcome — stop paying the backoff.
			break
		}
		if attempt < launchdBootstrapAttempts {
			time.Sleep(launchdBootstrapRetryDelay)
		}
	}

	var bootstrapErr error
	if !succeeded {
		// Reporting only the bootstrap half is what left the operator with no
		// daemon at all and no idea of it. Verify what is actually running
		// rather than assuming, and name the recovery command.
		bootstrapErr = fmt.Errorf("launchctl bootstrap: %w: %s; %s",
			firstErr, strings.TrimSpace(string(firstOut)), verifiedRestartOutcome())
	}
	return errors.Join(refreshErr, bootstrapErr)
}

// launchctlAlreadyBootstrapped reports whether a `launchctl bootstrap` failure
// means the job is already registered in the domain, in which case retrying
// cannot help.
//
// Exit code, not message text, following the BOS-627 precedent above
// (bootoutLaunchdService): launchctl's human-readable strings vary across macOS
// builds, its exit codes are plain errno values. 17 is EEXIST ("File exists" —
// the service is already loaded) and 37 is EALREADY ("Operation already in
// progress" — a load is already under way). Both are definitive in the same
// sense 3 and 113 are definitive for bootout. Deliberately NOT listed is 5, the
// generic EIO the BOS-864 incident produced: it is ambiguous, so it keeps
// retrying and is then verified against actual status by
// verifiedRestartOutcome. Misclassifying here can only end the retry early — it
// never changes the error reported (always the first) nor the verified outcome.
func launchctlAlreadyBootstrapped(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	switch exitErr.ExitCode() {
	case 17, 37:
		return true
	default:
		return false
	}
}

const launchdBootstrapAttempts = 3

// launchdBootstrapRetryDelay keeps the bounded retry short. It is a package var
// so tests exercise the retry without sleeping.
var launchdBootstrapRetryDelay = 250 * time.Millisecond

// verifiedRestartOutcome probes the real post-failure state through the same
// path platformStop uses. bossdStillRunningProbe fails closed — a probe error
// reads as "still running" — so this can never falsely claim the daemon is
// stopped.
func verifiedRestartOutcome() string {
	if bossdStillRunningProbe() {
		return "bootstrap failed but a daemon is still running"
	}
	return "the daemon is now stopped — run 'boss daemon start'"
}

// platformStop bootouts the LaunchAgent so the running bossd terminates but
// the plist is left in place, and verifies the launchd job is actually gone
// before returning success. A subsequent `start` (or restart) re-bootstraps
// it.
//
// BOS-627: launchctl bootout exits 3 ("No such process") for an
// already-unloaded label — not 113, which belongs to `launchctl list` and is
// never returned by bootout at all. The previous exit-113-only check was
// therefore vacuous and every already-stopped `boss stop` returned a hard
// error. This build also observed bootout exit 5 (a generic EIO) for the
// same already-stopped case, indistinguishable by exit code alone from a job
// that genuinely failed to unload, so 5 is verified against actual status
// via bootoutLaunchdService rather than trusted or rejected outright.
func platformStop() error {
	if skipLaunchctl() {
		return nil
	}
	return bootoutLaunchdService(Label, bossdStillRunningProbe)
}

// bossdStillRunningProbe is platformStop's stillRunning callback. See
// mcpStillRunningProbe for why a probe error means "still running".
func bossdStillRunningProbe() bool {
	st, err := platformGetStatus()
	if err != nil {
		return true
	}
	return st.Running
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

	out, err := runLaunchctl("list", Label)
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
		if _, err := runLaunchctl("load", plistPath); err == nil {
			if waitForSocket(socketPath, LifecycleStartupTimeout) {
				return nil
			}
		}
	}

	// Fall back to starting bossd directly as a background process. Stage it
	// first so this path, used when no LaunchAgent is installed, also preserves
	// macOS TCC's resolved-executable-path grant across package upgrades.
	sourcePath, err := ResolveBossdPath()
	if err != nil {
		return fmt.Errorf("cannot auto-start daemon because start failed: %w", err)
	}
	bossdPath, err := EnsureStaged(sourcePath)
	if err != nil {
		return fmt.Errorf("stage fallback daemon: %w", err)
	}

	// Final guard before spawning: don't race a daemon that just came up.
	if isSocketReachable(socketPath) {
		return nil
	}

	if err := startDetachedBossd(bossdPath); err != nil {
		return err
	}

	if !waitForSocket(socketPath, LifecycleStartupTimeout) {
		return fmt.Errorf("daemon started but socket not ready after %s at %s", LifecycleStartupTimeout, socketPath)
	}

	return nil
}
