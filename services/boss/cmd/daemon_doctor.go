package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/revisiondrift"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
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

// daemonGetSpawnHistory is the launchd spawn-history probe, behind the same
// package-var seam idiom as findDaemonProcess and daemonAuthStateProbe.
//
// The seam is not a convenience: without it every doctor test on a developer's
// machine would read that machine's REAL launchd domain, so the assertions
// would depend on whether the engineer running them happens to have bossd
// loaded. BOS-1183.
var daemonGetSpawnHistory = daemon.GetSpawnHistory

// daemonGetJobDisabled is the launchd disable-override probe, behind the same
// package-var seam idiom as daemonGetSpawnHistory and for the identical
// reason: without the seam every doctor test would read the REAL launchd
// domain of whatever machine runs it, so the assertions would depend on
// whether the engineer running them happens to have bossd loaded and enabled.
// BOS-1222.
var daemonGetJobDisabled = daemon.GetJobDisabled

// daemonConsoleDevicePath is the device node whose owner launchd treats as
// holding the foreground console. It is a constant so the stat this command
// performs and the command it prints for the operator cannot drift apart: the
// BOS-1222 incident was a remediation that named a fact nothing had read, and
// two spellings of the same path would let that gap reopen quietly.
const daemonConsoleDevicePath = "/dev/console"

// daemonConsoleOwnerCommand is the operator-facing way to reproduce, by hand,
// the same fact daemonConsoleOwnerUID reads in-process. It is kept next to the
// path it reads for the reason above, and exists so a report can name how to
// check an indeterminate verdict rather than leaving the operator nowhere to
// go. BOS-1222.
const daemonConsoleOwnerCommand = "stat -f %Su " + daemonConsoleDevicePath

// daemonConsoleOwnership is what doctor ESTABLISHED about who owns
// /dev/console, not what it assumes.
//
// Three outcomes and not two, deliberately. A bool would have to fold "the
// console could not be read" into one of its values, and either fold is a lie:
// folding it into "owned by someone else" invents a cause, and folding it into
// "owned by us" reports ownership as fine on evidence that does not exist —
// the fail-open guess this whole ticket removes. An unreadable console is
// "cannot tell", and asserts nothing either way. BOS-1222.
type daemonConsoleOwnership int

const (
	// daemonConsoleOwnershipUnknown means the owner could not be determined.
	// It asserts nothing: the candidate cause is neither established nor ruled
	// out, and a report must say "not checked" rather than pick a side.
	daemonConsoleOwnershipUnknown daemonConsoleOwnership = iota
	// daemonConsoleOwnershipCurrentUser means the console is owned by the user
	// this command is running as, which RULES OUT foreground-console ownership
	// as the cause of a never-spawned job.
	daemonConsoleOwnershipCurrentUser
	// daemonConsoleOwnershipOtherUser means the console is owned by a
	// different user, which ESTABLISHES foreground-console ownership as a
	// cause: this session's launchd will not spawn new RunAtLoad jobs.
	daemonConsoleOwnershipOtherUser
)

// daemonConsoleOwnerUID reports the owning UID of /dev/console.
//
// A package var over a plain stat, behind the same seam idiom as
// findDaemonProcess and daemonGetSpawnHistory, because without it every test
// of this verdict would read the REAL console of whatever machine runs it —
// so the assertions would depend on whether the engineer is signed in at the
// foreground, which is precisely the condition under test.
//
// It is a stat and NOT a subprocess, even though the operator-facing command
// is `stat -f %Su`. BOS-864 rejected an advisory subprocess inside a
// diagnostic after an unbounded one caused a multi-minute silent stall in this
// codebase; a file-info read has no such failure mode.
var daemonConsoleOwnerUID = func() (int, error) {
	info, err := os.Stat(daemonConsoleDevicePath)
	if err != nil {
		return 0, err
	}
	// Comma-ok, never a bare assertion: a platform whose FileInfo carries no
	// Stat_t must return an error so the verdict fails closed to unknown. A
	// guessed UID here would be reported as an established fact.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: file info carries no owner UID on this platform", daemonConsoleDevicePath)
	}
	return int(stat.Uid), nil
}

// daemonCurrentUID is the UID this command runs as, behind a seam for the same
// reason as daemonConsoleOwnerUID: the comparison must be drivable from a test
// without the host's own identity deciding the outcome.
var daemonCurrentUID = os.Getuid

// classifyDaemonConsoleOwnership maps a console-owner reading onto a verdict.
//
// Pure, and split out from the seams for the reason classifyLaunchdSpawnHistory
// is: the truth table is the thing worth proving, and proving it through a
// rendered report proves the rendering instead.
//
// The ladder is fail-closed top to bottom — every branch that is not a positive
// comparison of two trustworthy UIDs ends in unknown. Negative UIDs are guarded
// rather than compared: no OS produces one, so a negative here means the seam
// returned a zero value alongside an error a caller dropped, or an
// os.Getuid that reported "unsupported" (-1). Comparing those would let two
// untrustworthy values agree and read as "ownership is fine".
func classifyDaemonConsoleOwnership(ownerUID int, ownerErr error, currentUID int) daemonConsoleOwnership {
	switch {
	case ownerErr != nil:
		return daemonConsoleOwnershipUnknown
	case ownerUID < 0, currentUID < 0:
		return daemonConsoleOwnershipUnknown
	case ownerUID == currentUID:
		return daemonConsoleOwnershipCurrentUser
	default:
		return daemonConsoleOwnershipOtherUser
	}
}

// daemonDoctorConsoleOwnership wires the two seams into the classifier. It is
// the only entry point a reporter should call; the classifier stays pure so
// its table can be tested without them.
func daemonDoctorConsoleOwnership() daemonConsoleOwnership {
	ownerUID, err := daemonConsoleOwnerUID()
	return classifyDaemonConsoleOwnership(ownerUID, err, daemonCurrentUID())
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
	return !commandPathIsOneOf(commandPath, daemonStalenessWarningRemedyCommands)
}

// commandPathIsOneOf matches a cmd.CommandPath() against a list of command
// paths by SUBTREE, not by substring: "boss daemon doctor" matches itself and
// anything below it, while "boss daemon status" does not match "boss daemon"
// entries it merely shares a prefix of.
//
// One definition, because there are now two passive warnings that each skip
// their own remedy commands, and two copies of a prefix test is how one of them
// quietly becomes a substring test.
func commandPathIsOneOf(commandPath string, paths []string) bool {
	for _, candidate := range paths {
		if commandPath == candidate || strings.HasPrefix(commandPath, candidate+" ") {
			return true
		}
	}
	return false
}

// skipBossRevisionDriftWarningEnv suppresses the revision-drift warning for
// scripted use, following the BOSS_DAEMON_SKIP_STALE_WARNING precedent.
const skipBossRevisionDriftWarningEnv = "BOSS_SKIP_REVISION_DRIFT_WARNING"

// bossRevisionDriftWarningText is the single line every `boss` subcommand may
// emit when the executing binary provably predates the checkout it runs in.
const bossRevisionDriftWarningText = "boss: this boss binary predates the checkout it is running in — " +
	"rebuild and reinstall it (details: boss daemon doctor)"

// bossRevisionDriftWarningRemedyCommands are the commands that already report
// this fact in full, or that are themselves the remedy. Warning on them is
// noise at best and misleading at worst.
var bossRevisionDriftWarningRemedyCommands = []string{
	"boss daemon doctor",
	"boss env",
	"boss upgrade",
}

// bossExecutablePath is a seam over os.Executable so the checkout-build guard
// below is testable: a test binary lives in a temp directory of the toolchain's
// choosing, which is never inside the fixture's checkout.
var bossExecutablePath = os.Executable

// warnIfBossBinaryBehindCheckout writes at most one stderr line when the
// executing boss binary provably predates the checkout it is being run in.
//
// This surface exists because of BOS-864's finding, which the incident behind
// this check then repeated one binary over: "the detection was never the
// problem. The surface was." Four mitigations existed for the daemon case and
// all four failed, doctor included, because diagnostic commands are run after
// you already suspect a problem — and the whole failure mode is not suspecting
// it.
//
// Only the `behind` outcome warns. Every unknown stays silent here: an unknown
// earns a line in a diagnostic an operator asked for, not on every invocation.
//
// It can never fail a command. Every error inside is swallowed, nothing is
// written to stdout — a --json consumer would be corrupted by a warning there —
// and the exit code is untouched.
func warnIfBossBinaryBehindCheckout(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	if os.Getenv(skipBossRevisionDriftWarningEnv) != "" {
		return
	}
	if commandPathIsOneOf(cmd.CommandPath(), bossRevisionDriftWarningRemedyCommands) {
		return
	}
	// Ordered BEFORE the probe, and that ordering is the point. Everything
	// above this line reads only the process's own environment, its command
	// path, and the filesystem; the probe below spawns up to FIVE git
	// subprocesses — two unbounded ones inside the trust check, plus
	// rev-parse HEAD, rev-parse --git-common-dir and merge-base
	// --is-ancestor — and this runs on EVERY ordinary `boss` invocation. A
	// suppression guard that runs after the work it suppresses is a filter on
	// the output, not a guard on the cost.
	//
	// The root is resolved locally through the same FindCheckoutRoot the
	// classifier itself defaults to, so this compares against the directory
	// the probe would have named, without paying for the probe.
	if bossExecutableIsCheckoutBuild(bossRevisionDriftCheckoutRoot()) {
		return
	}
	drift := bossRevisionDriftProbe(cmd.Context())
	if !drift.BehindKnown || !drift.Behind {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s [checkout: %s]\n", bossRevisionDriftWarningText, drift.CheckoutRoot)
}

// bossRevisionDriftCheckoutRoot resolves the checkout root WITHOUT spawning a
// subprocess, so the per-invocation warning's checkout-build guard can run
// before the probe rather than after it. FindCheckoutRoot is a structural
// upward walk, which is also the classifier's own default resolution — so the
// hoisted guard and the probe agree on which directory they mean.
//
// A package var for the reason the probes are: left unstubbed it reads the
// DEVELOPER's real checkout and the suite's verdicts become machine-dependent.
var bossRevisionDriftCheckoutRoot = func() string {
	startDir, err := os.Getwd()
	if err != nil {
		return ""
	}
	root, ok := revisiondrift.FindCheckoutRoot(startDir)
	if !ok {
		return ""
	}
	return root
}

// bossExecutableIsCheckoutBuild reports whether the executing binary is the
// checkout's own build output, <checkout>/bin/boss.
//
// A developer who just ran `make build` is legitimately running a binary that
// may lag their working tree by a commit, and nagging them on every invocation
// is how a warning gets trained away — the same reasoning the daemon staleness
// warning's Cellar guard applies, one binary over.
//
// Symlinks are evaluated on BOTH sides. On macOS the temp and per-user
// directories these paths commonly sit under are symlinks (/var -> /private/var
// being the usual one), so comparing one resolved path against one unresolved
// path reports two spellings of the same directory as different.
func bossExecutableIsCheckoutBuild(checkoutRoot string) bool {
	if checkoutRoot == "" {
		return false
	}
	executable, err := bossExecutablePath()
	if err != nil {
		// Unresolvable: fail closed toward SILENCE. A warning that cannot
		// establish it is not nagging a developer is the one that gets
		// suppressed wholesale, and this warning can never be worth a command.
		return true
	}
	return sameDirectory(filepath.Dir(executable), filepath.Join(checkoutRoot, daemonbin.BinDirName))
}

func sameDirectory(left, right string) bool {
	return resolvedPath(left) == resolvedPath(right)
}

func resolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
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

// reportDaemonIdentity describes the identity the running daemon resolved at
// startup and presents to the orchestrator. It intentionally consumes the
// persisted record instead of today's settings: settings may have changed
// since startup, while this diagnostic must describe the daemon currently
// registered upstream.
func reportDaemonIdentity(out io.Writer, metadata daemonstate.Metadata, metadataErr error) {
	if metadataErr != nil {
		return
	}

	name := sanitizeDaemonDoctorField(metadata.DisplayName)
	if name == "" {
		_, _ = fmt.Fprintf(out, "daemon identity: presents as unknown (daemon id: %s) — run 'boss daemon restart' to refresh it\n", daemonDoctorIdentityID(metadata.DaemonID))
		return
	}

	// The no-override path uses config.DefaultDisplayHostname at daemon
	// startup. On macOS that may be the operator-facing ComputerName rather
	// than the raw hostname, so call it a machine default rather than claiming
	// a more specific source than the persisted record can prove.
	source := "machine default"
	if metadata.DisplayNameOverride {
		source = "daemon_name override"
	}
	_, _ = fmt.Fprintf(out, "daemon identity: presents as %s (from %s; daemon id: %s) — rename with 'boss settings --daemon-name <name>' and restart with 'boss daemon restart'\n", name, source, daemonDoctorIdentityID(metadata.DaemonID))
}

func daemonDoctorIdentityID(id string) string {
	if id = sanitizeDaemonDoctorField(id); id != "" {
		return id
	}
	return "unknown"
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
//
// The ownership verdict is NOT decided here. It comes from
// daemonSupervisionOfLiveRecord (services/boss/cmd/daemon_supervision.go),
// which daemonSupervisionLine in handlers.go renders as a `boss daemon status`
// line and which delegates to daemon.ClassifyServingMode — the same decision
// `boss daemon restart` branches on. That shared call replaces what used to be
// a hand-copied ladder whose agreement was asserted only in this comment, and
// a comment asserting an invariant cannot fail when the invariant breaks.
//
// Exactly one divergence between the two renderers survives, and
// TestDaemonSupervisionVerdictsMatchDoctor pins it as the only one: a daemon
// recorded while no service is installed reads unknown here (the not-installed
// check below owns that fact and its remedy; claiming it twice would print two
// failures for one cause) and unsupervised on the status line.
func reportDaemonSupervision(
	out io.Writer,
	metadata daemonstate.Metadata,
	metadataErr error,
	supervision daemon.SupervisionModeStatus,
	ownership daemon.WatchdogOwnership,
) (unhealthy bool, remediation daemonSupervisionRemediation) {
	// Checked BEFORE the service view is read, not after. Under this env var
	// platformGetStatus deliberately returns Installed=true, Running=false
	// without ever asking launchd or systemd — which is byte-identical to the
	// detached-daemon shape this function exists to flag. Interpreting it would
	// turn every test harness and CI run into a FAIL, which is precisely how a
	// diagnostic gets ignored on the host where it is telling the truth.
	if os.Getenv("BOSS_DAEMON_SKIP_LAUNCHCTL") != "" {
		_, _ = fmt.Fprintln(out, "daemon supervision: unknown (service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL)")
		return false, daemonSupervisionRemediationNone
	}

	st, statusErr := daemonGetStatus()
	switch {
	case statusErr != nil:
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (service status unavailable: %v)\n", statusErr)
		return false, daemonSupervisionRemediationNone
	case metadataErr != nil:
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (no daemon state record: %v)\n", metadataErr)
		return false, daemonSupervisionRemediationNone
	case !st.Installed && !daemonUnattendedSubstrateOwnsVerdict(supervision):
		// "not installed" has its own check and its own remedy below; claiming
		// it here too would print two failures for one fact.
		//
		// BOS-1204: `st.Installed` is the per-user LaunchAgent, and an
		// unattended install deliberately supersedes that agent — so on a
		// correctly supervised unattended host this rung is TRUE and used to
		// swallow the verdict entirely, leaving doctor saying "unknown" where
		// status said "supervised". That is a divergence beyond the single one
		// TestDaemonSupervisionVerdictsMatchDoctor declares, which is why the
		// guard is here rather than left to the wording below.
		_, _ = fmt.Fprintln(out, "daemon supervision: unknown (no service is installed)")
		return false, daemonSupervisionRemediationNone
	case metadata.PID <= 0:
		_, _ = fmt.Fprintln(out, "daemon supervision: unknown (no recorded daemon PID)")
		return false, daemonSupervisionRemediationNone
	}

	alive, aliveErr := daemonProcessAlive(metadata.PID)
	switch {
	case aliveErr != nil:
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (recorded PID %d could not be checked: %v)\n", metadata.PID, aliveErr)
		return false, daemonSupervisionRemediationNone
	case !alive:
		// Nothing is running under the recorded PID. The running-process check
		// below owns that story.
		_, _ = fmt.Fprintf(out, "daemon supervision: unknown (recorded PID %d is not running)\n", metadata.PID)
		return false, daemonSupervisionRemediationNone
	}

	// The verdict is decided by daemonSupervisionOfLiveRecord — the one
	// decision `boss daemon status` renders too and `boss daemon restart`
	// branches on, via daemon.ClassifyServingMode. Only the wording and the
	// remediation flags are chosen here.
	verdict, reason := daemonSupervisionOfLiveRecord(st, metadata.PID, supervision, ownership)
	switch reason {
	case daemonSupervisionReasonDetached:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: FAIL bossd (PID %d) is running but the service manager does not own it — %s\n",
			metadata.PID, daemonUnsupervisedConsequences)
	case daemonSupervisionReasonNoServicePID:
		// Reachable on systemd when `systemctl is-active` succeeds but the
		// MainPID read does not. BOS-1218 made it unreachable on launchd:
		// unparseable `launchctl list` output leaves PIDKnown false, which now
		// forces Running false too, so such a host reaches the detached arm
		// instead. Certifying ownership here would emit a false healthy verdict
		// from the one check added to detect an ownership mismatch.
		_, _ = fmt.Fprintf(out,
			"daemon supervision: unknown (the service manager reports running but did not report a PID; recorded daemon is PID %d)\n",
			metadata.PID)
	case daemonSupervisionReasonForeignPID:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: FAIL the service manager owns PID %d but the recorded daemon is PID %d — two daemons, or a stale state record\n",
			st.PID, metadata.PID)
	case daemonSupervisionReasonManagerOwned:
		_, _ = fmt.Fprintf(out, "daemon supervision: ok (PID %d is owned by the service manager)\n", metadata.PID)
	case daemonSupervisionReasonWatchdogOwned:
		// Worded as the two-step chain rather than as plain "the service
		// manager owns it", for the reason daemonSupervisionLine is: on this
		// substrate bossd is a grandchild of launchd, and the gui/<uid> job an
		// operator would go looking for is absent by design.
		_, _ = fmt.Fprintf(out,
			"daemon supervision: ok (launchd owns the root-owned %s LaunchDaemon and that watchdog spawns bossd, so PID %d is a grandchild of launchd rather than a launchd job of its own)\n",
			daemon.WatchdogLabel, metadata.PID)
	case daemonSupervisionReasonWatchdogNotLoaded:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: FAIL the %s watchdog is installed at %s but launchd does not have %s loaded, so nothing will restart bossd (PID %d)\n",
			supervision.Mode, supervision.Unattended.PlistPath, ownership.Target, metadata.PID)
	case daemonSupervisionReasonWatchdogUnreadable:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: unknown (the %s watchdog is installed, but whether launchd has %s loaded could not be established: %s)\n",
			supervision.Mode, daemon.WatchdogTarget(), watchdogOwnershipReason(ownership))
	case daemonSupervisionReasonWatchdogInsecure:
		// Unknown, not FAIL. The insecure path IS a fault and IS reported —
		// once, by the substrate line reportDaemonSupervisionMode renders,
		// which names the offending path in full. A second FAIL here would be
		// two failures for one fact.
		_, _ = fmt.Fprintf(out,
			"daemon supervision: unknown (the %s watchdog is installed but not safe for a root-owned job, so its ownership of PID %d cannot be relied on; see the daemon supervision substrate line)\n",
			supervision.Mode, metadata.PID)
	default:
		_, _ = fmt.Fprintf(out,
			"daemon supervision: unknown (the service manager's view of PID %d could not be attributed)\n",
			metadata.PID)
	}
	// Derived from the shared VERDICT rather than restated per rung. A `return
	// true, true` literal in each unhealthy case is a second, hand-maintained
	// copy of the same classification — the exact shape this repair removed —
	// and it is what would let a future rung print FAIL while reporting healthy.
	// Only an unsupervised daemon is a fault: an unknown is a probe that could
	// not tell, and restarting on it would act on nothing observed.
	unhealthy = verdict == daemonSupervisionUnsupervised
	if !unhealthy {
		return false, daemonSupervisionRemediationNone
	}
	// The REMEDY is chosen from the reason, not from the verdict, because the
	// two unsupervised reasons have disjoint fixes and only one of them is a
	// restart. A watchdog whose plist is on disk but whose `system`-domain job
	// launchd never bootstrapped cannot be repaired by any per-user command:
	// `boss daemon restart` re-bootstraps the LaunchAgent this substrate
	// deliberately supersedes, so it would succeed while leaving the fault
	// exactly where it was. Loading a root-owned LaunchDaemon needs root.
	if reason == daemonSupervisionReasonWatchdogNotLoaded {
		return true, daemonSupervisionRemediationInstallWatchdog
	}
	return true, daemonSupervisionRemediationRestart
}

// daemonSupervisionRemediation names WHICH remedy an unhealthy supervision
// verdict needs, so the two remediation ladders below cannot print a per-user
// restart for a fault only root can clear.
//
// It replaced a bool. The bool made "unhealthy" and "restart" the same fact,
// which was true while every unsupervised shape was a detached bossd and became
// false the moment BOS-1204 added a rung whose fault is a LaunchDaemon launchd
// does not have loaded.
type daemonSupervisionRemediation int

const (
	// daemonSupervisionRemediationNone is the zero value: nothing observed to
	// remedy.
	daemonSupervisionRemediationNone daemonSupervisionRemediation = iota
	// daemonSupervisionRemediationRestart is the LaunchAgent-substrate remedy —
	// a detached or foreign-PID bossd, which runDaemonRestart already handles.
	daemonSupervisionRemediationRestart
	// daemonSupervisionRemediationInstallWatchdog is the unattended-substrate
	// remedy: bootstrap the root-owned watchdog, which needs root.
	daemonSupervisionRemediationInstallWatchdog
)

// daemonWatchdogNotLoadedRemediation is the remedy for a watchdog that is
// installed on disk and not loaded in launchd's `system` domain.
//
// It names the same command daemonSupervisionModeRemediation prints for an
// ABSENT install, and deliberately says why a restart is not the answer: the
// operator is looking at a FAIL whose plist they can see on disk, so "install"
// reads like a step they have already taken.
func daemonWatchdogNotLoadedRemediation() string {
	return fmt.Sprintf("  run `%s` to bootstrap the unattended supervision watchdog into launchd's system domain (macOS will prompt for an administrator password) — 'boss daemon restart' cannot load a root-owned LaunchDaemon", daemon.WatchdogInstallCommand)
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

// daemonSpawnRemediation names which remedy a spawn-history verdict calls for.
//
// An enum rather than a second and third bool because the remedies are mutually
// exclusive by construction — never-spawned is a launchd domain problem and
// crash-looping is bossd's own — and a pair of bools would admit a "both" state
// the Remediation block would then have to pick between arbitrarily.
type daemonSpawnRemediation int

const (
	// daemonSpawnRemediationNone covers every healthy and every inconclusive
	// verdict: nothing to tell the operator to do.
	daemonSpawnRemediationNone daemonSpawnRemediation = iota

	// daemonSpawnRemediationConsole is the never-spawned remedy: launchd will
	// not start the job in this domain, so the requirement to state is the
	// foreground console, never a boss command.
	daemonSpawnRemediationConsole

	// daemonSpawnRemediationForeground is the crash-loop remedy: launchd DID
	// spawn bossd and bossd died, so the only step that makes the failure
	// visible is running the staged binary in the foreground.
	daemonSpawnRemediationForeground

	// daemonSpawnRemediationSpawnCauses is the never-spawned remedy for the
	// case daemonSpawnRemediationConsole above cannot honestly cover: launchd
	// never tried, and console ownership was NOT established as the reason.
	//
	// A fourth enum value and not a bool alongside Console, for the same
	// reason recorded above this block: the remedies are mutually exclusive by
	// construction. "Console ownership is the established cause" and "no
	// candidate cause is established" cannot both be true, so a bool pair
	// would admit a state the Remediation ladder would have to pick between
	// arbitrarily — and picking arbitrarily between a cause and the absence of
	// one is precisely the defect BOS-1222 removes.
	daemonSpawnRemediationSpawnCauses
)

// daemonSpawnCauseVerdict is what doctor ESTABLISHED about one candidate cause
// of a never-spawned job — never what it assumes.
//
// Three values and not a bool, for the reason daemonConsoleOwnership carries
// three: "not checked" is a real answer, and folding it into either side is
// the fail-open guess this ticket exists to remove.
type daemonSpawnCauseVerdict int

const (
	// daemonSpawnCauseNotChecked means doctor could not settle this candidate.
	// It asserts nothing, and a report must hand over the command that would.
	daemonSpawnCauseNotChecked daemonSpawnCauseVerdict = iota
	// daemonSpawnCauseRuledOut means doctor measured this candidate and it is
	// not what is holding the job.
	daemonSpawnCauseRuledOut
	// daemonSpawnCauseEstablished means doctor measured this candidate and it
	// holds.
	daemonSpawnCauseEstablished
)

// String renders a verdict as the word a report prints. It is a method rather
// than a rendering-site switch so every candidate line in every branch is
// spelled the same way.
func (v daemonSpawnCauseVerdict) String() string {
	switch v {
	case daemonSpawnCauseRuledOut:
		return "ruled out"
	case daemonSpawnCauseEstablished:
		return "established"
	case daemonSpawnCauseNotChecked:
		return "not checked"
	default:
		// A verdict this build does not recognise asserts nothing, which is
		// the same fail-closed direction every other classifier here takes.
		return "not checked"
	}
}

// daemonSpawnCauseFacts is what doctor OBSERVED, gathered on its own side of
// the boundary: console ownership and the disable override are facts about the
// user's login session, which is where this command runs.
//
// The two command strings travel with the facts rather than being rebuilt at
// the rendering site, because the domain they name is resolved by the probe
// and a remediation that names a domain nobody probed is the same class of
// defect as one that names a cause nobody measured.
type daemonSpawnCauseFacts struct {
	// Console is the /dev/console ownership verdict.
	Console daemonConsoleOwnership
	// Disabled is the service manager's disable-override verdict.
	Disabled daemon.JobDisabledState
	// SocketKnown is false when the profile could not be resolved, so whether
	// the socket answers is genuinely unknown rather than "no".
	SocketKnown bool
	// SocketReachable reports whether the daemon socket answered. Meaningful
	// only when SocketKnown is true.
	SocketReachable bool
	// Label is the job's launchd label — what the disable-override probe
	// asked about. It is carried so the report can NAME the string an
	// operator is told to look for; daemon.Label is darwin-only, so the cmd
	// package can only learn it by being handed it.
	Label string
	// DisabledCommand is the operator-facing `launchctl print-disabled
	// <domain>` that settles the disable candidate by hand.
	DisabledCommand string
	// EnableCommand is the literal `launchctl enable <domain>/<label>` that
	// CLEARS an established override. Empty when the probe resolved no label,
	// because a command naming a label doctor did not read would be the same
	// defect class this ticket removes.
	EnableCommand string
	// DomainCommand is the operator-facing `launchctl print <domain>` that
	// settles the one candidate doctor cannot check: a GUI domain that is
	// on-demand-only, or otherwise will not honour RunAtLoad.
	DomainCommand string
}

// daemonSpawnCauses is the per-candidate verdict set the never-spawned
// remediation reports.
type daemonSpawnCauses struct {
	Console  daemonSpawnCauseVerdict
	Disabled daemonSpawnCauseVerdict
	Served   daemonSpawnCauseVerdict
}

// classifyDaemonSpawnCauses maps observed facts onto per-candidate verdicts.
//
// Pure, and split out from every reporter for the reason classifyProbeResult
// and classifyLaunchdSpawnHistory are: the truth table is the thing worth
// proving, and proving it through a rendered report proves the rendering
// instead.
//
// Every unreadable input lands on daemonSpawnCauseNotChecked. There is no
// branch here that turns an absent measurement into a verdict.
func classifyDaemonSpawnCauses(facts daemonSpawnCauseFacts) daemonSpawnCauses {
	causes := daemonSpawnCauses{}

	switch facts.Console {
	case daemonConsoleOwnershipOtherUser:
		causes.Console = daemonSpawnCauseEstablished
	case daemonConsoleOwnershipCurrentUser:
		causes.Console = daemonSpawnCauseRuledOut
	case daemonConsoleOwnershipUnknown:
		causes.Console = daemonSpawnCauseNotChecked
	default:
		causes.Console = daemonSpawnCauseNotChecked
	}

	switch facts.Disabled {
	case daemon.JobDisabledStateDisabled:
		causes.Disabled = daemonSpawnCauseEstablished
	case daemon.JobDisabledStateEnabled:
		causes.Disabled = daemonSpawnCauseRuledOut
	case daemon.JobDisabledStateUnknown, daemon.JobDisabledStateUnsupported:
		causes.Disabled = daemonSpawnCauseNotChecked
	default:
		causes.Disabled = daemonSpawnCauseNotChecked
	}

	switch {
	case !facts.SocketKnown:
		// No profile means we cannot know whether the socket answers, and a
		// verdict printed on a guess sends an operator to foreground a bossd
		// that is already up.
		causes.Served = daemonSpawnCauseNotChecked
	case facts.SocketReachable:
		causes.Served = daemonSpawnCauseEstablished
	default:
		causes.Served = daemonSpawnCauseRuledOut
	}

	return causes
}

// anyCauseEstablished reports whether a candidate FAULT was established.
//
// Served is deliberately excluded, and that exclusion is the whole reason this
// is a method rather than a loop over the three fields. A served socket is not
// a fault: it is the ordinary shape after the detached-fallback recovery —
// `boss daemon start` spawned bossd directly, the socket answers, and
// launchd's own spawn count stays 0 forever. Folding it in as an "established
// cause" is what sent an operator to foreground a SECOND bossd over a socket
// that already answered, the duplicate the three isSocketReachable guards in
// platformEnsureRunning exist to prevent. It is reassurance, and a report must
// read it that way.
func (c daemonSpawnCauses) anyCauseEstablished() bool {
	return c.Console == daemonSpawnCauseEstablished || c.Disabled == daemonSpawnCauseEstablished
}

// reportDaemonSpawnHistory asks the question every other macOS check in this
// command is structurally unable to ask: did launchd ever actually TRY to start
// the job?
//
// BOS-1183: on 2026-09-06 doctor was clean on every adjacent condition — the
// staged binary up to date, the protected roots ok, the plist naming the right
// executable — while the LaunchAgent sat registered in a GUI domain launchd
// would never spawn anything in, because fast user switching had backgrounded
// the user's Aqua session. `launchctl list` exits 0 for such a job, so nothing
// the other checks read could see it, and the diagnosis took an hour of
// `launchctl print` and system-log reading outside the tooling entirely.
//
// The two FAIL verdicts carry DISJOINT remedies, which is the whole reason the
// distinction has to reach the operator: never-spawned is a launchd domain
// problem no amount of restarting bossd will fix, and failing is bossd's own
// crash, which has nothing to do with the domain.
//
// Every inconclusive input returns unknown rather than a failure, matching
// reportDaemonSupervision above. This runs on developer machines and in CI, and
// a false FAIL there is how an operator learns to skip the one line that is
// telling the truth on a real host.
func reportDaemonSpawnHistory(out io.Writer, stagedPath string, supervision daemon.SupervisionModeStatus) (unhealthy bool, remediation daemonSpawnRemediation, facts daemonSpawnCauseFacts) {
	// BOS-1204 AC8: the FAILING verdict's whole value is that it points at the
	// binary launchd actually ran, and on the unattended substrate that is the
	// watchdog's ROOT-OWNED copy, never the per-user staged one. The staged
	// path is deliberately kept for every other message here — the
	// never-spawned remedy and the startup directive are about the LaunchAgent
	// substrate or about the file this user can run by hand.
	spawnedBinary := stagedPath
	if daemonUnattendedSubstrateConfigured(supervision) && supervision.Unattended.BinaryPath != "" {
		spawnedBinary = supervision.Unattended.BinaryPath
	}
	history, err := daemonGetSpawnHistory()
	if err != nil {
		// A non-nil error means launchctl could not be EXECUTED at all. The
		// returned history is fail-closed on that path, so reporting unknown
		// here cannot certify anything. The message is a foreign process's
		// error text, so it is bounded like every other one.
		_, _ = fmt.Fprintf(out, "launchd spawn history: unknown (%s)\n", sanitizeDaemonDoctorField(err.Error()))
		return false, daemonSpawnRemediationNone, facts
	}

	switch history.State {
	case daemon.SpawnStateUnsupported:
		// The caller only reaches this inside the darwin-only section, so there
		// is nothing to report and nothing to warn about.
		return false, daemonSpawnRemediationNone, facts
	case daemon.SpawnStateNeverSpawned:
		_, _ = fmt.Fprintf(out,
			"launchd spawn history: FAIL launchd has never attempted to spawn %s (runs = 0) — the job is registered in a domain launchd will not start it in, which is a launchd domain problem and never a bossd crash\n",
			history.Target)
		// R5: everything above this point — the detection, the FAIL wording
		// and the unhealthy verdict — is unchanged. The ONLY thing that
		// changed is which remediation value this arm returns, and it now
		// depends on what doctor established rather than on nothing at all.
		//
		// Facts are gathered on this arm and nowhere else: they are the inputs
		// to the never-spawned candidate report, and reading /dev/console or
		// shelling launchctl on a healthy run would be cost for no answer.
		facts = gatherDaemonSpawnCauseFacts()
		// R4: an established console verdict still gets its three unchanged
		// lines — but ONLY when the decisive cause is not also established.
		//
		// A job that is BOTH disabled and owned by another console user used
		// to short-circuit here and print the console remediation alone, so
		// the disable override never reached the operator at all; and that
		// remediation's closing line promises launchd "spawns the job once
		// that user owns /dev/console again", which is FALSE while the job
		// carries a disable override. That is this ticket's own defect class
		// recurring one layer up: a confident sentence naming a cause that is
		// not the one holding the job. reportDaemonSpawnCauses already ranks
		// the disable override first; this routing was bypassing that rank.
		if facts.Console == daemonConsoleOwnershipOtherUser && facts.Disabled != daemon.JobDisabledStateDisabled {
			return true, daemonSpawnRemediationConsole, facts
		}
		return true, daemonSpawnRemediationSpawnCauses, facts
	case daemon.SpawnStateFailing:
		// Runs and LastExitCode are readable by construction here: the
		// classifier reaches this state only after parsing both.
		//
		// WHOSE exit this is depends on the substrate, and saying "bossd itself
		// started and failed" on the unattended one would be a diagnosis of the
		// wrong process. There launchd spawns the WATCHDOG — bossd is its
		// grandchild, reached through `launchctl asuser` — so the recorded exit
		// is the watchdog's and can precede bossd being spawned at all. The
		// target this history was read from moved to the watchdog job with
		// BOS-1204; the sentence describing it had not.
		if daemonUnattendedSubstrateConfigured(supervision) {
			_, _ = fmt.Fprintf(out,
				"launchd spawn history: FAIL launchd has spawned %s %d times and it last exited with code %d — that is the WATCHDOG's own exit, not bossd's, so it can precede bossd being spawned at all; the fault is in %s, not in the launchd domain\n",
				history.Target, history.Runs, history.LastExitCode, spawnedBinary)
			return true, daemonSpawnRemediationForeground, facts
		}
		_, _ = fmt.Fprintf(out,
			"launchd spawn history: FAIL launchd has spawned %s %d times and it last exited with code %d — bossd itself started and failed, so the fault is in the staged binary %s, not in the launchd domain\n",
			history.Target, history.Runs, history.LastExitCode, spawnedBinary)
		return true, daemonSpawnRemediationForeground, facts
	case daemon.SpawnStateHealthy:
		_, _ = fmt.Fprintf(out, "launchd spawn history: ok (launchd has spawned %s %d times)\n",
			history.Target, history.Runs)
		return false, daemonSpawnRemediationNone, facts
	default:
		// SpawnStateUnknown, plus anything a future build of the probe adds.
		// The Reason is printed verbatim rather than through
		// sanitizeDaemonDoctorField: it is this repository's own sentence,
		// already single-line, and it quotes any launchctl-supplied value with
		// %q — truncating it at the field bound would cut off the half that
		// says what could not be read.
		reason := history.Reason
		if reason == "" {
			reason = fmt.Sprintf("unrecognised spawn state %q", string(history.State))
		}
		_, _ = fmt.Fprintf(out, "launchd spawn history: unknown (%s)\n", reason)
		return false, daemonSpawnRemediationNone, facts
	}
}

// daemonSpawnConsoleRequirementLine is the one sentence BOS-1222 removed from
// the unconditional path. It is a named constant so a test can assert its
// ABSENCE precisely — the assertion this ticket most needs is the negative one,
// and matching a hand-copied prefix would let the sentence drift back in under
// a different spelling.
//
// It is NOT deleted. It is correct advice, and it still prints verbatim on the
// branch where doctor has ESTABLISHED that another user owns the console
// (R4). What was wrong was printing it unconditionally: on the measured host
// the console was owned by the daemon's own user and runs = 0 persisted, so
// the sentence named a cause the command had refused.
const daemonSpawnConsoleRequirementLine = "  bossd's user must own the FOREGROUND console — check with: " +
	daemonConsoleOwnerCommand + ", which must print that user."

// daemonSpawnDisabledDomainFallback names the domain shape in a printed
// command when the probe could not resolve a real one — on a platform with no
// launchd domain, or a probe that never ran. Naming a domain we did not
// resolve would be the same class of defect this ticket removes, so the
// substitution is one the operator's own shell expands.
const daemonSpawnDisabledDomainFallback = "gui/$(id -u)"

// daemonSpawnDisabledLabelPhrase names the label the operator has to look for
// in the dump, and falls back to the generic phrase when the probe resolved
// none.
//
// BOS-1222: the follow-up handed over a dump command and then told the
// operator to check it for a string doctor never spelled — a softer instance
// of the defect class this ticket removes.
func daemonSpawnDisabledLabelPhrase(label string) string {
	if label == "" {
		return "the job's label"
	}
	return "the job's label `" + label + "`"
}

// gatherDaemonSpawnCauseFacts reads the two candidate causes doctor can settle
// on its own side of the boundary, and records the commands that settle the
// rest.
//
// Socket reachability is deliberately NOT read here: runDaemonDoctor probes
// the socket once, later in the run, and calling daemonSocketReachable a
// second time would let one report contain two answers to the same question.
// The caller fills SocketKnown/SocketReachable in from that single probe.
func gatherDaemonSpawnCauseFacts() daemonSpawnCauseFacts {
	facts := daemonSpawnCauseFacts{Console: daemonDoctorConsoleOwnership()}

	// The returned value is fail-closed on BOTH paths, so the error is folded
	// in rather than branched on: a probe that could not be attempted is the
	// same "not checked" as one that ran and could not tell.
	disabled, _ := daemonGetJobDisabled()
	facts.Disabled = disabled.State

	domain := disabled.Domain
	if domain == "" {
		domain = daemonSpawnDisabledDomainFallback
	}
	facts.DisabledCommand = "launchctl print-disabled " + domain
	facts.DomainCommand = "launchctl print " + domain

	// The label gets the same discipline as the domain, in the opposite
	// direction: a domain has a shape the operator's own shell can expand, so
	// it falls back; a label does not, so an unresolved one leaves the enable
	// command EMPTY and the report falls back to prose. Naming a label doctor
	// never read would be exactly the unmeasured assertion BOS-1222 removes.
	facts.Label = disabled.Label
	if facts.Label != "" {
		facts.EnableCommand = "launchctl enable " + domain + "/" + facts.Label
	}
	return facts
}

// reportDaemonSpawnCauses prints the never-spawned candidate causes: what
// doctor established, what it ruled out, what it could not check, and the
// command that settles each unsettled one.
//
// This is the replacement for a remediation that asserted ONE cause it had
// never measured. The shape is the one
// docs/solutions/design-patterns/a-bounded-probe-must-classify-from-the-deadline-and-the-command-error-together.md
// prescribes for exactly this failure: decline to answer what was not
// established, name the suspects, and hand over the measurement. A message
// that is specific, confident and pointing away from the cause is worse than
// no message.
func reportDaemonSpawnCauses(out io.Writer, facts daemonSpawnCauseFacts) {
	causes := classifyDaemonSpawnCauses(facts)

	// Reassurance FIRST when the socket answers. runs = 0 is the ordinary
	// shape after a detached-fallback recovery, and this branch must never
	// read as a new fault on a host where the daemon is working.
	if causes.Served == daemonSpawnCauseEstablished {
		_, _ = fmt.Fprintln(out, "  Nothing is broken operationally: the daemon socket answers, so a bossd IS serving this profile. runs = 0 is the ordinary shape after a detached-fallback recovery — `boss daemon start` spawned bossd directly and launchd's own spawn count stays 0 forever. Do not start a second one.")
	}

	switch {
	case !causes.anyCauseEstablished():
		// R3: say it, rather than falling silent or falling back to asserting
		// a cause.
		//
		// Routed through the METHOD rather than re-derived inline. The
		// predicate "no candidate fault was established" is the branch's whole
		// content, and stating it twice — once in anyCauseEstablished, once as
		// a switch whose default arm happened to mean the same thing — left
		// the deliberate exclusion of Served re-encoded here by nothing more
		// than this switch not mentioning it. Fold Served into the method and
		// this renderer now visibly headlines a fault on a host where the
		// socket answers and nothing is wrong, which is what the method's own
		// doc comment argues must never happen.
		_, _ = fmt.Fprintln(out, "  NO CANDIDATE CAUSE WAS ESTABLISHED. Doctor reports each candidate below with what it could and could not settle, rather than naming one it did not measure.")
	case causes.Disabled == daemonSpawnCauseEstablished:
		// Ranked above the console arm: a disable override is decisive —
		// launchd will not spawn the job whatever else is true of the domain —
		// and reportDaemonSpawnHistory routes the both-established shape here
		// for exactly that reason.
		//
		// The one rung where doctor is CERTAIN of the cause was also the only
		// one in the whole ladder handing over prose rather than a literal
		// command. It now names the command, whenever the probe resolved the
		// label the command has to spell.
		remedy := "Re-enable it in that domain"
		if facts.EnableCommand != "" {
			remedy = "Re-enable it with: " + facts.EnableCommand
		}
		_, _ = fmt.Fprintf(out, "  ESTABLISHED CAUSE: the job carries a disable override in its launchd domain, so launchd will not spawn it whatever else is true of the domain. %s; confirm with: %s\n", remedy, facts.DisabledCommand)
	case causes.Console == daemonSpawnCauseEstablished:
		// Not reached from reportDaemonSpawnHistory: a console verdict
		// established with no disable override routes to
		// daemonSpawnRemediationConsole and its three lines, and one
		// established alongside a disable override is taken by the arm above.
		// Stated anyway so this renderer is total over its input and a future
		// caller cannot reach a silent branch.
		_, _ = fmt.Fprintf(out, "  ESTABLISHED CAUSE: %s is owned by another user, so this login session's launchd refuses new RunAtLoad spawns. Confirm with: %s\n", daemonConsoleDevicePath, daemonConsoleOwnerCommand)
	default:
		// Unreachable while anyCauseEstablished names exactly the two faults
		// the arms above headline. Stated so that a THIRD candidate added to
		// the method without a headline here cannot reach a silent branch: an
		// established fault the report says nothing about is the failure this
		// whole report replaces.
		_, _ = fmt.Fprintln(out, "  A CANDIDATE CAUSE WAS ESTABLISHED, and this build has no headline for it. Read the per-candidate verdicts below.")
	}

	_, _ = fmt.Fprintf(out, "  - console ownership (a GUI session backgrounded by fast user switching keeps its existing services running but refuses new RunAtLoad spawns): %s\n", causes.Console)
	switch causes.Console {
	case daemonSpawnCauseRuledOut:
		_, _ = fmt.Fprintf(out, "    %s is owned by the user this command runs as, so fast user switching is not what is holding the job.\n", daemonConsoleDevicePath)
	default:
		// Not-checked and established both hand the operator the command; so
		// does any verdict a future build adds. A bare default already
		// satisfies the exhaustive linter here (.golangci.yml sets
		// default-signifies-exhaustive), so naming the two arms bought
		// nothing but a second copy of this line to drift.
		_, _ = fmt.Fprintf(out, "    settle it with: %s — it must print the user bossd runs as.\n", daemonConsoleOwnerCommand)
	}

	_, _ = fmt.Fprintf(out, "  - a disable override on the job in its launchd domain: %s\n", causes.Disabled)
	switch causes.Disabled {
	case daemonSpawnCauseRuledOut:
		_, _ = fmt.Fprintln(out, "    launchd holds no disable override for this label in that domain.")
	default:
		// Same shape as the console candidate above, and for the same reason.
		_, _ = fmt.Fprintf(out, "    settle it with: %s — %s must not be listed with `=> true`.\n", facts.DisabledCommand, daemonSpawnDisabledLabelPhrase(facts.Label))
	}

	// The one candidate doctor cannot settle. It is environmental and
	// explicitly out of scope, which is exactly why it is NAMED with its
	// command rather than left out: an operator whose other candidates are all
	// ruled out otherwise has nowhere to go, which is the state the old
	// remediation left them in.
	_, _ = fmt.Fprintf(out, "  - the launchd domain is on-demand-only, or otherwise will not honour RunAtLoad: %s\n", daemonSpawnCauseNotChecked)
	_, _ = fmt.Fprintf(out, "    settle it with: %s — inspect the domain's own state and the job's RunAtLoad handling.\n", facts.DomainCommand)
}

// reportDaemonStartupFailureDirective names the only way to see a bossd startup
// failure that happens BEFORE the socket binds.
//
// BOS-1183's second invisible failure, in the same incident: bossd exited
// inside a fail-loud migration before it ever listened, and
// ~/Library/Logs/bossanova/bossd.stderr.log was 0 bytes. That file is written
// by launchd's own stdout/stderr redirect, so it holds nothing at all when
// launchd never ran the binary — neither `boss daemon status` nor doctor said a
// word about either half.
//
// It is deliberately a directive and not a probe. Detecting a pending migration
// means opening the database doctor is diagnosing, contending with a daemon
// that may well be running it; the cheaper pointer buys the same answer.
//
// It prints only when the daemon is known not to be serving. On a healthy
// machine it is noise, and noise is how the lines that matter get skipped.
func reportDaemonStartupFailureDirective(out io.Writer, stagedPath string, notServing bool) {
	if !notServing {
		return
	}
	_, _ = fmt.Fprintln(out, "startup diagnosis: a bossd failure before the socket binds (a fail-loud migration, for example) leaves nothing in ~/Library/Logs/bossanova/bossd.stderr.log — launchd writes that file through its own redirect, so it is empty when launchd never ran the binary at all")
	_, _ = fmt.Fprintf(out, "  to see such an error, run the staged bossd in the foreground: %s\n", stagedPath)
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
	reportDaemonIdentity(out, supervisionMetadata, supervisionMetadataErr)
	// Gathered ONCE for the whole run and threaded down, which is what makes
	// the claim true rather than merely written: BOS-1204 gave this status
	// three consumers — the ownership check, the substrate line, the
	// LaunchAgent gate and the spawn-history target — and separate reads of
	// settings.json inside one command are separate chances to render one
	// report about two different configurations.
	//
	// daemonSupervisionInputs is itself short-circuited by
	// BOSS_DAEMON_SKIP_LAUNCHCTL (daemonServiceProbingDisabled), so hoisting it
	// above reportDaemonSupervision's own env guard adds no launchctl probe on
	// a host that asked for none (BOS-1204 AC9).
	supervisionSubstrate, watchdogOwnership := daemonSupervisionInputs()
	supervisionUnhealthy, supervisionRemediation := reportDaemonSupervision(
		out, supervisionMetadata, supervisionMetadataErr, supervisionSubstrate, watchdogOwnership)
	// BOS-1184 R4, and cross-platform for the same reason the ownership check
	// above is: a settings key that the install path refuses to act on is not a
	// macOS concept, and this line is the only place an operator learns their
	// configured substrate is inert or rejected. It is kept OUT of
	// unhealthyNonAuth deliberately — that flag's remedy ladder ends in "run
	// 'boss daemon restart'", which cannot fix a value in settings.json, and
	// printing it would send an operator to restart a daemon over a typo.
	modeUnhealthy, modeRemediation := reportDaemonSupervisionMode(out, supervisionSubstrate)
	// Deliberately ABOVE the darwin early return, and argued rather than
	// mirrored by reflex. Staging genuinely IS a macOS concept — the TCC-stable
	// path exists because launchd needs one — and adding a symmetric
	// counterpart for it elsewhere would be cargo cult. Git ancestry is not: a
	// Linux operator runs an equally stale binary, and on that platform this is
	// the only check that would ever say so.
	//
	// Kept OUT of unhealthyNonAuth for the same reason the supervision-mode
	// check is: that flag's ladder ends in "run 'boss daemon restart'", and a
	// restart cannot replace an executing binary. It would send an operator to
	// restart a daemon over a build that needs rebuilding.
	bossRevisionUnhealthy, revisionRemediation := reportDaemonRevisionDrift(
		out, "boss binary", bossRevisionDriftProbe(cmd.Context()))
	bossdRevisionUnhealthy, bossdRevisionRemediation := reportDaemonRevisionDrift(
		out, "bossd binary", bossdFileRevisionDriftProbe(cmd.Context()))
	_, _ = fmt.Fprintln(out, daemonRunningProcessRevisionLine)
	revisionUnhealthy := bossRevisionUnhealthy || bossdRevisionUnhealthy
	if revisionRemediation == "" {
		revisionRemediation = bossdRevisionRemediation
	}
	if daemonDoctorGOOS != "darwin" {
		_, _ = fmt.Fprintf(out, "macOS daemon install and protected-folder checks: not applicable on %s\n", daemonDoctorGOOS)
		// The service-PATH and upstream-auth checks are NOT macOS-specific —
		// the systemd unit carries an explicit PATH too — so their verdicts
		// have to survive this early return. Returning nil here regardless
		// would make both a no-op on exactly the platform they matter most on.
		if servicePathStale || authUnhealthy || supervisionUnhealthy || modeUnhealthy || revisionUnhealthy {
			_, _ = fmt.Fprintln(out, "\nRemediation:")
			if servicePathStale || supervisionRemediation == daemonSupervisionRemediationRestart {
				_, _ = fmt.Fprintln(out, "  run 'boss daemon restart'")
			}
			if supervisionRemediation == daemonSupervisionRemediationInstallWatchdog {
				_, _ = fmt.Fprintln(out, daemonWatchdogNotLoadedRemediation())
			}
			if authRemediation {
				_, _ = fmt.Fprintln(out, "  run 'boss login'")
			}
			if modeUnhealthy {
				_, _ = fmt.Fprintln(out, modeRemediation)
			}
			if revisionUnhealthy {
				_, _ = fmt.Fprintln(out, revisionRemediation)
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

	// BOS-1204: under the unattended substrate there is deliberately NO
	// per-user LaunchAgent — platformInstallUnattended removes it, and
	// warnIfUnattendedWatchdogInstalled warns when a leftover one is found — so
	// reading it here reported `FAIL LaunchAgent plist … no such file` plus an
	// install remediation on a host whose supervision was working perfectly.
	//
	// Doctor defers to the substrate check it already has rather than probing
	// the watchdog again: reportDaemonSupervisionMode renders
	// describeUnattendedSupervisionMode, which already FAILs on absent and on
	// insecure and already names the watchdog plist. A second FAIL for the same
	// fact is the duplicate-failure shape reportDaemonSupervision's
	// `!st.Installed` rung is already written to avoid.
	//
	// The gate is the CONFIGURED mode, not the observed install state, and
	// deliberately so: on a host that selected this substrate and never ran the
	// root install, the LaunchAgent is equally absent and equally not the thing
	// to report — the substrate line says so, with the remedy that actually
	// works (`sudo boss daemon install`, which this block would never print).
	switch {
	case daemonUnattendedSubstrateConfigured(supervisionSubstrate):
		_, _ = fmt.Fprintf(out,
			"LaunchAgent plist: not applicable — the %s supervision substrate supersedes the per-user LaunchAgent; see the daemon supervision substrate line above\n",
			supervisionSubstrate.Mode)
	default:
		reportDaemonLaunchAgentPlist(out, stagedPath, &unhealthyNonAuth, &installRemediation)
	}

	// Placed inside the darwin-only section, unlike the auth and supervision
	// checks above: launchd spawn history is not a cross-platform concept, and a
	// Linux run must emit nothing new at all — not even a probe that prints
	// nothing.
	spawnUnhealthy, spawnRemediation, spawnFacts := reportDaemonSpawnHistory(out, stagedPath, supervisionSubstrate)
	if spawnUnhealthy {
		unhealthyNonAuth = true
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

	// The socket, not the process table, is the definition of "serving": the
	// BOS-1183 migration failure had a launchd job, a plist and a staged binary
	// and never bound anything. profileErr is a genuine third answer — with no
	// profile we cannot know whether the socket answers, and a verdict printed
	// on a guess sends an operator to foreground a bossd that is already up.
	//
	// The verdict is deliberately NOT widened by the spawn history. A job
	// launchd has never spawned can still be SERVED, and on this branch that is
	// the ordinary shape after the detached-fallback recovery: `boss daemon
	// start` spawns bossd directly, the socket answers, and launchd's runs
	// stays 0 forever. Folding spawnUnhealthy in here sent exactly that
	// operator to foreground a second bossd — the duplicate the three
	// isSocketReachable guards in platformEnsureRunning exist to prevent.
	//
	// notServing keeps its exact existing meaning — false for BOTH "reachable"
	// and "unknown" — because the foreground remedy is gated on it (R8).
	// socketKnown/socketReachable are the SEPARATE pair the candidate-cause
	// report needs, which must distinguish those two: "the socket answers" is
	// reassurance, and "we could not resolve a profile to ask" is not.
	notServing := false
	socketKnown := false
	socketReachable := false
	switch {
	case profileErr != nil:
		_, _ = fmt.Fprintf(out, "daemon socket: unknown (%v)\n", profileErr)
	case daemonSocketReachable(profile.SocketPath):
		socketKnown = true
		socketReachable = true
		_, _ = fmt.Fprintf(out, "daemon socket: %s — reachable\n", profile.SocketPath)
	default:
		socketKnown = true
		// Serving is what the daemon is FOR, so a socket known not to answer is
		// a failure verdict rather than a note. Without it the directive below
		// printed "run the staged bossd in the foreground" on a run that
		// emitted no Remediation section and exited 0 — doctor asserting health
		// while instructing recovery, which is R1's contradiction reproduced
		// inside doctor's own output.
		unhealthyNonAuth = true
		startRemediation = true
		notServing = true
		_, _ = fmt.Fprintf(out, "FAIL daemon socket: %s — not reachable, so bossd is not serving\n", profile.SocketPath)
	}
	spawnFacts.SocketKnown = socketKnown
	spawnFacts.SocketReachable = socketReachable

	reportDaemonStartupFailureDirective(out, stagedPath, notServing)

	if unhealthyNonAuth || authUnhealthy || modeUnhealthy || revisionUnhealthy {
		_, _ = fmt.Fprintln(out, "\nRemediation:")
		if unhealthyNonAuth {
			switch {
			case installRemediation:
				// Ahead of the console branch: a job with no plist has to be
				// installed before which domain it would land in can matter.
				_, _ = fmt.Fprintln(out, "  run 'boss daemon install'")
			case spawnRemediation == daemonSpawnRemediationConsole:
				// Deliberately NOT "run 'boss daemon start'", and ahead of the
				// startRemediation branch that would print it. launchd is not
				// going to spawn anything in this domain, so the only thing
				// that command can do is succeed by producing an UNSUPERVISED
				// bossd outside the login session — BOS-1183's third reported
				// failure, reached by following the remedy for its first.
				//
				// R4: these three lines are UNCHANGED. They were never wrong,
				// they were unconditional — and this branch is now reached
				// only once doctor has ESTABLISHED that another user owns the
				// console.
				_, _ = fmt.Fprintln(out, daemonSpawnConsoleRequirementLine)
				_, _ = fmt.Fprintln(out, "  A GUI session backgrounded by fast user switching keeps its existing services running but refuses new RunAtLoad spawns, so the job sits pending forever.")
				_, _ = fmt.Fprintln(out, "  Return that user's login session to the foreground console; launchd spawns the job once that user owns /dev/console again.")
			case spawnRemediation == daemonSpawnRemediationSpawnCauses:
				// Immediately after the console branch and ahead of everything
				// below, which keeps R8's precedence exactly: install still
				// wins over both never-spawned branches, and both still
				// precede the foreground and start remedies. launchd is not
				// going to spawn anything in this domain, so `boss daemon
				// start` could only succeed by producing an UNSUPERVISED bossd
				// outside the login session — BOS-1183's third reported
				// failure, reached by following the remedy for its first.
				reportDaemonSpawnCauses(out, spawnFacts)
			case spawnRemediation == daemonSpawnRemediationForeground && notServing:
				// Ahead of startRemediation, which an unreachable socket has
				// already set by this point, and ahead of the restart default.
				// launchd spawned bossd and bossd exited: restarting a binary
				// that starts and dies reproduces the crash, and `boss daemon
				// start` finds the job already loaded and does nothing at all.
				// The failure is only READABLE in the foreground, because the
				// launchd redirect that would hold it is written by launchd
				// itself and stays empty for a process that dies this early.
				//
				// Gated on notServing for the same reason
				// reportDaemonStartupFailureDirective is: a crash-loop stays on
				// launchd's record forever, so this rung is reached long after
				// the operator recovered with a detached `boss daemon start`.
				// Telling them to foreground a SECOND bossd over a socket that
				// already answers is the duplicate the three isSocketReachable
				// guards in platformEnsureRunning exist to prevent. A serving
				// but crash-marked job falls through to the restart default,
				// which is the coherent answer for a daemon that is up.
				_, _ = fmt.Fprintf(out, "  run the staged bossd in the foreground to see why it exits: %s\n", stagedPath)
			case supervisionRemediation == daemonSupervisionRemediationInstallWatchdog:
				// Ahead of BOTH per-user branches below. The fault is a
				// root-owned LaunchDaemon launchd does not have loaded, and
				// neither `boss daemon start` nor `boss daemon restart` can
				// load one — they would report success over an unchanged
				// fault, which is the misdirection this ladder exists to stop.
				_, _ = fmt.Fprintln(out, daemonWatchdogNotLoadedRemediation())
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
		if modeUnhealthy {
			_, _ = fmt.Fprintln(out, modeRemediation)
		}
		if revisionUnhealthy {
			_, _ = fmt.Fprintln(out, revisionRemediation)
		}
	}

	// A DISTINCT condition from the remediation gate above, not a duplicate:
	// this darwin path evaluates the unhealthy set twice, once to print the
	// remedy and once to decide the exit status. A verdict threaded into only
	// the first prints the remedy and still exits 0 — a doctor run that
	// instructs recovery while asserting health.
	if unhealthyNonAuth || authUnhealthy || modeUnhealthy || revisionUnhealthy {
		return errDaemonDoctorUnhealthy
	}
	return nil
}

// daemonSupervisionModeSettingsRemediation is the remedy for a supervision mode
// value this host will not act on. It is a settings edit, which is why it is
// printed alongside the restart/login/permission remedies rather than inside
// the unhealthyNonAuth ladder whose default branch is "run 'boss daemon
// restart'".
const daemonSupervisionModeSettingsRemediation = "  fix 'daemon_supervision_mode' in settings.json (remove the key to use the default)"

// daemonSupervisionModeRemediation picks the remedy that matches WHY the
// supervision substrate is unhealthy.
//
// BOS-1184 U3 made this a choice rather than a constant. A settings edit is the
// right answer for a value the resolver refused, and the wrong one for the two
// states the unattended substrate can be unhealthy in: an operator whose
// watchdog is simply not installed does not have a bad key to fix, and one
// whose watchdog sits on a user-writable path needs the path repaired, not the
// key removed. Printing the settings line for either would send them to edit a
// file that is already correct — the same misdirection this remedy was split
// out of the restart ladder to avoid.
//
// It reads the SAME gathered status describeDaemonSupervisionMode renders, so
// the verdict and its remedy cannot disagree about which state the host is in.
func daemonSupervisionModeRemediation(st daemon.SupervisionModeStatus) string {
	if st.Err == nil && st.Mode == daemon.SupervisionModeUnattended {
		switch st.Unattended.State {
		case daemon.UnattendedInstallAbsent:
			return fmt.Sprintf("  run `%s` to install the root-owned unattended supervision watchdog (macOS will prompt for an administrator password)", daemon.WatchdogInstallCommand)
		case daemon.UnattendedInstallInsecure:
			return fmt.Sprintf("  make the path named above root-owned and unwritable by any other user, then run `%s` followed by `%s`", daemon.WatchdogUninstallCommand, daemon.WatchdogInstallCommand)
		case daemon.UnattendedInstallPresent, daemon.UnattendedInstallNotApplicable:
			// Nothing to remedy about the substrate itself. The settings line
			// below is the honest fallback: this function is only consulted
			// when SOMETHING is unhealthy, and if it was not the substrate then
			// the configuration is the only thing left for it to speak to.
		}
	}
	return daemonSupervisionModeSettingsRemediation
}

// reportDaemonSupervisionMode prints the CONFIGURED supervision substrate,
// reports whether it is one this host refuses to act on, and returns the remedy
// that matches.
//
// The verdict and the wording are both describeDaemonSupervisionMode's — the
// same call `boss daemon status` makes — so the two surfaces cannot name
// different modes for one settings file. Only the FAIL marker is chosen here,
// which is this surface's own voice: status labels the fact, doctor grades it.
// The status is loaded ONCE and fed to both the description and the remedy, so
// a host cannot be described in one state and remediated for another.
func reportDaemonSupervisionMode(out io.Writer, status daemon.SupervisionModeStatus) (unhealthy bool, remediation string) {
	description, unhealthy := describeDaemonSupervisionMode(status)
	if unhealthy {
		_, _ = fmt.Fprintf(out, "daemon supervision substrate: FAIL %s\n", description)
	} else {
		_, _ = fmt.Fprintf(out, "daemon supervision substrate: %s\n", description)
	}
	return unhealthy, daemonSupervisionModeRemediation(status)
}

// daemonRevisionDriftRemediation is the remedy for a binary that does not
// contain the checkout's commits.
//
// It is a rebuild, which is why the verdict rides its own flag rather than
// unhealthyNonAuth: that ladder ends in "run 'boss daemon restart'", and a
// restart re-executes the same stale bytes and then reports success. That is
// the exact misdirection the incident behind this check produced, where a
// source-level inspection concluded the failure was impossible while the
// machine that suffered it was still running the old build.
const daemonRevisionDriftRemediation = "  rebuild and reinstall the boss binaries from this checkout " +
	"('make build', then install the rebuilt binaries) — restarting re-executes the same bytes"

// daemonRunningProcessRevisionLine states, explicitly, that the LIVE daemon
// process's revision is not obtainable.
//
// daemonstate.Metadata records the PID, the executable path and the start time
// but carries no build stamp, and DaemonService exposes no build-info RPC, so
// there is nothing to read. Closing that gap means stamping the metadata at
// daemon startup, which is deliberately out of scope here.
//
// Printed rather than omitted. This file's trichotomy tests already pin that
// doctor must not fabricate a line about a thing it never observed; the inverse
// holds too, because silently dropping a fact the operator came here for reads
// as "checked, fine".
//
// The phrasing avoids the literal "running bossd" deliberately.
// TestRunDaemonDoctorReportsMacOSChecksNotApplicable asserts that the
// non-darwin path emits no such token, guarding the darwin-only staging and
// liveness lines from leaking onto Linux. This line is cross-platform — no
// daemonstate record on any platform carries a build stamp — so it belongs
// above the early return, and the right fix was to word it distinctly rather
// than to loosen that guard.
const daemonRunningProcessRevisionLine = "live daemon process revision: unknown — " +
	"the daemon records no build stamp, so only the bossd file above was compared"

// daemonDoctorBossdRevisionTimeout bounds the `bossd --version` probe. Short
// for the same reason daemonAuthProbeTimeout is: an unbounded advisory
// subprocess inside a diagnostic has already caused a multi-minute silent
// stall in this codebase.
const daemonDoctorBossdRevisionTimeout = 5 * time.Second

// daemonDoctorBossdRevisionWaitDelay bounds the output pipe's close after the
// deadline above has already killed the child, so the two bounds are one bound.
const daemonDoctorBossdRevisionWaitDelay = 2 * time.Second

// bossdVersionRevisionRE extracts the version and revision from bossd's
// --version line, which is `"bossd " + buildinfo.String()` —
// `bossd <version> (<commit>) built <date>`.
var bossdVersionRevisionRE = regexp.MustCompile(`^bossd\s+(\S+)\s+\(([^)]*)\)`)

// daemonDoctorRevisionGit is the git seam the revision-drift probe runs
// through. A package var for the same reason skillDriftHistoryGit is: left
// unstubbed, every doctor test shells out to the DEVELOPER's real repository
// and the suite's verdicts become machine-dependent.
var daemonDoctorRevisionGit revisiondrift.GitRunner = revisiondrift.ExecGit

// daemonDoctorBossRevision reads the EXECUTING boss binary's build stamp.
//
// A function var over buildinfo's package globals, following
// upgradeCurrentVersion in handlers.go: the seam is the read rather than the
// global, so a test never mutates build metadata another test may be reading.
var daemonDoctorBossRevision = func() (revision, version string) {
	return buildinfo.Commit, buildinfo.Version
}

// daemonDoctorBossdRevision reads the installed bossd FILE's build stamp by
// running it. bossd accepts --version through the stdlib flag package; `boss`
// sets no Version field on its root command and rejects the flag outright,
// which is why this direction is the only one available.
var daemonDoctorBossdRevision = readBossdFileRevision

// daemonDoctorTrustCheckout authenticates a resolved checkout before its
// history becomes the reference, so a look-alike directory above the working
// directory cannot supply the comparison. It delegates to the predicate this
// package already owns rather than restating the canonical-repository identity.
var daemonDoctorTrustCheckout = func(root string) (bool, error) {
	return trustedSkillSourceRoot(filepath.Join(root, libskillinstall.SourceRelPath))
}

// bossRevisionDriftProbe and bossdFileRevisionDriftProbe are the two probes,
// seamed as whole units rather than as their four inputs. A fixture that pins a
// probe cannot accidentally leave one machine read live, which is exactly what
// pinning three of four seams would do.
//
// They are separate because their costs are separate. The `boss` verdict is
// wanted by three surfaces; the `bossd` file verdict costs a subprocess and is
// wanted only by the diagnostic that can also act on it, so `boss env` and the
// per-invocation warning must not pay for it.
var (
	bossRevisionDriftProbe      = inspectBossRevisionDrift
	bossdFileRevisionDriftProbe = inspectBossdFileRevisionDrift
)

// readBossdFileRevision runs `bossd --version` and parses its build stamp.
//
// Bounded and with stderr discarded: this is a diagnostic reading a sibling
// binary, and a bossd that prints a diagnostic of its own must not become this
// command's output.
func readBossdFileRevision(ctx context.Context, path string) (revision, version string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, daemonDoctorBossdRevisionTimeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdout = &stdout
	// Bounds the pipe-close wait, not the process, for the reason
	// revisiondrift.ExecGit sets it: capturing output makes os/exec copy
	// through a pipe, and Run blocks until every writer closes it, so a bossd
	// that forks before printing would hang this diagnostic past the deadline
	// that already killed it.
	cmd.WaitDelay = daemonDoctorBossdRevisionWaitDelay
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("run %s --version: %w", path, err)
	}
	match := bossdVersionRevisionRE.FindStringSubmatch(strings.TrimSpace(stdout.String()))
	if match == nil {
		return "", "", fmt.Errorf("%s --version did not report a build stamp", path)
	}
	return match[2], match[1], nil
}

// revisionDriftOptions builds the inputs both probes share, so the `boss` and
// `bossd` verdicts are decided against the same checkout, resolved once the
// same way. Two resolutions are two chances to report one machine against two
// references.
func revisionDriftOptions() revisiondrift.Options {
	startDir, err := os.Getwd()
	if err != nil {
		startDir = "."
	}
	return revisiondrift.Options{
		StartDir:      startDir,
		Git:           daemonDoctorRevisionGit,
		TrustCheckout: daemonDoctorTrustCheckout,
	}
}

// inspectBossRevisionDrift classifies the EXECUTING boss binary against the
// checkout the command is being run from.
func inspectBossRevisionDrift(ctx context.Context) revisiondrift.Drift {
	options := revisionDriftOptions()
	options.BinaryRevision, options.BinaryVersion = daemonDoctorBossRevision()
	return revisiondrift.Inspect(ctx, options)
}

// inspectBossdFileRevisionDrift classifies the installed bossd FILE — not the
// running daemon process, whose revision is not obtainable today (see
// daemonRunningProcessRevisionLine).
func inspectBossdFileRevisionDrift(ctx context.Context) revisiondrift.Drift {
	bossdPath, err := daemon.ResolveBossdPath()
	if err != nil {
		return revisiondrift.Unknown(revisiondrift.ReasonRevisionUnreadable, err.Error())
	}
	revision, version, err := daemonDoctorBossdRevision(ctx, bossdPath)
	if err != nil {
		// An unreadable stamp is its OWN outcome, not the unstamped one: that
		// would assert how the binary was built, which nothing here observed.
		//
		// Through revisiondrift.Unknown rather than a Drift literal, because
		// Inspect is the only other place the fact flags are established and a
		// literal sets them by omission — which is how RevisionStamped came to
		// read false here for no reason but that nothing was read.
		return revisiondrift.Unknown(revisiondrift.ReasonRevisionUnreadable, err.Error())
	}
	options := revisionDriftOptions()
	options.BinaryRevision, options.BinaryVersion = revision, version
	return revisiondrift.Inspect(ctx, options)
}

// reportDaemonRevisionDrift prints whether one binary contains the commits this
// checkout has, and returns the remedy that matches.
//
// Mirrors reportDaemonSupervisionMode: the classifier owns the verdict and the
// wording, and this surface owns only the FAIL marker — status labels the fact,
// doctor grades it. An unknown outcome prints its own distinct cause and leaves
// the verdict alone, because an undeterminable input is neither healthy nor
// unhealthy and rendering it as either is the conflation the classifier exists
// to prevent.
func reportDaemonRevisionDrift(out io.Writer, binaryLabel string, drift revisiondrift.Drift) (unhealthy bool, remediation string) {
	line := drift.Describe(binaryLabel)
	switch {
	case drift.BehindKnown && drift.Behind:
		_, _ = fmt.Fprintf(out, "FAIL %s\n", line)
		return true, daemonRevisionDriftRemediation
	case drift.Unknown():
		_, _ = fmt.Fprintf(out, "%s — unknown, so neither healthy nor stale\n", line)
		return false, ""
	default:
		_, _ = fmt.Fprintln(out, line)
		return false, ""
	}
}

// reportDaemonLaunchAgentPlist is the per-user LaunchAgent block, lifted out of
// runDaemonDoctor unchanged so BOS-1204's substrate gate could skip it as a
// unit rather than by wrapping a hundred lines in an `if`.
//
// The two flags are pointers because both are accumulators runDaemonDoctor
// keeps building after this returns; returning them would have made the call
// site re-implement the OR at every rung, which is the shape that lets one rung
// print FAIL while reporting healthy.
func reportDaemonLaunchAgentPlist(out io.Writer, stagedPath string, unhealthyNonAuth, installRemediation *bool) {
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		*unhealthyNonAuth = true
		_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist: resolve home directory: %v\n", homeErr)
		return
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
	programArguments, plistErr := readLaunchAgentProgramArguments(plistPath)
	switch {
	case plistErr != nil:
		*unhealthyNonAuth = true
		*installRemediation = errors.Is(plistErr, os.ErrNotExist)
		_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist %s: %v\n", plistPath, plistErr)
	case len(programArguments) == 0:
		*unhealthyNonAuth = true
		_, _ = fmt.Fprintf(out, "FAIL LaunchAgent ProgramArguments: no executable configured in %s\n", plistPath)
	default:
		programPath := programArguments[0]
		_, _ = fmt.Fprintf(out, "LaunchAgent ProgramArguments: %s\n", strings.Join(programArguments, " "))
		if strings.Contains(programPath, "/Cellar/") || filepath.Clean(programPath) != filepath.Clean(stagedPath) {
			*unhealthyNonAuth = true
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent executable must be staged at %s, not %s\n", stagedPath, programPath)
		}
	}
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
