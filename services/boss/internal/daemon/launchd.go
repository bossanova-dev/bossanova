//go:build darwin

// Package daemon manages the bossd daemon lifecycle via the macOS launchd agent.
package daemon

import (
	"bytes"
	"context"
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

// launchctlExitSaysAlreadyGone reports whether a launchctl error positively
// means the target is not registered.
//
// It is the single home for that empirical, macOS-version-sensitive knowledge
// (BOS-627): `bootout` exits 3 when the job is already unloaded, and 113 is
// kept defensively across releases. Every caller that needs it -- the bootout
// path and the watchdog's still-loaded probe, in both launchd domains -- reads
// it here, so a release that changes the codes is one edit rather than a hunt.
//
// Note what it deliberately does NOT cover: exit 5 is a generic EIO that also
// occurs when launchd simply has not finished tearing a job down, so it is not
// "already gone" and callers must verify by probing instead.
func launchctlExitSaysAlreadyGone(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	switch exitErr.ExitCode() {
	case 3, 113:
		return true
	}
	return false
}

// bootoutLaunchdService bootouts the gui-domain LaunchAgent for a label and
// verifies the job is actually gone before reporting an error, rather than
// trusting launchctl's exit code alone.
//
// BOS-627: on this macOS build, `launchctl bootout gui/<uid>/<label>` exits 3
// ("No such process") when the service is already unloaded, and a separate,
// unrelated `launchctl list <label>` exits 113 ("Could not find service").
// bootout never returns 113. The prior code special-cased only 113, so every
// bootout of an already-stopped service (which returns 3, or sometimes 5) fell
// through to a hard error. Those codes now live in
// launchctlExitSaysAlreadyGone; the verification lives in bootoutLaunchdTarget.
func bootoutLaunchdService(label string, stillRunning func() bool) error {
	return bootoutLaunchdTarget("gui/"+strconv.Itoa(os.Getuid())+"/"+label, stillRunning)
}

// bootoutLaunchdTarget is bootoutLaunchdService over an arbitrary service
// target, so the system-domain watchdog (BOS-1184) shares one implementation
// with the gui-domain agent rather than carrying a second copy of it.
//
// The duplication mattered because of WHAT was duplicated: which launchctl exit
// codes mean "already gone" is empirical, macOS-version-sensitive knowledge
// (BOS-627 above), and a second copy is a second thing to update when a release
// changes them. The copy had also dropped the nil-probe fail-closed guard
// below, which is the rung that keeps an exit code we do not recognise from
// being reported as a successful teardown.
func bootoutLaunchdTarget(target string, stillRunning func() bool) error {
	out, err := runLaunchctl("bootout", target)
	if err == nil {
		return nil
	}

	if launchctlExitSaysAlreadyGone(err) {
		return nil
	}

	// Without a probe there is no way to verify the job is actually gone, so
	// fail closed -- surface the launchctl error rather than silently reporting
	// success for an exit code (e.g. 5, a generic EIO) that can also mean the
	// job still exists.
	//
	// M7 originally recorded that no production caller passed nil here.
	// BOS-1203 added one: bootoutSupersededLaunchAgent, which boots out another
	// user's gui/<uid> agent from a root install and has no probe it may run in
	// that domain. It accepts the cost knowingly — its call is non-fatal and
	// warning-only — so this arm's fail-closed direction is unchanged, but it is
	// no longer unreachable in production. Note the interaction with
	// launchctlExitSaysAlreadyGone above, which recognises only 3 and 113: the
	// BOS-627 note on bootoutLaunchdService records that an already-gone bootout
	// returns "3, or sometimes 5", and 5 is deliberately excluded there as
	// ambiguous. A nil-probe caller therefore reports a false "may still be
	// loaded" on a 5. Every probe-carrying caller verifies past it.
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
	// BOS-1218: the same discriminator as platformGetStatus, because this is
	// the same command against the same launchd behaviour — and its consumer
	// mcpStillRunningProbe fills the same bootout-verify role. Fixing only the
	// bossd probe would have left one of two identical causes in place.
	st.PID, st.PIDKnown = parseLaunchctlListPID(out, McpLabel)
	st.Running = st.PIDKnown && st.PID > 0
	return st, nil
}

// platformInstall writes the LaunchAgent plist and loads it via launchctl.
// When force is false and the plist already exists, it refuses to overwrite.
func platformInstall(bossdPath string, force bool) error {
	// BOS-1184 U2: the supervision substrate is an explicit choice now, and it
	// is checked FIRST — before a plist path is resolved, a binary is staged or
	// anything is written — so a host whose configured mode cannot be honoured
	// installs nothing at all rather than acquiring a half-installed LaunchAgent
	// it did not ask for.
	//
	// Failing here is the point. A silent fallback to the LaunchAgent would
	// hand a multi-user host exactly the substrate it configured a mode to
	// escape, and report a successful install while doing it. The default
	// resolves clean, so this rung is invisible on every host that has not set
	// the key. Deliberately macOS-only: there is no counterpart in systemd.go,
	// which is what keeps the Linux path untouched.
	st := LoadSupervisionModeStatus()
	if st.Err != nil {
		return fmt.Errorf("daemon supervision mode: %w", st.Err)
	}
	// BOS-1184 U3: the unattended substrate is a different artifact in a
	// different launchd domain, so it gets its own install path rather than a
	// variant of this one. Everything below stays byte-identical for the
	// default mode — an absent key and an explicit "launch-agent" both fall
	// straight through, which is what R5 pins.
	if st.Mode == SupervisionModeUnattended {
		return platformInstallUnattended(bossdPath, force)
	}
	// BOS-1184 R6: the sequence that backs the change out — install the
	// watchdog, set the key back to launch-agent, run `boss daemon install` —
	// otherwise bootstraps a gui/<uid> LaunchAgent while the root
	// system/<label> job is still loaded with KeepAlive=true. Two supervisors
	// then contend for one socket and nothing reports it: status reads the
	// CONFIGURED mode and probes only the gui domain, and
	// LoadSupervisionModeStatus never observes the watchdog once the mode is
	// not unattended. The check stats the plist itself rather than trusting
	// that status struct, which is exactly why it is correct here.
	//
	// BOS-1203 made this symmetric with platformUninstall, which has called
	// removeStrandedWatchdog since BOS-1184. Warning alone left that back-out
	// sequence producing a freshly bootstrapped gui agent beside a root job
	// still loaded with KeepAlive=true — permanently, across reboots — while
	// naming a remedy the operator had to run as a separate command. Under root
	// the job is now removed; without root the observable behaviour is
	// unchanged, because removeStrandedWatchdog degrades to exactly the warning
	// that used to be here.
	if err := removeStrandedWatchdog(st); err != nil {
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
//
// BOS-1184 U3 added one branch and one warning, and both are deliberately
// biased towards REMOVING things:
//
//   - The unattended substrate is torn down by its own path, because its job
//     lives in the `system` domain and its artifacts are root-owned.
//   - A REFUSED supervision mode falls through to the LaunchAgent teardown
//     rather than failing, which is the opposite direction from platformInstall
//     and platformRestart. Those two CREATE a substrate, so a settings value
//     they cannot honour must stop them; this one DESTROYS a substrate, and a
//     host must never be left unable to remove what is installed because of a
//     typo in a file.
func platformUninstall() error {
	st := LoadSupervisionModeStatus()
	if st.Err == nil && st.Mode == SupervisionModeUnattended {
		return platformUninstallUnattended()
	}
	// A watchdog can still be installed here — the key may have been set back
	// to the default with the root-owned job left loaded. Remove it when we
	// have the privilege to, rather than only naming a command that would take
	// this same branch and do nothing. See removeStrandedWatchdog.
	if err := removeStrandedWatchdog(st); err != nil {
		return err
	}

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
	// BOS-1184 U2: the same fail-closed gate platformInstall carries, for the
	// same reason and for the same cost on the default path (none — the default
	// resolves clean, so this rung is invisible on every host that has not set
	// the key).
	//
	// It has to be here as well as in platformInstall because this function
	// restages the binary, rewrites the plist and re-bootstraps it into
	// gui/<uid>: without this check a host whose configured mode is refused
	// would have `boss daemon install` correctly install nothing and then
	// `boss daemon restart` re-establish and load exactly the substrate the
	// configuration refused, reporting success. The gate belongs on every path
	// that creates or re-bootstraps the substrate, not on the first one that
	// happened to get it.
	//
	// It does NOT reach the case of restart installing the LaunchAgent from
	// nothing, and does not need to: restartTakesStandalonePath sends a profile
	// with no service file down the standalone path, which never calls this
	// function.
	//
	// This is the SECOND line of defence, not the first. runDaemonRestart
	// checks the mode before it stops anything; refusing only here would fire
	// after the daemon had already been stopped, which is the BOS-1181 net-loss-
	// of-service shape. See the comment there.
	//
	// platformEnsureRunning is ROUTED rather than gated (BOS-1203): it never
	// refuses over a settings value — the unsupervised fallback stays reachable
	// so a host is never left with no daemon — but it does pick its recovery
	// substrate, keyed on whether a watchdog plist is on disk. That keeps it
	// from bootstrapping the gui/<uid> LaunchAgent beside a root-owned
	// watchdog. See classifyEnsureRunningRoute and
	// docs/ops/daemon-supervision-modes.md.
	st := LoadSupervisionModeStatus()
	if st.Err != nil {
		return fmt.Errorf("daemon supervision mode: %w", st.Err)
	}
	// BOS-1184 U3. Restarting the unattended substrate must NOT come through
	// the path below: that path restages the binary into the per-user staged
	// location and rewrites the gui/<uid> LaunchAgent, so on an unattended host
	// it would re-establish the exact substrate the configuration selected a
	// mode to escape — the same hole the U2 gate closed, one mode later.
	if st.Mode == SupervisionModeUnattended {
		return platformRestartUnattended()
	}
	// Same residue check platformInstall carries, for the same reason: this
	// path re-bootstraps the gui/<uid> LaunchAgent, so on a host that still has
	// the root watchdog loaded it creates the duplicate-supervisor state rather
	// than merely leaving an orphan behind.
	warnIfUnattendedWatchdogInstalled(st)

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
		// Not loaded, or the probe could not be run — launchctl exits non-zero
		// for both. Not running either way, and PIDKnown stays false because
		// nothing about the job's PID was settled (BOS-1218 R2a); telling those
		// two apart is the sibling doctor ticket's job, not this one's.
		return st, nil
	}

	st.PID, st.PIDKnown = parseLaunchctlListPID(out, Label)
	// BOS-1218: the verdict is the PARSED PID, not the exit code above.
	// `launchctl list <label>` exits 0 for a job launchd has merely
	// REGISTERED — measured on the host `delta` on 2026-09-08, where the
	// answer carried no "PID" key at all while `launchctl print` reported
	// `state = not running, runs = 0` — so the old exit-code-only assignment
	// reported Running for a daemon that owned no process. `boss daemon stop`
	// then booted out an empty registration, signalled nothing, and polled a
	// socket held by someone else until LifecycleShutdownTimeout expired.
	//
	// Deriving it here costs no extra process spawn: the PID is parsed out of
	// the same `out` this function already holds. `launchctl print`, which
	// platformSpawnHistory uses, answers a different question and stays there.
	st.Running = st.PIDKnown && st.PID > 0

	return st, nil
}

// parseLaunchctlListPID reads the job's PID out of a `launchctl list <label>`
// answer, reporting whether the answer settled the PID at all.
//
// known is true when the output was recognisable as an answer about this job:
// a plist-style `"key" = value;` dictionary, or the tab-separated
// `<pid> <status> <label>` row of the unfiltered list. Such an answer with no
// PID in it is a job launchd knows and has not spawned — an OBSERVATION, and
// the case BOS-1218 exists for. known is false for output with neither shape.
//
// A `"PID"` key whose value is not a positive integer settles NOTHING and
// forces known false for the whole answer, however many other keys parsed.
// Reading it as "known absent" would rebuild the very bug this function was
// split out to fix one notch over: an unparseable value is not an
// observation, and `readSystemdMainPID` rejects `v <= 0` on the other
// substrate for the same reason. Note what known false then means to the only
// consumers that exist — platformGetStatus and platformMcpGetStatus derive
// Running from it, so both an unreadable answer and a genuinely not-loaded
// label report Running false. Status collapses them BY DESIGN (see
// cmd/daemon_supervision.go, where the same collapse is recorded and scoped to
// the sibling doctor ticket); a probe that wants "could not tell" to fail
// closed cannot get it from this bool alone.
//
// Both shapes feed one PID because both have always been accepted here; the
// dictionary's own `"Label" = "<label>";` line matches the tab-separated
// branch too, and is harmless there because its leading field is not a number.
func parseLaunchctlListPID(out []byte, label string) (pid int, known bool) {
	// Tracked separately from `known` so a malformed PID line can veto an
	// answer whose OTHER lines were perfectly readable.
	sawPIDKey, pidSettled := false, false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "\"PID\"") || strings.HasPrefix(line, "\"pid\"") {
			known = true
			sawPIDKey = true
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				if v, ok := parseLeadingInt(strings.Trim(strings.TrimSpace(parts[1]), "\";")); ok && v > 0 {
					pid = v
					pidSettled = true
				}
			}
			continue
		}
		if strings.HasPrefix(line, "\"") && strings.Contains(line, "=") {
			// A dictionary key that is not the PID: the answer is readable and
			// simply does not name a PID.
			known = true
		}
		// Also try the tab-separated format from `launchctl list | grep`.
		if strings.Contains(line, label) {
			known = true
			if parts := strings.Fields(line); len(parts) >= 1 {
				if v, ok := parseLeadingInt(parts[0]); ok && v > 0 {
					pid = v
					pidSettled = true
				}
			}
		}
	}
	if sawPIDKey && !pidSettled {
		return 0, false
	}
	return pid, known
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

	// BOS-1203: pick the substrate BEFORE touching the LaunchAgent, because on
	// a host running the root-owned watchdog a `launchctl load` here bootstraps
	// a SECOND supervisor beside it — which is the socket-stealing shape the
	// two isSocketReachable guards in this very function were added to prevent.
	//
	// This sits BELOW the early return above, deliberately diverging from
	// platformInstall and platformRestart, which both put their supervision
	// gate at the very top. Their first statement is a gate on an operation the
	// operator explicitly asked for; this function's first statement is a
	// re-probe whose placement is itself the fix for the socket-stealing storm,
	// and this function is reached from newClient — so EVERY boss command runs
	// it. Hoisting a settings read plus the layout stat above the re-probe
	// would put that cost on the steady-state path of every invocation, where
	// the hazard does not exist. Below the guard it is paid only in the
	// socket-down window, which is the only window this routing is for.
	supervision := LoadSupervisionModeStatus()
	watchdogPlistPresent := false
	if _, statErr := os.Stat(watchdogPaths().PlistPath); statErr == nil {
		watchdogPlistPresent = true
	}
	route := classifyEnsureRunningRoute(ensureRunningFacts{
		Mode:                 supervision.Mode,
		ModeErr:              supervision.Err,
		SettingsErr:          supervision.SettingsErr,
		InstallState:         supervision.Unattended.State,
		WatchdogPlistPresent: watchdogPlistPresent,
	})

	if route != ensureRunningRouteLaunchAgent {
		mode, done, watchdogErr := ensureRunningOnWatchdog(socketPath, supervision, route)
		if done {
			return mode, watchdogErr
		}
		// Not root and the watchdog did not serve: fall through to the SAME
		// detached-spawn block the LaunchAgent arm uses (R3). stagedPath is
		// still empty, so that block resolves and stages for itself exactly as
		// it does on a host with no LaunchAgent installed.
	} else if st, err := platformGetStatus(); err == nil && st.Installed && !st.Running {
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
		staged, stageErr := EnsureStaged(sourcePath)
		if stageErr != nil {
			return StartModeUnknown, fmt.Errorf("stage fallback daemon: %w", stageErr)
		}
		bossdPath = staged
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

// watchdogRespawnWait bounds how long platformEnsureRunning waits for the
// root-owned watchdog to bring bossd back up before giving up on it.
//
// It is a package var and NOT LifecycleStartupTimeout, for two reasons that
// both matter. LifecycleStartupTimeout is a const — daemon_test.go asserts its
// relationship to LifecycleShutdownTimeout — so it cannot be shrunk by a test,
// and every test reaching this arm would sleep for real. And at 60s it is sized
// for bossd's own startup after a DELIBERATE start, whereas a watchdog respawn
// is launchd's sub-second KeepAlive plus that same startup on a job that is
// already loaded. Since platformEnsureRunning runs on every boss command, a
// budget sized for the deliberate case would make a watchdog that will never
// serve cost a full minute per invocation.
//
// It is a CEILING that is polled, not a sleep: waitForSocket returns the
// instant the socket answers.
var watchdogRespawnWait = 5 * time.Second

// warnWatchdogKickstartFailed reports a best-effort watchdog restart that did
// not succeed. Package var for the warnDaemonRefreshFailed idiom.
//
// The kickstart is best-effort by design: this is a recovery path, not a
// lifecycle command the operator invoked, and failing the whole call because
// the restart verb errored would turn a degraded start into no start at all.
var warnWatchdogKickstartFailed = func(err error) {
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: could not restart the unattended supervision watchdog (%s): %v\n",
		WatchdogLabel, err)
}

// warnUnattendedWatchdogCannotServe reports a watchdog that is installed but
// cannot serve, so this command is about to start an UNSUPERVISED daemon
// instead.
//
// It is a separate warning from warnUnattendedWatchdogResidue rather than a
// reuse of it. That one's subject is a watchdog installed on a host no longer
// configured for it, and its remedy is to remove the root job or restore the
// key; this one's subject is a watchdog the host still wants whose paths are
// not safe for a root-owned job, and its remedy is to fix the offending path.
// Telling an operator in the second state to run `boss daemon uninstall` would
// be advice for a problem they do not have.
var warnUnattendedWatchdogCannotServe = func(install UnattendedInstall) {
	reason := "it is installed but did not bring the daemon back up"
	if install.Err != nil {
		reason = install.Err.Error()
	}
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: the unattended supervision watchdog cannot serve this host: %s. Starting an UNSUPERVISED bossd instead — it will not come back after a reboot or a crash. Fix the path above and run `%s` to reinstall\n",
		reason, WatchdogInstallCommand)
}

// ensureRunningOnWatchdog is platformEnsureRunning's recovery arm for a host
// that has a watchdog plist on disk. It NEVER resolves platformServicePath and
// NEVER issues `launchctl load`, which is the whole of R1.
//
// done reports whether the returned verdict is final. done=false means this arm
// did what it could and the caller must fall through to the shared
// detached-spawn block — the fail-open property R3 preserves. It is only ever
// false off the root path, because R4 forbids a root detached spawn outright.
//
// The two "the socket came up while we waited" exits report
// StartModeAlreadyRunning rather than StartModeServiceManager. This process
// started nothing on those paths, and StartModeServiceManager's contract claims
// the service manager started the daemon and that it is therefore supervised —
// a claim this arm cannot support and one `boss daemon doctor` would contradict.
// StartModeAlreadyRunning says exactly what happened: supervision is whatever
// the running daemon already had, and this call did not change it.
func ensureRunningOnWatchdog(socketPath string, supervision SupervisionModeStatus, route ensureRunningRoute) (StartMode, bool, error) {
	// A key reverted to the default, a typo of "unattended", or an unreadable
	// settings.json all land here with a root job still loaded. This is the
	// existing residue warning, with its remedy, firing on exactly those arms —
	// it no-ops when the resolved mode IS unattended, which is the ordinary
	// host and needs no warning.
	warnIfUnattendedWatchdogInstalled(supervision)

	wait := route != ensureRunningRouteWatchdogNoWait

	if currentEUID() == 0 {
		// Root: act on the watchdog (R2). platformRestartUnattended kickstarts
		// the system-domain job in place rather than rewriting a root-owned
		// artifact, which is why it is safe from a recovery path.
		if err := platformRestartUnattended(); err != nil {
			warnWatchdogKickstartFailed(err)
		}
		if wait && waitForSocket(socketPath, watchdogRespawnWait) {
			return StartModeAlreadyRunning, true, nil
		}
		layout := watchdogPaths()
		return StartModeUnknown, true, fmt.Errorf(
			"%w: %s is still not being served after restarting %s. Refusing to spawn bossd as root beside the watchdog — "+
				"a root-owned daemon would hold this user's socket, app-data directory and singleton lock, and their own daemon "+
				"could never reclaim them. Check %s/bossd-watchdog.stderr.log, then run `%s` to reinstall the watchdog",
			ErrWatchdogRecoveryFailed, socketPath, WatchdogLabel, layout.LogDir, WatchdogInstallCommand)
	}

	// Not root: there is nothing this process may do to a system-domain job, so
	// wait for the watchdog to win the race and otherwise fall open.
	//
	// The cannot-serve warning fires HERE rather than beside the `wait`
	// assignment above, and the placement is the claim's truth condition: it
	// says this command is about to start an UNSUPERVISED bossd, and the
	// detached spawn it names is reachable only from this arm. Fired before the
	// euid branch it also reached the root arm, which starts nothing and returns
	// ErrWatchdogRecoveryFailed — announcing a spawn that was refused two lines
	// later, with a remedy for a state the operator was not in. The root arm
	// carries its own explanation in that error instead.
	if !wait {
		warnUnattendedWatchdogCannotServe(supervision.Unattended)
	}
	if wait && waitForSocket(socketPath, watchdogRespawnWait) {
		return StartModeAlreadyRunning, true, nil
	}
	return StartModeUnknown, false, nil
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
// BOS-1183: this is the only probe here that can tell a job launchd has never
// TRIED to spawn from one that started and died. `launchctl list <label>` —
// which platformGetStatus uses, and which this function deliberately does not
// touch — exits 0 for a job launchd has loaded into a domain it will never
// spawn anything in. BOS-1218 narrowed Status.Running to require a PID out of
// that same answer, so such a job no longer reports Running; what `list` still
// cannot say is WHY there is no PID, because a job launchd never attempted and
// one whose process exited answer alike. `launchctl print` carries `runs` and
// `last exit code`, which separate "launchd never tried" from "bossd started
// and died".
//
// Error discipline (see GetSpawnHistory): a non-nil error means launchctl could
// not be EXECUTED. A launchctl that ran and exited non-zero — the "could not
// find service in domain" case — is a nil error with an unknown verdict, since
// whether the job is registered at all is a fact Status.Installed already owns.
// Both paths still return a populated, fail-closed SpawnHistory.
func platformSpawnHistory() (SpawnHistory, error) {
	// BOS-1204: resolved from the configured substrate, because under
	// `unattended` the job launchd actually spawns is the root-owned watchdog
	// in the `system` domain and gui/<uid> carries no history at all. The
	// resolution happens ahead of the skip check below so the short-circuit
	// still NAMES the target this host would have probed — a skip line that
	// named the LaunchAgent on a machine that has none would be a report about
	// the wrong job.
	target := spawnHistoryTarget(LoadSupervisionModeStatus(),
		"gui/"+strconv.Itoa(os.Getuid())+"/"+Label)

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

// launchctlProbeTimeout bounds the ADVISORY launchctl reads on the diagnostic
// path. Three seconds because `launchctl print-disabled` is a local IPC read
// against launchd — it either answers immediately or it is wedged, and no
// useful answer arrives after that.
//
// BOS-1222 R7 forbids an unbounded advisory subprocess on the diagnostic path,
// and BOS-864 recorded why: an unbounded one caused a multi-minute silent
// stall in this codebase. A diagnostic an operator runs when things are
// already wrong must degrade to "not checked", never hang.
//
// A var and not a const, following the seam idiom the rest of this file uses:
// with it pinned as a const no test could drive a REAL deadline expiry through
// runLaunchctlBoundedProbe, so the %w wrap and the ctx.Err() check below were
// exercised only by a stub that hand-fabricated the wrapped error — a test
// that would pass byte-identically against a %v wrap, or against the check
// deleted outright. Production never assigns it.
var launchctlProbeTimeout = 3 * time.Second

// runLaunchctlBoundedProbe invokes launchctl under a hard deadline and returns
// its combined output.
//
// A SEPARATE seam from runLaunchctl, deliberately. runLaunchctl carries the
// lifecycle verbs — bootstrap, bootout, load — whose latency is the operator's
// own action and whose timeout semantics are not this ticket's to change.
// This one carries only bounded read-only probes, so the deadline can be short
// without any risk of cutting a lifecycle operation short.
//
// A deadline expiry is surfaced as a context.DeadlineExceeded-wrapping error
// rather than left as the "signal: killed" ExitError exec.CommandContext
// produces, because a caller that cannot tell a timeout from a refusal cannot
// name the timeout in its Reason.
var runLaunchctlBoundedProbe = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), launchctlProbeTimeout)
	defer cancel()
	// #nosec G204 -- launchctl; const argv verbs plus derived int uid domains; no shell
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("launchctl %s did not answer within %s: %w",
			strings.Join(args, " "), launchctlProbeTimeout, context.DeadlineExceeded)
	}
	return out, err
}

// platformJobDisabled reads whether launchd holds a disable override for the
// job it would spawn.
//
// BOS-1222: this is one of the candidate causes of a never-spawned job that
// doctor can settle cheaply, instead of asserting a different one it never
// measured. A disabled job is decisive — launchd will not spawn it whatever
// else is true of the domain.
//
// Error discipline (see GetJobDisabled): a non-nil error means launchctl could
// not be EXECUTED. A launchctl that ran and exited non-zero, and a launchctl
// that ran past the deadline, are both nil errors with an unknown verdict:
// "we asked and could not tell". Every path returns a populated, fail-closed
// JobDisabled that is never JobDisabledStateEnabled.
func platformJobDisabled() (JobDisabled, error) {
	// Resolved BEFORE the skip check for the same reason platformSpawnHistory
	// resolves its target first: the short-circuit must still NAME the domain
	// this host would have probed, or the report describes the wrong job.
	domain, label := jobDisabledDomain(LoadSupervisionModeStatus(),
		"gui/"+strconv.Itoa(os.Getuid()), Label)

	if skipLaunchctl() {
		return JobDisabled{
			State:  JobDisabledStateUnknown,
			Domain: domain,
			Label:  label,
			Reason: "service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL",
		}, nil
	}

	out, err := runLaunchctlBoundedProbe("print-disabled", domain)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Degrade to "not checked" and name the bound. A slow launchd must
			// never hold doctor open (R7).
			return JobDisabled{
				State:  JobDisabledStateUnknown,
				Domain: domain,
				Label:  label,
				Reason: fmt.Sprintf("launchctl print-disabled %s did not answer within %s", domain, launchctlProbeTimeout),
			}, nil
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return JobDisabled{
				State:  JobDisabledStateUnknown,
				Domain: domain,
				Label:  label,
				Reason: fmt.Sprintf("could not run launchctl print-disabled %s: %v", domain, err),
			}, fmt.Errorf("launchctl print-disabled %s: %w", domain, err)
		}
		return JobDisabled{
			State:  JobDisabledStateUnknown,
			Domain: domain,
			Label:  label,
			Reason: fmt.Sprintf("launchctl print-disabled %s exited %d: %q", domain, exitErr.ExitCode(), strings.TrimSpace(string(out))),
		}, nil
	}

	state, reason := parseLaunchdDisabledServices(out, label)
	return JobDisabled{State: state, Domain: domain, Label: label, Reason: reason}, nil
}
