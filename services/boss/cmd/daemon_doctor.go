package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/spf13/cobra"
)

var daemonDoctorGOOS = runtime.GOOS

var errDaemonDoctorUnhealthy = errors.New("daemon doctor found unhealthy state")

// findDaemonProcess resolves a recorded PID to something signalable. It is a
// package var over the same processSignaler interface handlers.go uses, so
// doctor's liveness probe is deterministic in tests without picking a real PID
// (mirrors daemon.findMcpProcess). BOS-864.
var findDaemonProcess = func(pid int) (processSignaler, error) {
	return os.FindProcess(pid)
}

// daemonProcessAlive probes a recorded PID with signal 0. It deliberately does
// not shell out to `ps` for a start-time cross-check: an unbounded advisory
// subprocess inside a diagnostic has already caused a multi-minute silent
// stall in this codebase. PID reuse is an accepted, recorded risk.
func daemonProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("no PID recorded")
	}
	process, err := findDaemonProcess(pid)
	if err != nil {
		return false, err
	}
	switch err := process.Signal(syscall.Signal(0)); {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		// The process exists but belongs to another user. Alive, not ours.
		return true, nil
	default:
		return false, err
	}
}

// daemonStalenessGOOS mirrors daemonDoctorGOOS so the non-darwin no-op is
// testable on a darwin CI machine. Staging is darwin-only and stays that way.
var daemonStalenessGOOS = runtime.GOOS

// skipDaemonStalenessWarningEnv suppresses the startup warning for scripted
// use, following the BOSS_DAEMON_SKIP_LAUNCHCTL precedent.
const skipDaemonStalenessWarningEnv = "BOSS_DAEMON_SKIP_STALE_WARNING"

// daemonStalenessWarningText is the single line every `boss` subcommand may
// emit when the daemon is running a build older than the installed one.
const daemonStalenessWarningText = "boss: bossd is running an older build than the one installed — " +
	"run 'boss daemon restart' (details: boss daemon doctor)"

// daemonStalenessWarningRemedyCommands are the commands that are themselves the
// remedy. Warning on them is noise at best and misleading at worst.
var daemonStalenessWarningRemedyCommands = []string{
	"boss daemon doctor",
	"boss daemon restart",
	"boss daemon start",
	"boss upgrade",
}

// daemonStalenessWarningApplies reports whether a command path should carry the
// warning, using the cmd.CommandPath() prefix idiom rootCmd already uses.
func daemonStalenessWarningApplies(commandPath string) bool {
	for _, remedy := range daemonStalenessWarningRemedyCommands {
		if commandPath == remedy || strings.HasPrefix(commandPath, remedy+" ") {
			return false
		}
	}
	return true
}

// warnIfDaemonBinaryStale writes at most one line to stderr when the running
// bossd is behind the installed build. It is called from rootCmd's
// PersistentPreRunE and must never fail a command: every error inside is
// swallowed, and nothing here ever writes to the staged path.
//
// stderr specifically, never cmd.OutOrStdout(): --json commands write machine
// output to stdout and a warning there would corrupt every JSON consumer.
func warnIfDaemonBinaryStale(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	if daemonStalenessGOOS != "darwin" {
		return
	}
	if os.Getenv(skipDaemonStalenessWarningEnv) != "" {
		return
	}
	if !daemonStalenessWarningApplies(cmd.CommandPath()) {
		return
	}
	staleness, ok := inspectDaemonStaleness()
	if !ok {
		return
	}
	if (staleness.StagedKnown && staleness.StagedBehindSource) ||
		(staleness.RunningKnown && staleness.RunningBehindStaged) {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), daemonStalenessWarningText)
	}
}

// inspectDaemonStaleness resolves the source, staged and running-daemon inputs
// and classifies them. ok is false whenever any input is unavailable, so a
// caller can never mistake a failed lookup for a healthy verdict.
//
// The Cellar guard runs before any comparison: a developer running ./bin/boss
// in a checkout legitimately resolves a source binary that differs from the
// released staged copy, and an unguarded warning would nag on every dev
// invocation (BOS-696's "silently downgrading a dev build" objection, applied
// to the warning itself).
func inspectDaemonStaleness() (daemonbin.Staleness, bool) {
	sourcePath, err := daemon.ResolveBossdPath()
	if err != nil {
		return daemonbin.Staleness{}, false
	}
	if !daemonbin.IsHomebrewCellarBinary(sourcePath) {
		return daemonbin.Staleness{}, false
	}
	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		return daemonbin.Staleness{}, false
	}
	var startedAt time.Time
	if profile, profileErr := currentDaemonProfile(); profileErr == nil {
		if metadata, readErr := daemonstate.Read(profile.AppDataDir); readErr == nil {
			startedAt = metadata.StartedAt
		}
	}
	staleness, err := daemonbin.Inspect(sourcePath, daemonbin.StagedPath(appDataDir), startedAt)
	if err != nil {
		return daemonbin.Staleness{}, false
	}
	return staleness, true
}

// daemonRunningImageLine renders the staged-file-versus-running-process
// distinction for `boss daemon status`. Empty when it cannot be determined at
// all, so status never states something it did not verify.
func daemonRunningImageLine(startedAt time.Time) string {
	if daemonStalenessGOOS != "darwin" {
		return ""
	}
	sourcePath, err := daemon.ResolveBossdPath()
	if err != nil {
		return ""
	}
	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		return ""
	}
	staleness, err := daemonbin.Inspect(sourcePath, daemonbin.StagedPath(appDataDir), startedAt)
	if err != nil {
		return ""
	}
	switch {
	case !staleness.RunningKnown:
		return fmt.Sprintf("running image: unknown (%s)", staleness.Reason)
	case staleness.RunningBehindStaged:
		return "running image: stale — the process started before the staged binary was written; run 'boss daemon restart'"
	default:
		return "running image: up to date"
	}
}

func daemonDoctorTimestamp(when time.Time) string {
	if when.IsZero() {
		return "unknown"
	}
	return when.Format(time.RFC3339)
}

// daemonDoctorToolLine renders one "does the daemon see this tool" line and
// reports whether it resolved. A missing tool is deliberately NOT counted as an
// unhealthy daemon: not every machine installs both, and doctor's exit status
// is reserved for states the remediation block can actually fix.
func daemonDoctorToolLine(servicePath, tool string) (string, bool) {
	resolved, ok := daemon.LookPathIn(servicePath, tool)
	if !ok {
		return fmt.Sprintf("  %s: not found on the service PATH", tool), false
	}
	return fmt.Sprintf("  %s: %s", tool, resolved), true
}

// reportDaemonServicePath prints the PATH the SERVICE uses and resolves the
// agent-runner tools under THAT path. Resolving node/claude under the caller's
// interactive shell is exactly the check that passed on the machine whose
// daemon could not run a single `node` cron gate (BOS-880).
//
// It distinguishes two PATHs that are easy to conflate and were the whole bug:
// the one recorded in the INSTALLED service file, which is what the running
// daemon actually has, and the one the NEXT restart will write. Tools resolve
// against the installed value whenever it is readable, so this diagnostic
// cannot report a tool as visible while the live daemon still cannot see it.
//
// It runs on EVERY platform, ahead of the macOS-only checks below: the Linux
// systemd unit now carries an explicit PATH too, so a Linux operator needs this
// answer just as much as a macOS one.
//
// Returns true when a restart is required to pick up a changed PATH.
func reportDaemonServicePath(out io.Writer) bool {
	nextPath := daemon.ServiceEnvPath()
	installedPath, installedOK := daemon.InstalledServiceEnvPath()

	resolveAgainst := nextPath
	switch {
	case !installedOK:
		_, _ = fmt.Fprintf(out, "service PATH (next restart): %s\n", nextPath)
		_, _ = fmt.Fprintln(out, "service PATH (installed): unknown (no service file, or it sets no PATH)")
	case installedPath == nextPath:
		_, _ = fmt.Fprintf(out, "service PATH: %s\n", installedPath)
		resolveAgainst = installedPath
	default:
		_, _ = fmt.Fprintf(out, "service PATH (installed): %s\n", installedPath)
		_, _ = fmt.Fprintf(out, "service PATH (next restart): %s\n", nextPath)
		resolveAgainst = installedPath
	}

	for _, tool := range []string{"node", "claude"} {
		line, _ := daemonDoctorToolLine(resolveAgainst, tool)
		_, _ = fmt.Fprintln(out, line)
	}

	return installedOK && installedPath != nextPath
}

func runDaemonDoctor(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	// A stale service PATH is a real unhealthy state with an actionable remedy
	// (restart), and it is precisely the state BOS-880 left every existing
	// install in until its first restart after the upgrade.
	servicePathStale := reportDaemonServicePath(out)
	if servicePathStale {
		_, _ = fmt.Fprintln(out, "service PATH: stale — the running daemon still uses the installed PATH; run 'boss daemon restart'")
	}
	if daemonDoctorGOOS != "darwin" {
		_, _ = fmt.Fprintf(out, "macOS daemon install and protected-folder checks: not applicable on %s\n", daemonDoctorGOOS)
		// The service-PATH check is NOT macOS-specific — the systemd unit carries
		// an explicit PATH too — so its verdict has to survive this early return.
		// Returning nil here regardless would make stale detection a no-op on
		// exactly the platform the PATH line is new on.
		if servicePathStale {
			_, _ = fmt.Fprintln(out, "\nRemediation:")
			_, _ = fmt.Fprintln(out, "  run 'boss daemon restart'")
			return errDaemonDoctorUnhealthy
		}
		return nil
	}
	unhealthy := servicePathStale
	permissionRemediation := false
	installRemediation := false
	startRemediation := false

	stagedAppDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		return fmt.Errorf("resolve app data directory: %w", err)
	}
	stagedPath := daemonbin.StagedPath(stagedAppDataDir)

	sourcePath, sourceErr := daemon.ResolveBossdPath()
	if sourceErr != nil {
		unhealthy = true
		_, _ = fmt.Fprintf(out, "FAIL source bossd: %v\n", sourceErr)
	} else {
		_, _ = fmt.Fprintf(out, "source bossd: %s\n", sourcePath)
	}

	if sourceErr == nil {
		needsStage, reason, compareErr := daemonbin.NeedsStage(sourcePath, stagedPath)
		switch {
		case compareErr != nil:
			unhealthy = true
			_, _ = fmt.Fprintf(out, "FAIL staged bossd: %s (comparison failed: %v)\n", stagedPath, compareErr)
		case needsStage:
			unhealthy = true
			_, _ = fmt.Fprintf(out, "staged bossd: %s — stale (%s)\n", stagedPath, reason)
		default:
			_, _ = fmt.Fprintf(out, "staged bossd: %s — up to date\n", stagedPath)
		}
	} else {
		_, _ = fmt.Fprintf(out, "staged bossd: %s — source unavailable\n", stagedPath)
	}

	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		unhealthy = true
		_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist: resolve home directory: %v\n", homeErr)
	} else {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
		programArguments, plistErr := readLaunchAgentProgramArguments(plistPath)
		switch {
		case plistErr != nil:
			unhealthy = true
			installRemediation = errors.Is(plistErr, os.ErrNotExist)
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist %s: %v\n", plistPath, plistErr)
		case len(programArguments) == 0:
			unhealthy = true
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent ProgramArguments: no executable configured in %s\n", plistPath)
		default:
			programPath := programArguments[0]
			_, _ = fmt.Fprintf(out, "LaunchAgent ProgramArguments: %s\n", strings.Join(programArguments, " "))
			if strings.Contains(programPath, "/Cellar/") || filepath.Clean(programPath) != filepath.Clean(stagedPath) {
				unhealthy = true
				_, _ = fmt.Fprintf(out, "FAIL LaunchAgent executable must be staged at %s, not %s\n", stagedPath, programPath)
			}
		}
	}

	var metadata daemonstate.Metadata
	profile, profileErr := currentDaemonProfile()
	metadataErr := profileErr
	if profileErr == nil {
		metadata, metadataErr = daemonstate.Read(profile.AppDataDir)
	}

	// The staged *file* verdict above and the running *process* verdict below
	// are two independent facts. Conflating them is what let BOS-864 report a
	// fully healthy daemon that was executing a 20-hour-old binary: after a
	// failed restart the file is current while the live process is not, and a
	// recorded PID was printed as running without ever being probed.
	switch {
	case metadataErr != nil:
		_, _ = fmt.Fprintf(out, "running bossd: unknown (no daemon state record: %v)\n", metadataErr)
	case metadata.ExecutablePath == "" || metadata.PID <= 0:
		_, _ = fmt.Fprintln(out, "running bossd: unknown (daemon state record names no running process)")
	default:
		alive, probeErr := daemonProcessAlive(metadata.PID)
		switch {
		case probeErr != nil:
			_, _ = fmt.Fprintf(out, "running executable: %s (PID %d) — unknown (liveness probe failed: %v)\n",
				metadata.ExecutablePath, metadata.PID, probeErr)
		case !alive:
			unhealthy = true
			startRemediation = true
			_, _ = fmt.Fprintf(out, "running executable: %s (PID %d) — not running (the recorded process is gone; this daemon state record is stale)\n",
				metadata.ExecutablePath, metadata.PID)
		default:
			staleness, inspectErr := daemonbin.Inspect(sourcePath, stagedPath, metadata.StartedAt)
			switch {
			case inspectErr != nil:
				_, _ = fmt.Fprintf(out, "running executable: %s (PID %d) — unknown (running-image check failed: %v)\n",
					metadata.ExecutablePath, metadata.PID, inspectErr)
			case !staleness.RunningKnown:
				_, _ = fmt.Fprintf(out, "running executable: %s (PID %d) — unknown (%s)\n",
					metadata.ExecutablePath, metadata.PID, staleness.Reason)
			case staleness.RunningBehindStaged:
				unhealthy = true
				_, _ = fmt.Fprintf(out, "running executable: %s (PID %d) — stale: the process started %s but the staged binary was written %s\n",
					metadata.ExecutablePath, metadata.PID,
					daemonDoctorTimestamp(metadata.StartedAt), daemonDoctorTimestamp(staleness.StagedModTime))
			default:
				_, _ = fmt.Fprintf(out, "running executable: %s (PID %d) — up to date (started %s)\n",
					metadata.ExecutablePath, metadata.PID, daemonDoctorTimestamp(metadata.StartedAt))
			}
		}
	}

	switch {
	case metadataErr != nil || (!metadata.TCCProbeCompleted && len(metadata.TCCProbeResults) == 0):
		_, _ = fmt.Fprintln(out, "protected root status: unavailable (bossd has not yet recorded startup probe results)")
	case len(metadata.TCCProbeResults) == 0:
		_, _ = fmt.Fprintln(out, "protected root status: no protected roots require access (bossd startup probe completed)")
	default:
		for _, result := range metadata.TCCProbeResults {
			_, _ = fmt.Fprintf(out, "protected root %s: %s", result.Path, result.Status)
			if result.Diagnostic != "" && result.Status != daemonstate.TCCProbeStatusAbsent {
				_, _ = fmt.Fprintf(out, " (%s)", result.Diagnostic)
			}
			_, _ = fmt.Fprintln(out)
			if result.Status == daemonstate.TCCProbeStatusDenied || result.Status == daemonstate.TCCProbeStatusBlocked || result.Status == daemonstate.TCCProbeStatusError {
				unhealthy = true
			}
			if result.Status == daemonstate.TCCProbeStatusDenied || result.Status == daemonstate.TCCProbeStatusBlocked {
				permissionRemediation = true
			}
		}
	}

	if unhealthy {
		_, _ = fmt.Fprintln(out, "\nRemediation:")
		switch {
		case installRemediation:
			_, _ = fmt.Fprintln(out, "  run 'boss daemon install'")
		case startRemediation:
			// Nothing is running, so there is nothing to restart. This is the
			// recovery from a restart whose bootstrap failed after its bootout.
			_, _ = fmt.Fprintln(out, "  run 'boss daemon start'")
		default:
			_, _ = fmt.Fprintln(out, "  run 'boss daemon restart'")
		}
		if permissionRemediation {
			_, _ = fmt.Fprintln(out, "  Open System Settings > Privacy & Security > Files and Folders (or Full Disk Access).")
			_, _ = fmt.Fprintf(out, "  Grant access to %s — look for the staged path, not a Homebrew/Cellar path.\n", stagedPath)
		}
	}

	if unhealthy {
		return errDaemonDoctorUnhealthy
	}
	return nil
}

func readLaunchAgentProgramArguments(plistPath string) ([]string, error) {
	// #nosec G304 -- the path is the fixed per-user bossd LaunchAgent path.
	// owner=@recurser review-by=2027-02-04 issue=BOS-696
	file, err := os.Open(plistPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	decoder := xml.NewDecoder(file)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("ProgramArguments key not found")
		}
		if err != nil {
			return nil, fmt.Errorf("parse plist: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return nil, fmt.Errorf("parse plist key: %w", err)
		}
		if key != "ProgramArguments" {
			continue
		}
		return decodePlistStringArray(decoder)
	}
}

func decodePlistStringArray(decoder *xml.Decoder) ([]string, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("parse ProgramArguments: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "array" {
			return nil, fmt.Errorf("ProgramArguments is not an array")
		}

		var arguments []string
		for {
			token, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("parse ProgramArguments array: %w", err)
			}
			switch element := token.(type) {
			case xml.StartElement:
				if element.Name.Local != "string" {
					continue
				}
				var argument string
				if err := decoder.DecodeElement(&argument, &element); err != nil {
					return nil, fmt.Errorf("parse ProgramArguments value: %w", err)
				}
				arguments = append(arguments, argument)
			case xml.EndElement:
				if element.Name.Local == "array" {
					return arguments, nil
				}
			}
		}
	}
}
