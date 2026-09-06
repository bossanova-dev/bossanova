package main

import (
	"context"
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
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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

// daemonAuthProbeTimeout bounds the GetAuthState call. Short on purpose: this
// runs inside a diagnostic, and the failure mode it is checking for is a
// daemon that does not answer. A doctor that hangs waiting for a wedged daemon
// has become the problem it was written to detect.
const daemonAuthProbeTimeout = 5 * time.Second

// daemonAuthStateProbe asks the LOCAL daemon for its live auth state. It is a
// package var over the same seam idiom as findDaemonProcess so the rendering
// below is testable without a running daemon.
//
// It deliberately dials the daemon rather than reading the credential record
// from disk. A local read is architecturally incapable of catching the fault
// this check exists for: through the whole BOS-942 incident the
// `workos-tokens-v1` record was present and parseable while the daemon failed
// to re-register every 30 seconds. Only the daemon knows.
var daemonAuthStateProbe = func(ctx context.Context) (*pb.GetAuthStateResponse, error) {
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	return client.NewLocal(socketPath).GetAuthState(ctx)
}

// daemonAuthCheckUnsupported reports whether err means "this daemon predates
// the RPC" rather than "this daemon is broken".
//
// The two must not be conflated. A boss CLI upgraded ahead of its daemon —
// which is the normal state for the minutes between an upgrade and a restart —
// gets CodeUnimplemented, and reporting that as an auth fault would fire the
// check on every upgrade and teach operators to ignore it. It is also exactly
// the scope carve-out where a checker goes quiet, so the caller says so on its
// own line instead of silently skipping.
func daemonAuthCheckUnsupported(err error) bool {
	return connect.CodeOf(err) == connect.CodeUnimplemented
}

// daemonDoctorSince renders how long ago when was, for an operator. It refuses
// to render a duration it cannot believe: a negative one (the daemon's clock is
// ahead of ours, or the timestamp is garbage) would otherwise print as
// something like "-3h0m0s ago", which reads as a typo rather than as a problem
// and invites the reader to ignore the whole line.
func daemonDoctorSince(when, now time.Time) string {
	if when.IsZero() {
		return "never"
	}
	elapsed := now.Sub(when)
	if elapsed < 0 {
		return "unknown duration"
	}
	return elapsed.Truncate(time.Second).String()
}

// daemonDoctorMaxFieldLen bounds any daemon-supplied string this command
// renders. The daemon is not hostile, but it is a separate process whose
// fields reach an operator's terminal verbatim, and a value long enough to
// scroll the diagnosis off the screen destroys the diagnostic just as
// effectively as a wrong one would.
const daemonDoctorMaxFieldLen = 120

// sanitizeDaemonDoctorField makes a daemon-supplied string safe to print on a
// single terminal line: every non-printable rune becomes a space, runs of
// whitespace collapse, and the result is truncated.
//
// The predicate is !unicode.IsPrint rather than unicode.IsControl, which is
// the same shape lib/bossalib/broadcast/selector.go settled on. IsControl
// covers only category Cc, so it lets through exactly the runes this function
// exists to stop: U+2028/U+2029 (Zl/Zp) break the one-line-per-check shape
// just as a newline does, and the Cf bidi overrides (U+202E, U+2066-U+2069)
// reorder the rendered verdict around the text they were injected into.
// IsPrint admits the ASCII space, which the branch below handles anyway.
//
// Know the cost of that predicate before reusing this helper: IsPrint excludes
// ALL of category Cf, not only the bidi controls — U+200D ZWJ, U+200C ZWNJ and
// U+FE0F go too. That is free for the two fields it receives today (an
// enumerated relogin marker and a Go dial error, both ASCII), and wrong for
// free text: pointed at a repo name, a branch, or an upstream message it would
// break emoji ZWJ sequences and correct Indic/Arabic rendering. Keep it to
// machine-generated fields, or narrow the Cf removal first.
//
// It only ever REMOVES material — it must never add any, because AC #15 says
// no token, header value, or upstream response body may reach this output, and
// a "helpful" transformation is how such material gets reintroduced. The
// specific hazards are a multi-line transport error that breaks the one-line-
// per-check shape doctor's readers scan for, and an embedded escape sequence
// that repaints the terminal around the verdict.
func sanitizeDaemonDoctorField(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) || r == ' ' {
			// One space stands in for any run of whitespace or non-printable
			// characters, so a stack-trace-shaped error collapses instead of
			// wrapping the section.
			if !space {
				b.WriteRune(' ')
				space = true
			}
			continue
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > daemonDoctorMaxFieldLen {
		out = strings.TrimSpace(string([]rune(out)[:daemonDoctorMaxFieldLen])) + "…"
	}
	return out
}

// daemonMetadataForDoctor resolves the recorded daemon state the same way the
// macOS block below does. It is read twice rather than hoisted because the
// supervision check has to run ABOVE the platform early return and the rest of
// the metadata consumers sit below it; two cheap file reads are a better trade
// than reordering a function whose ordering is documented as load-bearing.
func daemonMetadataForDoctor() (daemonstate.Metadata, error) {
	profile, profileErr := currentDaemonProfile()
	if profileErr != nil {
		return daemonstate.Metadata{}, profileErr
	}
	return daemonstate.Read(profile.AppDataDir)
}

// reportDaemonSupervision answers a question none of the other checks ask: is
// the bossd that is actually running the one the service manager started?
//
// Every other check here reads CONFIGURATION — the plist's PATH, the plist's
// ProgramArguments, the staged binary's mtime — and a detached daemon matches
// all of it, because it is the same executable at the same path. On 2026-09-03
// that gap let `boss daemon doctor` exit 0 while the running daemon could not
// authenticate to GitHub and was writing every log line to /dev/null. A
// diagnostic that checks configuration rather than the live process reports
// healthy on a process the configuration does not describe.
//
// The three consequences travel together because they have one cause. A bossd
// started detached (over SSH, or by hand) is outside the GUI login session, so
// on macOS its gh subprocesses cannot read the login keychain and fall back to
// UNAUTHENTICATED requests — which surfaces as 401s on writes and anonymous
// rate limits on reads — and it inherits whatever stdio the starting shell had
// rather than the service's log files.
//
// Every inconclusive input returns "unknown" rather than a failure. This check
// runs on developer machines and in CI, where BOSS_DAEMON_SKIP_LAUNCHCTL makes
// the service view meaningless; a false FAIL there would train operators to
// ignore the one line that matters on a real host.
func reportDaemonSupervision(out io.Writer, metadata daemonstate.Metadata, metadataErr error) (unhealthy bool, restartRemediation bool) {
	// Checked BEFORE the service view is read, not after. Under this env var
	// platformGetStatus deliberately returns Installed=true, Running=false
	// without ever asking launchd or systemd — which is byte-identical to the
	// detached-daemon shape this function exists to flag. Interpreting it would
	// turn every test harness and CI run into a FAIL, which is precisely how a
	// diagnostic gets ignored on the host where it is telling the truth.
	if os.Getenv("BOSS_DAEMON_SKIP_LAUNCHCTL") != "" {
		_, _ = fmt.Fprintln(out, "daemon supervision: unknown (service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL)")
		return false, false
	}

	st, statusErr := daemonGetStatus()
	switch {
	case statusErr != nil:
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (service status unavailable: %v)\n", statusErr)
		return false, false
	case metadataErr != nil:
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (no daemon state record: %v)\n", metadataErr)
		return false, false
	case !st.Installed:
		// "not installed" has its own check and its own remedy below; claiming
		// it here too would print two failures for one fact.
		_, _ = fmt.Fprintln(out, "daemon supervision: unknown (no service is installed)")
		return false, false
	case metadata.PID <= 0:
		_, _ = fmt.Fprintln(out, "daemon supervision: unknown (no recorded daemon PID)")
		return false, false
	}

	alive, aliveErr := daemonProcessAlive(metadata.PID)
	switch {
	case aliveErr != nil:
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (recorded PID %d could not be checked: %v)\n", metadata.PID, aliveErr)
		return false, false
	case !alive:
		// Nothing is running under the recorded PID. The running-process check
		// below owns that story.
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (recorded PID %d is not running)\n", metadata.PID)
		return false, false
	}

	switch {
	case !st.Running:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: FAIL bossd (PID %d) is running but the service manager does not own it — it was started detached, so on macOS it cannot reach the login keychain and gh silently falls back to unauthenticated requests\n",
			metadata.PID)
		return true, true
	case st.PID == 0:
		// Reachable on systemd when `systemctl is-active` succeeds but the
		// MainPID read does not, and on launchd when its output cannot be
		// parsed. Certifying ownership here would emit a false healthy verdict
		// from the one check added to detect an ownership mismatch.
		_, _ = fmt.Fprintf(out,
			"daemon supervision: unknown (the service manager reports running but did not report a PID; recorded daemon is PID %d)\n",
			metadata.PID)
		return false, false
	case st.PID != metadata.PID:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: FAIL the service manager owns PID %d but the recorded daemon is PID %d — two daemons, or a stale state record\n",
			st.PID, metadata.PID)
		return true, true
	default:
		_, _ = fmt.Fprintf(out, "daemon supervision: ok (PID %d is owned by the service manager)\n", metadata.PID)
		return false, false
	}
}

// reportDaemonAuthState prints the live-auth section and reports whether it
// found an unhealthy state and whether the fix is a login.
//
// The FAIL discriminator is `relogin_reason set OR auth_failing_since set`,
// and both halves are load-bearing:
//
//   - auth_failing_since alone, with needs_login=false, is the BOS-942 shape
//     precisely — the daemon does not think it needs a login, so nothing else
//     in the system will ever prompt for one.
//   - relogin_reason alone is a daemon whose refresh was rejected before any
//     failure clock started. markNeedsRelogin always persists a reason; only
//     the deliberate `boss logout` path (NotifyLogout) persists none. Treating
//     a reason as benign would print the reassuring signed-out line, and exit
//     zero, for a daemon that cannot authenticate.
//
// needs_login is NOT a fault on its own, and upstream_connected is false during
// every ordinary reconnect gap on a healthy daemon. needs_login=true with
// neither a reason nor a clock is exactly what a deliberate sign-out looks
// like; rendering that as a red FAIL would fire the check on every logout and
// teach operators to ignore it.
func reportDaemonAuthState(ctx context.Context, out io.Writer) (unhealthy bool, loginRemediation bool) {
	// Derived from the caller's context, not from Background: doctor is a
	// subcommand, and a probe that ignores cancellation keeps a Ctrl-C'd
	// command alive for its whole timeout waiting on the daemon it was about
	// to report as unresponsive.
	//
	// The nil check is not defensive padding: cobra's Command.Context() hands
	// back a nil context on a Command that was never Execute()d — which is how
	// this package's own tests build one — and context.WithTimeout panics on a
	// nil parent. A diagnostic must not be the thing that crashes.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, daemonAuthProbeTimeout)
	defer cancel()

	state, err := daemonAuthStateProbe(ctx)
	switch {
	case err != nil && daemonAuthCheckUnsupported(err):
		_, _ = fmt.Fprintln(out, "daemon auth: unknown — this daemon predates the check; upgrade it and run 'boss daemon restart'")
		return false, false
	case err != nil:
		// Could-not-evaluate is its own outcome, not a pass. Say so rather
		// than printing nothing, which would leave the section looking
		// checked-and-clean.
		//
		// It is not counted as unhealthy on its own, because the commonest
		// cause is a daemon that is not running and "run 'boss login'" is the
		// wrong remedy for that. Be honest about the cost: on darwin the
		// install/start/liveness checks below own that diagnosis with a better
		// remedy, but those checks are darwin-only, so on Linux a fully
		// stopped daemon renders here as a non-fatal "unknown" and the command
		// still exits 0. Nothing else in this doctor currently owns that case.
		// Closing it properly needs a platform-neutral liveness check with a
		// start-shaped remedy, which is a larger change than this diagnostic —
		// do not "fix" it by returning unhealthy here, which would print the
		// login remedy for a stopped daemon.
		_, _ = fmt.Fprintf(out, "daemon auth: unknown — could not reach the daemon (%s)\n",
			sanitizeDaemonDoctorField(err.Error()))
		return false, false
	}

	if !state.GetUpstreamConfigured() {
		_, _ = fmt.Fprintln(out, "daemon auth: not configured (local-only daemon)")
		return false, false
	}

	now := time.Now()
	// An unset timestamp must be read from the FIELD, not from AsTime(): a nil
	// timestamppb answers AsTime() with the Unix epoch, not the zero Time, so
	// piping it straight into daemonDoctorSince renders "a daemon that has
	// never registered" as a 55-year duration.
	var registeredAt time.Time
	if ts := state.GetLastRegisteredAt(); ts != nil {
		registeredAt = ts.AsTime()
	}
	registered := daemonDoctorSince(registeredAt, now)

	// Discriminate on the RAW field and render the sanitized one. Sanitizing
	// first would let a reason made entirely of characters the sanitizer
	// removes collapse to "", drop through to the reassuring signed-out line,
	// and exit zero — a daemon that reported a wedge reported as healthy
	// because its reason string was unprintable.
	rawReason := state.GetReloginReason()
	reason := sanitizeDaemonDoctorField(rawReason)
	failingSince := state.GetAuthFailingSince()
	if failingSince != nil || rawReason != "" {
		shownReason := reason
		if shownReason == "" {
			shownReason = "not reported"
		}
		// A reason with no failure clock is a real state: markNeedsRelogin
		// persists the reason, and nothing guarantees the stream ever opened
		// afterwards to start the clock. Say the duration is unknown rather
		// than inventing one — "failing for never" would read as a rendering
		// bug and invite the reader to dismiss the whole line.
		elapsed := "an unknown duration"
		if failingSince != nil {
			elapsed = daemonDoctorSince(failingSince.AsTime(), now)
		}
		_, _ = fmt.Fprintf(out,
			"FAIL daemon auth: upstream authentication has been failing for %s (reason: %s); last successful registration: %s\n",
			elapsed, shownReason, registered)
		if !state.GetNeedsLogin() {
			// The BOS-942 shape exactly: the daemon does not think it needs a
			// login, so nothing else in the system will ever prompt for one.
			_, _ = fmt.Fprintln(out, "  the daemon has not flagged itself as needing a login, so nothing will prompt for one")
		}
		return true, true
	}

	if state.GetNeedsLogin() {
		// needs_login with NEITHER a reason NOR a failure clock. That is the
		// deliberate `boss logout` signature and nothing else: every
		// markNeedsRelogin path persists a reason, and the branch above has
		// already claimed anything that carries one. The daemon is behaving
		// correctly by parking instead of hammering the orchestrator with
		// credentials it knows are dead. The remedy is stated on the line
		// itself rather than through the Remediation block, which is reserved
		// for states that set the unhealthy exit status.
		_, _ = fmt.Fprintln(out, "daemon auth: signed out — run 'boss login' to sign in again")
		return false, false
	}

	_, _ = fmt.Fprintf(out, "daemon auth: signed in — last successful registration: %s (stream connected: %t)\n",
		registered, state.GetUpstreamConnected())
	return false, false
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
	// The live-auth check runs on every platform and BEFORE the macOS-only
	// early return below. An upstream credential wedge has nothing to do with
	// launchd, and putting the call after that return would make the whole
	// check silently unreachable on Linux — the platform most bossd instances
	// that talk to an orchestrator actually run on.
	authUnhealthy, authRemediation := reportDaemonAuthState(cmd.Context(), out)
	// Also above the macOS early return, and for the same reason: a daemon
	// running outside its service manager is not a macOS concept.
	// platformGetStatus fills PID on launchd and on systemd alike, so the check
	// is genuinely cross-platform.
	supervisionMetadata, supervisionMetadataErr := daemonMetadataForDoctor()
	supervisionUnhealthy, supervisionRemediation := reportDaemonSupervision(out, supervisionMetadata, supervisionMetadataErr)
	if daemonDoctorGOOS != "darwin" {
		_, _ = fmt.Fprintf(out, "macOS daemon install and protected-folder checks: not applicable on %s\n", daemonDoctorGOOS)
		// The service-PATH and upstream-auth checks are NOT macOS-specific —
		// the systemd unit carries an explicit PATH too — so their verdicts
		// have to survive this early return. Returning nil here regardless
		// would make both a no-op on exactly the platform they matter most on.
		if servicePathStale || authUnhealthy || supervisionUnhealthy {
			_, _ = fmt.Fprintln(out, "\nRemediation:")
			if servicePathStale || supervisionRemediation {
				_, _ = fmt.Fprintln(out, "  run 'boss daemon restart'")
			}
			if authRemediation {
				_, _ = fmt.Fprintln(out, "  run 'boss login'")
			}
			return errDaemonDoctorUnhealthy
		}
		return nil
	}
	// unhealthyNonAuth is kept separate from authUnhealthy because they have
	// different remedies. Folding an auth wedge into the same flag would print
	// "run 'boss daemon restart'" for a problem a restart cannot fix — the
	// credentials are still dead after it.
	// The supervision verdict rides the non-auth flag: its remedy IS
	// "boss daemon restart", which runDaemonRestart already implements for
	// exactly this shape (installed-but-not-running takes the branch that kills
	// the stray recorded-PID daemon and re-bootstraps it under the service
	// manager).
	unhealthyNonAuth := servicePathStale || supervisionUnhealthy
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
		unhealthyNonAuth = true
		_, _ = fmt.Fprintf(out, "FAIL source bossd: %v\n", sourceErr)
	} else {
		_, _ = fmt.Fprintf(out, "source bossd: %s\n", sourcePath)
	}

	if sourceErr == nil {
		needsStage, reason, compareErr := daemonbin.NeedsStage(sourcePath, stagedPath)
		switch {
		case compareErr != nil:
			unhealthyNonAuth = true
			_, _ = fmt.Fprintf(out, "FAIL staged bossd: %s (comparison failed: %v)\n", stagedPath, compareErr)
		case needsStage:
			unhealthyNonAuth = true
			_, _ = fmt.Fprintf(out, "staged bossd: %s — stale (%s)\n", stagedPath, reason)
		default:
			_, _ = fmt.Fprintf(out, "staged bossd: %s — up to date\n", stagedPath)
		}
	} else {
		_, _ = fmt.Fprintf(out, "staged bossd: %s — source unavailable\n", stagedPath)
	}

	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		unhealthyNonAuth = true
		_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist: resolve home directory: %v\n", homeErr)
	} else {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
		programArguments, plistErr := readLaunchAgentProgramArguments(plistPath)
		switch {
		case plistErr != nil:
			unhealthyNonAuth = true
			installRemediation = errors.Is(plistErr, os.ErrNotExist)
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist %s: %v\n", plistPath, plistErr)
		case len(programArguments) == 0:
			unhealthyNonAuth = true
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent ProgramArguments: no executable configured in %s\n", plistPath)
		default:
			programPath := programArguments[0]
			_, _ = fmt.Fprintf(out, "LaunchAgent ProgramArguments: %s\n", strings.Join(programArguments, " "))
			if strings.Contains(programPath, "/Cellar/") || filepath.Clean(programPath) != filepath.Clean(stagedPath) {
				unhealthyNonAuth = true
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
			unhealthyNonAuth = true
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
				unhealthyNonAuth = true
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
				unhealthyNonAuth = true
			}
			if result.Status == daemonstate.TCCProbeStatusDenied || result.Status == daemonstate.TCCProbeStatusBlocked {
				permissionRemediation = true
			}
		}
	}

	if unhealthyNonAuth || authUnhealthy {
		_, _ = fmt.Fprintln(out, "\nRemediation:")
		if unhealthyNonAuth {
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
		}
		if authRemediation {
			_, _ = fmt.Fprintln(out, "  run 'boss login'")
		}
		if permissionRemediation {
			_, _ = fmt.Fprintln(out, "  Open System Settings > Privacy & Security > Files and Folders (or Full Disk Access).")
			_, _ = fmt.Fprintf(out, "  Grant access to %s — look for the staged path, not a Homebrew/Cellar path.\n", stagedPath)
		}
	}

	if unhealthyNonAuth || authUnhealthy {
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
