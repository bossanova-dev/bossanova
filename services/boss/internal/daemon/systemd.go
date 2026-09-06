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

	// unitTemplate runs bossd. BOS-880 gave it an explicit Environment=PATH=:
	// it previously inherited the systemd user manager's PATH, which is not the
	// interactive shell's, so a nodenv/nvm/asdf toolchain was invisible and
	// every `node`-based cron gate exited 127. It renders from serviceEnvPath,
	// the same helper mcpUnitTemplate uses.
	//
	// The PATH value is QUOTED. systemd splits an unquoted Environment= line on
	// whitespace into separate assignments, so a perfectly legal directory such
	// as `/opt/my tools/bin` would silently truncate the daemon's PATH — the
	// same class of failure this change exists to fix.
	//
	// TimeoutStopSec is the SIGTERM-to-SIGKILL grace, and BOS-888 pins it
	// rather than inheriting it. systemd's DefaultTimeoutStopSec (90s) already
	// exceeds bossd's graceful budget, but a distro that lowered the default
	// would cut the failover-proxy drain mid-stream, so the value is written
	// explicitly and must stay above LifecycleShutdownTimeout. The rationale
	// lives here rather than as a `#` comment in the rendered unit because
	// TestGenerateUnitRejectsNewlineInjection requires every generated line to
	// be a directive or a section header — prose in the template would blunt an
	// injection guard to no operator benefit.
	//
	// Reach: NEW INSTALLS ONLY. Unlike launchd's, this platform's restart is a
	// bare `systemctl --user restart`, so it neither regenerates the unit nor
	// daemon-reloads; an existing install keeps whatever TimeoutStopSec it was
	// written with until `boss daemon install --force`.
	unitTemplate = `[Unit]
Description=Bossanova Daemon
After=network.target

[Service]
ExecStart={{.BossdPath}}
LimitNOFILE=65536
Restart=always
RestartSec=5
TimeoutStopSec=90
Environment="PATH={{.Path}}"
Environment=LC_CTYPE=C.UTF-8

[Install]
WantedBy=default.target
`

	// mcpUnitTemplate runs `mcp --http 127.0.0.1:<port>`. Its PATH comes from
	// serviceEnvPath, the same helper the bossd unit renders from, so the MCP
	// server (and any agent CLI it shells out to) can find node/agent binaries.
	mcpUnitTemplate = `[Unit]
Description=Bossanova MCP Server
After=network.target

[Service]
ExecStart={{.McpPath}} --http {{.Addr}}
Restart=always
RestartSec=5
Environment="PATH={{.Path}}"
Environment=LC_CTYPE=C.UTF-8

[Install]
WantedBy=default.target
`
)

type unitData struct {
	BossdPath string
	Path      string
}

type mcpUnitData struct {
	McpPath string
	Addr    string
	Path    string
}

// serviceEnvPath builds the PATH for BOTH systemd units — bossd and the MCP
// server. One helper, because the two diverging was the bug (BOS-880): the
// bossd unit set no PATH at all and inherited the systemd user manager's,
// while the MCP unit prepended the agent-runner shim directories.
//
// The baseline prepends ~/.nodenv/shims and ~/.local/bin to the PATH this
// process inherited, so the unit stays as wide as the environment that
// installed it. daemon_path_extra prepends to that; it can never remove a
// baseline entry.
func serviceEnvPath() string {
	entries := []string{}
	if home, err := userHomeDir(); err == nil {
		entries = append(entries,
			filepath.Join(home, ".nodenv", "shims"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	if p := os.Getenv("PATH"); p != "" {
		// The inherited PATH is already colon-joined; split it so the dedupe in
		// joinServicePath sees individual directories rather than one opaque
		// entry that could never match a configured extra.
		entries = append(entries, strings.Split(p, ":")...)
	} else {
		entries = append(entries, "/usr/local/bin", "/usr/bin", "/bin")
	}
	return joinServicePath(pathExtras(serviceEnvSettings()), entries)
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
	if err := tmpl.Execute(&buf, unitData{BossdPath: bossdPath, Path: serviceEnvPath()}); err != nil {
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
		Path:    serviceEnvPath(),
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
//
// The returned StartMode says whether systemd started the daemon or whether
// this fell through to the unsupervised direct spawn. BOS-1183: the caller has
// no other way to tell the two apart, because both return a nil error and a
// serving socket. The control flow here is unchanged; only the verdict is
// threaded out.
func platformEnsureRunning(socketPath string) (StartMode, error) {
	// Re-probe: a daemon may have come up (or another caller started one) since
	// EnsureRunning's initial check. Spawning a duplicate bossd here is the root
	// cause of the socket-stealing storm, so never start one if the socket is
	// already being served.
	if isSocketReachable(socketPath) {
		return StartModeAlreadyRunning, nil
	}

	// Try the systemd service first (if installed).
	st, err := platformGetStatus()
	if err == nil && st.Installed && !st.Running {
		if _, err := runSystemctl("--user", "start", ServiceName); err == nil {
			if waitForSocket(socketPath, LifecycleStartupTimeout) {
				return StartModeServiceManager, nil
			}
		}
	}

	// Fall back to starting bossd directly as a background process.
	bossdPath, err := ResolveBossdPath()
	if err != nil {
		return StartModeUnknown, fmt.Errorf("cannot auto-start daemon because start failed: %w", err)
	}

	// Final guard before spawning: don't race a daemon that just came up.
	if isSocketReachable(socketPath) {
		return StartModeAlreadyRunning, nil
	}

	// #nosec G204 -- self-spawn of the discovered bossd binary (bossdPath); literal args, no shell; local-trust, not attacker-controlled.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command(bossdPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return StartModeUnknown, fmt.Errorf("start bossd: %w", err)
	}

	// Release the child process so it runs independently.
	_ = cmd.Process.Release()

	if !waitForSocket(socketPath, LifecycleStartupTimeout) {
		return StartModeUnknown, fmt.Errorf("daemon started but socket not ready after %s at %s", LifecycleStartupTimeout, socketPath)
	}

	return StartModeDetached, nil
}

// InstalledServiceEnvPath returns the PATH recorded in the systemd unit file
// on disk right now — what the RUNNING daemon has, which differs from
// serviceEnvPath() until the next restart rewrites the unit.
//
// ok is false when the unit is absent or sets no PATH, so a caller can never
// mistake "could not read it" for "it matches".
func InstalledServiceEnvPath() (string, bool) {
	unitPath, err := platformServicePath()
	if err != nil {
		return "", false
	}
	// #nosec G304 -- platformServicePath returns the fixed per-user systemd unit path; non-secret local service state.
	// owner=@recurser review-by=2027-01-18 issue=BOS-880
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return "", false
	}
	return unitEnvironmentPath(string(data))
}

// unitEnvironmentPath extracts the PATH from a unit's Environment= line,
// accepting both the quoted form this package writes and the bare form an
// older install (or a hand edit) may carry.
//
// A single Environment= line may hold SEVERAL assignments, quoted or not
// (`Environment="PATH=/a b:/bin" LC_CTYPE=C.UTF-8`), so the value is split into
// assignments before PATH is picked out. Trimming outer quotes and taking
// everything after `PATH=` would swallow the following assignments into the
// path and report a spurious mismatch.
func unitEnvironmentPath(unit string) (string, bool) {
	for _, line := range strings.Split(unit, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment=")
		if !ok {
			continue
		}
		for _, assignment := range splitUnitAssignments(value) {
			if path, ok := strings.CutPrefix(assignment, "PATH="); ok {
				return path, path != ""
			}
		}
	}
	return "", false
}

// splitUnitAssignments splits a systemd Environment= value into its individual
// assignments, treating a double-quoted run as one token so a directory
// containing a space stays intact.
func splitUnitAssignments(value string) []string {
	var (
		assignments []string
		current     strings.Builder
		quoted      bool
	)
	flush := func() {
		if current.Len() > 0 {
			assignments = append(assignments, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t'):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return assignments
}

// platformSpawnHistory reports that Linux has no launchd-style spawn history to
// read.
//
// BOS-1183 is a launchd-domain defect: launchd will register a job into a
// session it never spawns anything in, and `launchctl list` cannot see the
// difference. systemd has no equivalent blind spot — it reports unit substates
// (activating/failed/inactive) and a restart count directly — so there is
// nothing extra to probe here. Returning "unsupported" rather than a verdict
// keeps the Linux reporting path behaviourally unchanged: this must never make
// Linux report unhealthy.
func platformSpawnHistory() (SpawnHistory, error) {
	return SpawnHistory{
		State:  SpawnStateUnsupported,
		Reason: "systemd reports unit substates directly; no launchd-style spawn history",
	}, nil
}
