//go:build darwin

// Package daemon manages the bossd daemon lifecycle via the macOS launchd agent.
package daemon

import (
	"bytes"
	"encoding/xml"
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

	// mcpPlistTemplate runs `mcp --http 127.0.0.1:<port>`. Its PATH comes from
	// serviceEnvPath, the same helper the bossd plist renders from, so the MCP
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

	// ExitTimeOut is the SIGTERM-to-SIGKILL grace launchd gives bossd on
	// `bootout`. It defaults to 20s, which is BELOW bossd's own graceful
	// shutdown budget once the failover proxy drain is in the path (BOS-888) —
	// a hard kill there skips the deferred database.Close and the socket
	// cleanup. Keep it above LifecycleShutdownTimeout so the CLI's wait, not
	// launchd's axe, is what bounds a stuck shutdown.
	//
	// Reach: platformRestart rewrites the plist, so existing installs pick this
	// up without a reinstall — but it boots the old job out FIRST, so the one
	// restart that performs the upgrade still shuts down under the previous
	// ExitTimeOut. Only the restart after that is covered.
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
	<key>ExitTimeOut</key>
	<integer>90</integer>
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
		<string>{{.Path}}</string>
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
	Path      string
}

type mcpPlistData struct {
	Label   string
	McpPath string
	Addr    string
	LogDir  string
	Path    string
}

// serviceEnvPath builds the PATH for BOTH LaunchAgents — bossd and the MCP
// server. One helper, because the two diverging was the bug: launchd never
// sources an interactive shell config, so a nodenv/nvm/asdf toolchain or a
// native `claude` in ~/.local/bin was invisible to bossd while the MCP agent
// could see it, and every `node`-based cron gate exited 127 (BOS-880).
//
// The baseline includes the agent-runner shim directories (~/.nodenv/shims and
// ~/.local/bin), which the Homebrew launchd PATH omits. daemon_path_extra
// prepends to it; it can never remove a baseline entry.
func serviceEnvPath() string {
	entries := []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"}
	if home, err := userHomeDir(); err == nil {
		entries = append([]string{
			filepath.Join(home, ".nodenv", "shims"),
			filepath.Join(home, ".local", "bin"),
		}, entries...)
	}
	return joinServicePath(pathExtras(serviceEnvSettings()), entries)
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
		Path:      serviceEnvPath(),
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
		Path:    serviceEnvPath(),
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

// refreshStagedPlist stages sourcePath at the stable staged path and brings
// plistPath in line with it, leaving the file untouched when its generated
// bytes already match what is on disk. It returns the staged path so a caller
// that goes on to need it does not have to re-derive it — see
// platformEnsureRunning, where repeating the work means re-hashing the binary
// rather than re-running a stat.
//
// This is the single stage-then-compare-then-write path. platformRestart and
// platformEnsureRunning both go through it, which is what stops `start` and
// `restart` disagreeing about which build the LaunchAgent names: the plist
// points at the staged copy, so whoever hands that plist to launchctl has to
// have refreshed the copy first (BOS-977).
//
// Rewriting only on a byte difference is deliberate. The plist normally names
// the same stable staged path every time, so an unconditional write would churn
// a file launchd watches for no gain.
func refreshStagedPlist(sourcePath, plistPath string) (string, error) {
	stagedPath, err := EnsureStaged(sourcePath)
	if err != nil {
		return "", err
	}
	plist, err := generatePlist(stagedPath)
	if err != nil {
		return "", err
	}

	// #nosec G304 -- both callers pass platformServicePath's result: the fixed per-user LaunchAgents plist; non-secret local service state.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	currentPlist, readErr := os.ReadFile(plistPath)
	switch {
	case readErr == nil && bytes.Equal(currentPlist, []byte(plist)):
		// Preserve the existing file when its content is current.
		return stagedPath, nil
	case readErr == nil || os.IsNotExist(readErr):
		if writeErr := os.WriteFile(plistPath, []byte(plist), 0o600); writeErr != nil {
			return "", fmt.Errorf("rewrite plist: %w", writeErr)
		}
		return stagedPath, nil
	default:
		return "", fmt.Errorf("read plist before rewrite: %w", readErr)
	}
}

// refreshInstalledDaemon resolves the installed bossd and refreshes the staged
// copy plus the plist that the LaunchAgent load will read, returning the staged
// path it brought up to date.
func refreshInstalledDaemon(plistPath string) (string, error) {
	sourcePath, err := ResolveBossdPath()
	if err != nil {
		return "", err
	}
	return refreshStagedPlist(sourcePath, plistPath)
}

// warnDaemonRefreshFailed reports a pre-load staging or plist-refresh failure
// to the operator without failing the start. It is a package var so tests can
// observe the surfaced reason, following the runLaunchctl / executablePath
// indirection idiom in this package.
//
// The wording names the plist as well as the binary because refreshStagedPlist
// fails for either: a current staged copy with an unreadable plist lands here
// too, and a message that blamed only staging would misdirect that operator.
var warnDaemonRefreshFailed = func(err error) {
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: could not refresh the staged bossd or its LaunchAgent plist before starting it: %v; starting the previously staged build — run 'boss daemon restart' if it is out of date\n",
		err)
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
		_, refreshErr = refreshStagedPlist(sourcePath, plistPath)
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
	//
	// BOS-977: the flat 250ms delay made that window ~750ms in total, which the
	// reported incident outran. The waits now back off (see
	// launchdBootstrapDelays) under an explicitly bounded total.
	delays := launchdBootstrapDelays()
	var (
		firstOut  []byte
		firstErr  error
		succeeded bool
	)
	for attempt := 0; attempt <= len(delays); attempt++ {
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
		if attempt < len(delays) {
			launchdSleep(delays[attempt])
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

const (
	// launchdBootstrapAttempts caps how many `launchctl bootstrap` attempts one
	// restart makes.
	launchdBootstrapAttempts = 6

	// launchdBootstrapRetryWindow caps the TOTAL time spent waiting between
	// those attempts.
	//
	// Bounding the window and not just the attempt count is the point of
	// BOS-977. The incident's bootout had still not released the job well past
	// the old flat 3 x 250ms budget, so the delays have to grow — but growing
	// them under an attempt cap alone silently trades a fixed upgrade race for
	// a genuinely broken bootstrap that takes far longer to report. Five
	// seconds comfortably covers the observed race while still failing fast.
	launchdBootstrapRetryWindow = 5 * time.Second
)

// launchdBootstrapRetryDelay is the FIRST backoff step; each later step doubles
// it until launchdBootstrapRetryWindow is spent. It is a package var so tests
// exercise the retry without sleeping.
var launchdBootstrapRetryDelay = 250 * time.Millisecond

// launchdSleep is the wait between bootstrap attempts. It is a package var so a
// test can drive the bounded window on a simulated clock rather than real time.
var launchdSleep = time.Sleep

// launchdBootstrapDelays returns the ordered waits between the bounded
// bootstrap attempts: each step doubles the one before it, and the schedule is
// truncated so the waits can never total more than
// launchdBootstrapRetryWindow — including the final step, which is clamped to
// whatever is left of the window rather than dropped.
//
// A zero base delay (what tests set) keeps every attempt and drops only the
// waiting, so the retry's shape stays observable without real time passing.
func launchdBootstrapDelays() []time.Duration {
	delays := make([]time.Duration, 0, launchdBootstrapAttempts-1)
	remaining := launchdBootstrapRetryWindow
	delay := launchdBootstrapRetryDelay
	for len(delays) < launchdBootstrapAttempts-1 {
		if delay <= 0 {
			delays = append(delays, 0)
			continue
		}
		if remaining <= 0 {
			break
		}
		if delay > remaining {
			delay = remaining
		}
		delays = append(delays, delay)
		remaining -= delay
		delay *= 2
	}
	return delays
}

// verifiedRestartOutcome probes the real post-failure state through the same
// path platformStop uses. bossdStillRunningProbe fails closed — a probe error
// reads as "still running" — so this can never falsely claim the daemon is
// stopped.
func verifiedRestartOutcome() string {
	if bossdStillRunningProbe() {
		return "bootstrap failed but a daemon is still running"
	}
	return RestartRecoveryHint
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
//
// The returned StartMode says which of those two happened. BOS-1183: the
// fallback is unsupervised, and the caller has no other way to tell — both
// paths return a nil error and a serving socket. The control flow here is
// unchanged; only the verdict is threaded out.
func platformEnsureRunning(socketPath string) (StartMode, error) {
	// Re-probe: a daemon may have come up (or another caller started one) since
	// EnsureRunning's initial check. Spawning a duplicate bossd here is the root
	// cause of the socket-stealing storm, so never start one if the socket is
	// already being served.
	if isSocketReachable(socketPath) {
		return StartModeAlreadyRunning, nil
	}

	// stagedPath is set only when the LaunchAgent refresh below actually
	// succeeded, and lets the direct-spawn fallback reuse that work instead of
	// resolving and staging a second time. daemonbin.NeedsStage short-circuits
	// on a SHA-256 of both the source and the staged copy, not on a stat, so
	// the repeat would re-hash ~38 MB twice over on a path already reached by
	// a failed load.
	var stagedPath string

	// Try the LaunchAgent first (if installed).
	st, err := platformGetStatus()
	if err == nil && st.Installed && !st.Running {
		plistPath, _ := platformServicePath()

		// BOS-977: the plist names the STAGED copy, so loading it without
		// refreshing that copy first starts the PREVIOUS build after a package
		// upgrade — and reports success. That is why `boss daemon restart`,
		// which stages unconditionally, was the only command that cleared the
		// staleness warning.
		//
		// Staging is a file copy rather than a service operation, so it is safe
		// ahead of the load and leaves both surrounding isSocketReachable
		// guards where they are.
		//
		// A refresh failure must never make a working start worse: the
		// behaviour here has always been "load whatever is already staged", so
		// surface the reason and fall through to the load rather than turning a
		// stale start into no start at all.
		refreshed, refreshErr := refreshInstalledDaemon(plistPath)
		if refreshErr != nil {
			warnDaemonRefreshFailed(refreshErr)
		} else {
			stagedPath = refreshed
		}

		if _, err := runLaunchctl("load", plistPath); err == nil {
			if waitForSocket(socketPath, LifecycleStartupTimeout) {
				return StartModeServiceManager, nil
			}
		}
	}

	// Fall back to starting bossd directly as a background process. Stage it
	// first so this path, used when no LaunchAgent is installed, also preserves
	// macOS TCC's resolved-executable-path grant across package upgrades —
	// unless the refresh above already did exactly that, in which case reuse
	// its result rather than paying for it twice.
	bossdPath := stagedPath
	if bossdPath == "" {
		sourcePath, resolveErr := ResolveBossdPath()
		if resolveErr != nil {
			return StartModeUnknown, fmt.Errorf("cannot auto-start daemon because start failed: %w", resolveErr)
		}
		bossdPath, err = EnsureStaged(sourcePath)
		if err != nil {
			return StartModeUnknown, fmt.Errorf("stage fallback daemon: %w", err)
		}
	}

	// Final guard before spawning: don't race a daemon that just came up.
	if isSocketReachable(socketPath) {
		return StartModeAlreadyRunning, nil
	}

	if err := startDetachedBossd(bossdPath); err != nil {
		return StartModeUnknown, err
	}

	if !waitForSocket(socketPath, LifecycleStartupTimeout) {
		return StartModeUnknown, fmt.Errorf("daemon started but socket not ready after %s at %s", LifecycleStartupTimeout, socketPath)
	}

	return StartModeDetached, nil
}

// InstalledServiceEnvPath returns the PATH recorded in the LaunchAgent plist
// that is on disk right now — which is what the RUNNING daemon has, and which
// differs from serviceEnvPath() until the next restart rewrites the file.
//
// Reporting only the computed value would reproduce the BOS-880 failure inside
// the diagnostic itself: on the affected machine `boss daemon doctor` would
// have said node was visible while the live daemon still could not see it.
//
// ok is false when the plist is absent or carries no PATH, so a caller can
// never mistake "could not read it" for "it matches".
func InstalledServiceEnvPath() (string, bool) {
	plistPath, err := platformServicePath()
	if err != nil {
		return "", false
	}
	// #nosec G304 -- platformServicePath returns the fixed per-user LaunchAgents plist; non-secret local service state.
	// owner=@recurser review-by=2027-01-18 issue=BOS-880
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return "", false
	}
	return plistEnvironmentPath(data)
}

// plistEnvironmentPath extracts EnvironmentVariables > PATH from plist XML.
//
// The PATH key is looked up ONLY inside the EnvironmentVariables dict, which is
// why the scan is scoped to that dict rather than run over the whole document:
// a plist that sets no environment PATH but carries an unrelated <key>PATH</key>
// elsewhere must report "not found", not that unrelated value.
func plistEnvironmentPath(data []byte) (string, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return "", false
		}
		if key == "EnvironmentVariables" {
			return plistDictStringValue(decoder, "PATH")
		}
	}
}

// plistDictStringValue reads the <dict> that follows the current position and
// returns the string value for want. It stops at that dict's own closing tag,
// so the search can never run on into the rest of the document.
func plistDictStringValue(decoder *xml.Decoder, want string) (string, bool) {
	// Advance to the dict this key introduces.
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		if start, ok := token.(xml.StartElement); ok {
			if start.Name.Local != "dict" {
				return "", false
			}
			break
		}
	}

	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch {
			case element.Name.Local == "dict" || element.Name.Local == "array":
				depth++
				if err := decoder.Skip(); err != nil {
					return "", false
				}
				depth--
			case depth == 0 && element.Name.Local == "key":
				var key string
				if err := decoder.DecodeElement(&key, &element); err != nil {
					return "", false
				}
				if key != want {
					continue
				}
				return plistNextString(decoder)
			}
		case xml.EndElement:
			if element.Name.Local == "dict" {
				// Closed the EnvironmentVariables dict without finding want.
				return "", false
			}
		}
	}
}

// plistNextString returns the next <string> element's text.
func plistNextString(decoder *xml.Decoder) (string, bool) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local != "string" {
				return "", false
			}
			var value string
			if err := decoder.DecodeElement(&value, &element); err != nil {
				return "", false
			}
			return value, value != ""
		case xml.EndElement:
			// The key had no value element.
			return "", false
		}
	}
}

// platformSpawnHistory reads launchd's spawn history for the installed bossd
// job.
//
// BOS-1183: this is the only probe here that can tell a REGISTERED job from a
// RUNNABLE one. `launchctl list <label>` — which platformGetStatus uses, and
// which this function deliberately does not touch — exits 0 for a job launchd
// has loaded into a domain it will never spawn anything in, so Status.Running
// reports true for a daemon that is never going to start. `launchctl print`
// carries `runs` and `last exit code`, which separate "launchd never tried"
// from "bossd started and died".
//
// Error discipline (see GetSpawnHistory): a non-nil error means launchctl could
// not be EXECUTED. A launchctl that ran and exited non-zero — the "could not
// find service in domain" case — is a nil error with an unknown verdict, since
// whether the job is registered at all is a fact Status.Installed already owns.
// Both paths still return a populated, fail-closed SpawnHistory.
func platformSpawnHistory() (SpawnHistory, error) {
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + Label

	// Checked BEFORE shelling out, matching reportDaemonSupervision in
	// services/boss/cmd/daemon_doctor.go: under this env var the service view
	// is meaningless, and a verdict derived from it on every CI run and test
	// harness would train operators to ignore the one line that matters on a
	// real host.
	if skipLaunchctl() {
		return SpawnHistory{
			State:  SpawnStateUnknown,
			Target: target,
			Reason: "service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL",
		}, nil
	}

	out, err := runLaunchctl("print", target)
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return SpawnHistory{
				State:  SpawnStateUnknown,
				Target: target,
				Reason: fmt.Sprintf("could not run launchctl print %s: %v", target, err),
			}, fmt.Errorf("launchctl print %s: %w", target, err)
		}
		return SpawnHistory{
			State:  SpawnStateUnknown,
			Target: target,
			Reason: fmt.Sprintf("launchctl print %s exited %d: %q", target, exitErr.ExitCode(), strings.TrimSpace(string(out))),
		}, nil
	}

	history := parseLaunchdSpawnHistory(out)
	history.Target = target
	return history, nil
}
