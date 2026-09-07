//go:build darwin

// This file is the half of the `unattended` supervision substrate that touches
// the machine: it observes real path ownership, writes the root-owned artifacts
// and drives launchctl in the `system` domain. The rule it enforces and the
// argv it writes both live in watchdog.go, where they are pure and provable
// from either platform.
package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/recurser/bossalib/daemonbin"
)

// These live here rather than in watchdog.go because every consumer is
// darwin-only. Untagged, the Linux build compiles them with nothing referencing
// them and `unused` fails the tree-wide lint — which is what CI runs and a
// darwin-only local `make lint` never sees.
const (
	// watchdogRootOwnerUID is the uid every path in the layout must be owned by.
	watchdogRootOwnerUID uint32 = 0

	// watchdogPlistMode and watchdogBinaryMode are the modes the installer
	// writes and the verifier then re-reads from disk. launchd refuses to load a
	// group- or world-writable plist, so 0644 is not a preference; and 0755 is
	// what a root-owned executable a non-root process must be able to exec
	// needs.
	watchdogPlistMode  fs.FileMode = 0o644
	watchdogBinaryMode fs.FileMode = 0o755
	watchdogDirMode    fs.FileMode = 0o755
)

// watchdogFilesystemRoot is the filesystem root the layout is built under. It
// is "/" in production and a temp directory under test, which is what lets the
// install, uninstall and verification paths be exercised without writing to
// this machine's real /Library/LaunchDaemons. It follows the runLaunchctl /
// executablePath package-var indirection idiom.
var watchdogFilesystemRoot = "/"

// watchdogPaths is the layout for this process. newWatchdogLayout itself stays
// platform-independent in watchdog.go, where the untagged tests derive layouts
// under their own roots; only this process-wide accessor is darwin-only.
func watchdogPaths() watchdogLayout { return newWatchdogLayout(watchdogFilesystemRoot) }

// The package-var seams below follow the runLaunchctl / executablePath idiom.
// They exist so the install, uninstall and verification paths are testable
// without root and without mutating this host: a test cannot chown a file to
// root, and must not, so it declares its own uid trusted and stubs the chown.
var (
	// currentEUID reports the effective uid of this process.
	currentEUID = os.Geteuid

	// chownToRoot makes a path root:wheel. Stubbed in tests, where the process
	// is not root and the real call would always fail.
	chownToRoot = func(path string) error { return os.Chown(path, 0, 0) }

	// watchdogTrustedOwnerUID is the uid every watchdog path must be owned by.
	// Production is root; tests set their own uid so a real directory tree can
	// stand in for a root-owned one and the REFUSALS stay meaningful.
	watchdogTrustedOwnerUID = watchdogRootOwnerUID

	// lookupUserByUID resolves a uid to the account's NAME and home directory.
	// The name is returned as well as the home because SUDO_USER and SUDO_UID
	// are two independent environment values and the rendered argv uses both —
	// see resolveUnattendedTarget.
	lookupUserByUID = func(uid int) (name, home string, err error) {
		u, err := user.LookupId(strconv.Itoa(uid))
		if err != nil {
			return "", "", fmt.Errorf("look up uid %d: %w", uid, err)
		}
		return u.Username, u.HomeDir, nil
	}
)

// statPathSecurity observes one path with Lstat, so a symlink is reported as a
// symlink rather than silently resolved to whatever it points at.
func statPathSecurity(path, role string) pathSecurityFact {
	fact := pathSecurityFact{Path: path, Role: role}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fact.Missing = true
		return fact
	case err != nil:
		fact.StatErr = err
		return fact
	}
	fact.Mode = info.Mode()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		fact.OwnerUID = stat.Uid
	} else {
		// Unreachable on darwin, and left as an explicit refusal rather than a
		// default of 0: reporting "owned by root" for an owner we could not
		// read is the one wrong answer this whole check exists to prevent.
		fact.StatErr = fmt.Errorf("owner uid unavailable for %s", path)
	}
	return fact
}

// watchdogFactFilter selects which of the security targets one pass observes.
//
// The three passes below differ ONLY by these two booleans. Writing them as
// three loops meant the differences lived in prose above each one, and a change
// to the shared part had to be made three times and kept in step by reading —
// on the code path whose whole job is refusing a privilege escalation.
type watchdogFactFilter struct {
	// SkipArtifacts omits the plist and the binary, leaving the directories.
	// Used by the passes that run before either artifact has been written.
	SkipArtifacts bool
	// TolerateMissing drops paths that do not exist yet, instead of letting the
	// rule refuse them. Only the pre-create pass sets it: on a clean host the
	// layout directories legitimately do not exist, and the installer is about
	// to create them itself with an explicit mode and root ownership.
	TolerateMissing bool
}

// gatherWatchdogPathFacts observes the security targets this pass covers.
func gatherWatchdogPathFacts(layout watchdogLayout, filter watchdogFactFilter) []pathSecurityFact {
	targets := watchdogSecurityTargets(layout)
	facts := make([]pathSecurityFact, 0, len(targets))
	for _, target := range targets {
		if filter.SkipArtifacts && target.IsArtifact {
			continue
		}
		fact := statPathSecurity(target.Path, target.Role)
		if filter.TolerateMissing && fact.Missing {
			continue
		}
		facts = append(facts, fact)
	}
	return facts
}

// verifyWatchdogPaths is the gate: it refuses with ErrUnattendedInsecurePath
// unless the plist, the binary and every directory above them are owned by the
// trusted uid and unwritable by anyone else. It runs after both artifacts are
// written and before launchctl is asked to bootstrap the job.
func verifyWatchdogPaths(layout watchdogLayout) error {
	return classifyWatchdogPathSecurity(
		gatherWatchdogPathFacts(layout, watchdogFactFilter{}), watchdogTrustedOwnerUID)
}

// verifyWatchdogDirs runs the same rule over the DIRECTORIES only, in full — a
// missing one is refused. It runs once the installer has created them and
// before either artifact is written into them.
//
// It is not redundant with verifyWatchdogPaths. Creating a root-owned file
// inside a directory a non-root user can write is itself the hole — that user
// can pre-create the path as a symlink, or replace it between our write and
// launchd's read. Checking the containing directories first means we never
// write into a tree we would go on to reject.
func verifyWatchdogDirs(layout watchdogLayout) error {
	return classifyWatchdogPathSecurity(
		gatherWatchdogPathFacts(layout, watchdogFactFilter{SkipArtifacts: true}), watchdogTrustedOwnerUID)
}

// verifyWatchdogDirsBeforeCreate is the PRE-write half of the directory gate,
// run before the installer creates anything.
//
// verifyWatchdogDirs cannot serve here: on a clean host the layout directories
// legitimately do not exist yet, and the rule refuses a missing path, so
// running it first would refuse every first install.
//
// Tolerating absence is safe; tolerating a pre-planted entry is not. The
// mkdir/chmod/chown sequence resolves symlinks (see mkdirRootOwned), and an
// existing directory owned by somebody else, or group-writable, is a directory
// we would be writing a root-owned artifact into — so those refusals are the
// ones this pass keeps. It is pinned on its own by
// TestPlatformInstallUnattendedRefusesAPrePlantedWritableLayoutDirectory.
func verifyWatchdogDirsBeforeCreate(layout watchdogLayout) error {
	return classifyWatchdogPathSecurity(
		gatherWatchdogPathFacts(layout, watchdogFactFilter{SkipArtifacts: true, TolerateMissing: true}),
		watchdogTrustedOwnerUID)
}

// unattendedTarget is the account the watchdog drops privileges to.
type unattendedTarget struct {
	UID  int
	User string
	Home string
}

// resolveUnattendedTarget answers WHICH user bossd should run as when the
// installer itself is root.
//
// SUDO_UID / SUDO_USER are the source because they name the operator who
// invoked `sudo boss daemon install` — the person whose login keychain,
// worktrees and settings bossd must reach. Falling back to the current process
// owner would silently install a watchdog that runs bossd as ROOT, which is the
// privilege escalation this whole file is arranged to prevent, so a root login
// with no SUDO_* is refused rather than guessed at.
//
// The two values are also cross-checked against each other, because they are
// two INDEPENDENT strings in an environment the caller controls and the
// rendered argv mixes them:
//
//	launchctl asuser <SUDO_UID> /usr/bin/sudo -u <SUDO_USER> /usr/bin/env HOME=<home of SUDO_UID> … bossd
//
// A mismatched pair therefore installs a persistent root-owned job that runs
// one account's bossd inside ANOTHER account's bootstrap and audit session,
// with that other account's HOME — and nothing downstream would ever notice,
// because verifyWatchdogPaths inspects file modes, not identities. The uid is
// treated as the authority (it is what asuser and the home lookup both use) and
// the name must be the one that uid actually resolves to.
func resolveUnattendedTarget() (unattendedTarget, error) {
	name := strings.TrimSpace(os.Getenv("SUDO_USER"))
	rawUID := strings.TrimSpace(os.Getenv("SUDO_UID"))
	if name == "" || rawUID == "" || name == "root" {
		return unattendedTarget{}, fmt.Errorf(
			"%w: could not tell which user bossd should run as (SUDO_USER/SUDO_UID are unset or name root). "+
				"Run `%s` from that user's own shell rather than from a root login — installing a watchdog that ran bossd as root would be a privilege escalation, so it is refused instead of guessed",
			ErrUnattendedRequiresRoot, WatchdogInstallCommand)
	}
	uid, err := strconv.Atoi(rawUID)
	if err != nil || uid <= 0 {
		return unattendedTarget{}, fmt.Errorf("%w: SUDO_UID %q is not a usable uid", ErrUnattendedRequiresRoot, rawUID)
	}
	resolvedName, home, err := lookupUserByUID(uid)
	if err != nil {
		return unattendedTarget{}, fmt.Errorf("%w: %v", ErrUnattendedRequiresRoot, err)
	}
	if resolved := strings.TrimSpace(resolvedName); resolved != name {
		return unattendedTarget{}, fmt.Errorf(
			"%w: SUDO_USER %q and SUDO_UID %d disagree — uid %d is %q. The watchdog would run %q's bossd inside %q's login session with %q's HOME, so the pair is refused rather than reconciled",
			ErrUnattendedRequiresRoot, name, uid, uid, resolved, name, resolved, resolved)
	}
	if strings.TrimSpace(home) == "" {
		return unattendedTarget{}, fmt.Errorf("%w: uid %d has no home directory", ErrUnattendedRequiresRoot, uid)
	}
	return unattendedTarget{UID: uid, User: name, Home: home}, nil
}

// requireUnattendedRoot refuses a privileged operation attempted as a normal
// user, naming the exact command to run instead.
//
// Failing loudly here rather than letting the first write fail with EACCES is
// the point: `boss daemon install` is normally run as the user, and a
// half-written root-owned tree is worse than no install at all.
func requireUnattendedRoot(command string) error {
	if currentEUID() == 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: the %q supervision mode installs a root-owned LaunchDaemon (%s), which needs root — run `%s`. "+
			"macOS will prompt for an administrator password, so this step cannot complete unattended",
		ErrUnattendedRequiresRoot, SupervisionModeUnattended, WatchdogLabel, command)
}

// mkdirRootOwned creates a directory tree and makes the LEAF directory
// root-owned with an explicit mode. MkdirAll honours the process umask, so the
// mode is re-applied rather than trusted.
//
// It REFUSES a path that already exists as a symlink, and that refusal is the
// load-bearing part rather than a nicety. None of the three calls below is
// symlink-safe: os.MkdirAll stats THROUGH a link and returns nil when it
// resolves to a directory, and os.Chmod and os.Chown both follow it —
// os.Lchown exists precisely because os.Chown does not. Measured: with a
// symlink at the target pointing at a 0700 directory, MkdirAll and Chmod both
// returned nil, the link kept its own mode and the TARGET became 0755. So
// wherever a non-root user can create an entry at one of these paths — the
// Homebrew-owns-/usr/local case is the real one — an unguarded call here would
// chmod and chown root:wheel a path of that user's choosing, and the
// after-the-fact verification would refuse only once the damage was done.
//
// Intermediate levels MkdirAll creates are NOT revisited: they keep MkdirAll's
// own mode and the installer's ownership, which is root only because
// requireUnattendedRoot has already run. verifyWatchdogDirsBeforeCreate is
// what proves the levels that already exist are safe to write through, and
// verifyWatchdogDirs re-proves the whole chain afterwards; neither is
// redundant with this.
func mkdirRootOwned(dir string, mode fs.FileMode) error {
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink, which can be repointed at another target without changing any mode bits — refusing to chmod or chown through it",
				ErrUnattendedInsecurePath, dir)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s could not be inspected (%v), so it cannot be shown safe to create",
			ErrUnattendedInsecurePath, dir, err)
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set mode on %s: %w", dir, err)
	}
	if err := chownToRoot(dir); err != nil {
		return fmt.Errorf("set root ownership on %s: %w", dir, err)
	}
	return nil
}

// platformInstallUnattended installs the root-owned watchdog LaunchDaemon.
//
// The ordering is the design, and it is a THREE-stage gate rather than two:
//
//  1. Root is required before anything is touched.
//  2. Every layout directory that ALREADY EXISTS is proven safe before the
//     installer creates, chmods or chowns anything
//     (verifyWatchdogDirsBeforeCreate). This rung exists because mkdir, chmod
//     and chown all resolve symlinks, so a check that ran only afterwards
//     would refuse an install that had already chmod'd and chown'd an
//     attacker-chosen path — and then leave that behind.
//  3. The directories are re-verified once created (verifyWatchdogDirs), the
//     artifacts are written with explicit modes and root ownership, and the
//     WHOLE tree is re-verified from disk before the job is bootstrapped
//     (verifyWatchdogPaths).
//
// The last step is what makes the security posture enforced rather than merely
// intended — it re-reads what is actually on the filesystem, so an ancestor a
// package manager took ownership of (Homebrew owns /usr/local on Intel Macs)
// stops the install instead of shipping a root-execution primitive.
func platformInstallUnattended(bossdPath string, force bool) error {
	if err := requireUnattendedRoot(WatchdogInstallCommand); err != nil {
		return err
	}
	target, err := resolveUnattendedTarget()
	if err != nil {
		return err
	}

	layout := watchdogPaths()
	if !force {
		if _, statErr := os.Stat(layout.PlistPath); statErr == nil {
			return fmt.Errorf("watchdog plist already exists at %s (use --force to overwrite)", layout.PlistPath)
		}
	}

	if err := verifyWatchdogDirsBeforeCreate(layout); err != nil {
		return err
	}
	for _, dir := range []string{layout.BinDir, layout.LogDir, filepath.Dir(layout.PlistPath)} {
		if err := mkdirRootOwned(dir, watchdogDirMode); err != nil {
			return err
		}
	}
	if err := verifyWatchdogDirs(layout); err != nil {
		return err
	}

	// The root-owned copy, never EnsureStaged's per-user one. daemonbin.Stage
	// is reused for the copy itself (same atomic write, same 0755) and then the
	// ownership is forced, because staging alone would leave the file owned by
	// whoever ran the installer.
	if err := daemonbin.Stage(bossdPath, layout.BinaryPath); err != nil {
		return fmt.Errorf("stage root-owned bossd at %s: %w", layout.BinaryPath, err)
	}
	if err := os.Chmod(layout.BinaryPath, watchdogBinaryMode); err != nil {
		return fmt.Errorf("set mode on %s: %w", layout.BinaryPath, err)
	}
	if err := chownToRoot(layout.BinaryPath); err != nil {
		return fmt.Errorf("set root ownership on %s: %w", layout.BinaryPath, err)
	}

	plist, err := renderWatchdogPlist(watchdogSpec{
		UID:       target.UID,
		User:      target.User,
		Home:      target.Home,
		BossdPath: layout.BinaryPath,
		Path:      serviceEnvPath(),
		LogDir:    layout.LogDir,
	})
	if err != nil {
		return err
	}
	// 0644, not 0600: launchd refuses a group- or world-WRITABLE plist, and
	// also has to be able to read it. The mode is re-applied after the write
	// because WriteFile honours the umask.
	if err := os.WriteFile(layout.PlistPath, []byte(plist), watchdogPlistMode); err != nil {
		return fmt.Errorf("write watchdog plist: %w", err)
	}
	if err := os.Chmod(layout.PlistPath, watchdogPlistMode); err != nil {
		return fmt.Errorf("set mode on %s: %w", layout.PlistPath, err)
	}
	if err := chownToRoot(layout.PlistPath); err != nil {
		return fmt.Errorf("set root ownership on %s: %w", layout.PlistPath, err)
	}

	if err := verifyWatchdogPaths(layout); err != nil {
		return err
	}

	// BOS-1203: supersede the substrate this one replaces. Nothing removed the
	// per-user LaunchAgent before, so it sat on disk with RunAtLoad and got
	// bootstrapped at the target user's next login — a second supervisor beside
	// a root job with KeepAlive=true, permanently, across reboots.
	//
	// The file removal is above the skipLaunchctl gate and the bootout below
	// it, which is the ordering the rest of this function already uses: every
	// file write happens above that gate and only service operations sit below.
	// Putting the removal below it would make the removal unreachable through
	// the package's own test seam.
	removeSupersededLaunchAgent(target)

	if skipLaunchctl() {
		return nil
	}
	out, err := runLaunchctl("bootstrap", "system", layout.PlistPath)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap system: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	// The bootout follows the bootstrap rather than preceding it, so there is a
	// brief window in which both jobs are loaded. That is deliberate and
	// bounded: bossd holds a singleton lock, so whichever instance loses exits
	// cleanly, and ordering it the other way would instead leave a window with
	// NO supervisor — the BOS-1181 net-loss-of-service shape.
	bootoutSupersededLaunchAgent(target)
	return nil
}

// warnLaunchAgentNotSuperseded reports a per-user LaunchAgent the unattended
// install could not clear. Package var, following the warnDaemonRefreshFailed
// idiom.
//
// It is a dedicated warning rather than a reuse of warnUnattendedWatchdogResidue.
// That one's wording is deliberately verb-NEUTRAL because three callers share
// it, and its subject is the opposite artifact: a root-owned watchdog left on a
// host no longer configured for it. Superseding is a fourth, different
// semantic, and telling an operator here to run `sudo boss daemon uninstall`
// would name a remedy for a problem they do not have.
var warnLaunchAgentNotSuperseded = func(subject, reason string) {
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: the unattended supervision watchdog is installed, but %s: %s. It can be bootstrapped again at the next login and contend with the watchdog for one bossd socket — clear it by hand once the cause above is fixed\n",
		subject, reason)
}

// removeSupersededLaunchAgent unlinks the per-user LaunchAgent plist the
// unattended substrate replaces.
//
// The path is derived from target.Home, NEVER from platformServicePath(). That
// helper reads os.UserHomeDir(), which under `sudo` is ROOT's home, so it would
// resolve a file in /var/root and leave the operator's own plist exactly where
// it was. resolveUnattendedTarget has already run at the top of the install and
// has already cross-checked SUDO_USER against SUDO_UID, so target.Home is the
// only home that has been proven to belong to the user this watchdog runs bossd
// as.
//
// The whole step is NON-FATAL, which diverges from this file's
// fail-closed-on-create convention and is deliberate: by this point the
// watchdog is written, verified and about to be bootstrapped, and failing the
// install in order to report a leftover file would be strictly worse than
// completing it and reporting the leftover file (R8).
//
// It refuses to unlink THROUGH a symlinked parent. Both `Library` and
// `LaunchAgents` are user-owned directories the invoking user could have
// repointed before running `sudo`, and os.Remove follows the path — this is the
// same class mkdirRootOwned refuses at length. The impact is bounded (a fixed
// basename, a non-sensitive file), so it warns and skips rather than aborting
// the install.
func removeSupersededLaunchAgent(target unattendedTarget) {
	launchAgentsDir := filepath.Join(target.Home, "Library", "LaunchAgents")
	plistPath := filepath.Join(launchAgentsDir, Label+".plist")
	subject := fmt.Sprintf("the per-user LaunchAgent at %s was left behind", plistPath)

	for _, dir := range []string{filepath.Join(target.Home, "Library"), launchAgentsDir} {
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// R8: this is "could not verify", not success. An unmounted or
			// network home has no Library directory, and reporting a clean
			// supersession there would claim something never observed.
			warnLaunchAgentNotSuperseded(subject,
				fmt.Sprintf("%s does not exist, so whether a LaunchAgent is installed could not be established", dir))
			return
		case err != nil:
			warnLaunchAgentNotSuperseded(subject, fmt.Sprintf("%s could not be inspected (%v)", dir, err))
			return
		case info.Mode()&fs.ModeSymlink != 0:
			warnLaunchAgentNotSuperseded(subject,
				fmt.Sprintf("%s is a symlink, which can be repointed at another target without changing any mode bits, so unlinking through it is refused", dir))
			return
		}
	}

	// An ENOENT on the plist itself with the directory present is SUCCESS, not
	// a fault: it is the ordinary state of a host that never installed the
	// LaunchAgent, and it is also the `--force` reinstall path. Warning here
	// would make the warning noise on the commonest run.
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		warnLaunchAgentNotSuperseded(subject, err.Error())
	}
}

// bootoutSupersededLaunchAgent unloads the per-user LaunchAgent job, if the
// target user has a gui domain for it to be loaded in.
//
// The target is built from target.UID rather than by calling
// bootoutLaunchdService, and that is the whole reason this function exists.
// That helper hard-codes os.Getuid(), which is 0 under `sudo` — so it would
// boot out gui/0/com.bossanova.bossd, a job that does not exist, report
// success, and leave the operator's own agent loaded. bootoutLaunchdTarget
// takes an explicit target for exactly this case.
//
// It must not fail closed. A target user with no Aqua session — headless, or
// not logged in since boot — has no gui/<uid> domain at all, and
// bootoutLaunchdTarget with a nil probe surfaces that as an error rather than
// as "already gone". Failing the install there would refuse to install a
// watchdog on precisely the unattended host the mode exists for.
func bootoutSupersededLaunchAgent(target unattendedTarget) {
	service := "gui/" + strconv.Itoa(target.UID) + "/" + Label
	if err := bootoutLaunchdTarget(service, nil); err != nil {
		warnLaunchAgentNotSuperseded(
			fmt.Sprintf("the per-user LaunchAgent job %s may still be loaded", service),
			err.Error())
	}
}

// watchdogJobStillLoaded probes whether the system-domain watchdog job is still
// registered with launchd.
//
// It fails CLOSED, exactly as mcpStillRunningProbe does: only an exit code that
// positively means "no such service" is read as gone. A probe that failed for
// any other reason is "cannot tell", and reporting "gone" there would turn an
// unverifiable end state into a success. It is a package var so a test can
// state the two answers without a launchd to ask.
var watchdogJobStillLoaded = func() bool {
	if _, err := runLaunchctl("print", "system/"+WatchdogLabel); err != nil {
		return !launchctlExitSaysAlreadyGone(err)
	}
	return true
}

// bootoutWatchdogJob boots out the system-domain watchdog and VERIFIES the job
// is actually gone before reporting an error, rather than trusting launchctl's
// exit code alone.
func bootoutWatchdogJob() error {
	// Shares bootoutLaunchdTarget with the gui-domain agent: which launchctl
	// exit codes mean "already gone" is empirical, macOS-version-sensitive
	// knowledge (BOS-627), and this function used to carry a second copy of it
	// that had also dropped the nil-probe fail-closed guard.
	return bootoutLaunchdTarget("system/"+WatchdogLabel, watchdogJobStillLoaded)
}

// platformUninstallUnattended removes everything platformInstallUnattended
// created, leaving no root-owned residue.
//
// Every step tolerates absence, so a partial install — a plist with no binary,
// a binary with no plist, a job booted out by hand — is cleaned up rather than
// aborting on the first missing piece. A half-removed root job is worse than an
// installed one, because nothing reports it any more.
func platformUninstallUnattended() error {
	if err := uninstallUnattendedArtifacts(); err != nil {
		return err
	}
	// BOS-1203: say what the host is left with. Since the install now
	// SUPERSEDES the per-user LaunchAgent, a successful `sudo boss daemon
	// uninstall` leaves a machine with no supervisor at all — where before this
	// change it left the agent behind and the host kept working. Finishing
	// silently would make that a surprise discovered at the next reboot.
	//
	// It is fired here rather than inside uninstallUnattendedArtifacts because
	// removeStrandedWatchdog reaches that function from `boss daemon install`,
	// which is about to bootstrap a LaunchAgent — telling that operator to run
	// `boss daemon install` would contradict the command they are already
	// running.
	noticeUnattendedSupervisionRemoved()
	return nil
}

// noticeUnattendedSupervisionRemoved is the closing line of a successful
// unattended uninstall. Package var, following the warnDaemonRefreshFailed
// idiom, so a test can observe it without capturing stderr.
var noticeUnattendedSupervisionRemoved = func() {
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: the %q supervision watchdog has been removed, and this host now has NO bossd supervisor — installing it superseded the per-user LaunchAgent. To go back to the default substrate, set 'daemon_supervision_mode' to %q and run `boss daemon install` as your own user (not with sudo)\n",
		SupervisionModeUnattended, SupervisionModeLaunchAgent)
}

// uninstallUnattendedArtifacts is platformUninstallUnattended's removal half,
// split out so removeStrandedWatchdog can reuse the teardown without the
// closing notice that only makes sense when the operator asked for it.
func uninstallUnattendedArtifacts() error {
	if err := requireUnattendedRoot(WatchdogUninstallCommand); err != nil {
		return err
	}
	layout := watchdogPaths()

	if !skipLaunchctl() {
		if err := bootoutWatchdogJob(); err != nil {
			return err
		}
	}

	if err := os.Remove(layout.PlistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove watchdog plist: %w", err)
	}
	// The whole directory, not just the binary: the directory is ours, was
	// created root-owned by the install, and leaving it behind is exactly the
	// root-owned residue an operator would then need root to clear.
	if err := os.RemoveAll(layout.BinDir); err != nil {
		return fmt.Errorf("remove watchdog binary directory: %w", err)
	}
	for _, name := range []string{"bossd-watchdog.stdout.log", "bossd-watchdog.stderr.log"} {
		if err := os.Remove(filepath.Join(layout.LogDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove watchdog log: %w", err)
		}
	}
	// Remove the log directory only when our own logs were all it held. A
	// non-empty-directory error here is the correct outcome, not a failure:
	// something else put a file there and deleting it is not this command's
	// business.
	if err := os.Remove(layout.LogDir); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("remove watchdog log directory: %w", err)
	}
	return nil
}

// platformRestartUnattended restarts the watchdog job in place.
//
// `launchctl kickstart -k` stops the running job and starts it again inside the
// existing bootstrap, which is the right verb here: the LaunchAgent path has to
// bootout-then-bootstrap because it also restages the binary and rewrites the
// plist, whereas restarting the watchdog must NOT rewrite a root-owned artifact
// — that is `boss daemon install`'s job and it is the step that needs an admin
// prompt. Bootout-then-bootstrap would also leave a window in which the job is
// unloaded, which is precisely the BOS-1181 net-loss-of-service shape.
func platformRestartUnattended() error {
	if err := requireUnattendedRoot("sudo boss daemon restart"); err != nil {
		return err
	}
	layout := watchdogPaths()
	if _, err := os.Stat(layout.PlistPath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: no watchdog plist at %s — run `%s` first", ErrUnattendedNotInstalled, layout.PlistPath, WatchdogInstallCommand)
	}
	if err := verifyWatchdogPaths(layout); err != nil {
		return err
	}
	if skipLaunchctl() {
		return nil
	}
	out, err := runLaunchctl("kickstart", "-k", "system/"+WatchdogLabel)
	if err != nil {
		return fmt.Errorf("launchctl kickstart system/%s: %w\n%s", WatchdogLabel, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// observeUnattendedInstall reports what the unattended substrate actually looks
// like on this host, for the ONE reporting decision
// describeDaemonSupervisionMode renders on both `boss daemon status` and
// `boss daemon doctor`.
//
// It is a read-only observation and is only ever called when the resolved mode
// IS unattended, so a host on the default substrate performs no extra work.
func observeUnattendedInstall() UnattendedInstall {
	layout := watchdogPaths()
	state := UnattendedInstall{
		PlistPath:  layout.PlistPath,
		BinaryPath: layout.BinaryPath,
		State:      UnattendedInstallAbsent,
	}
	if _, err := os.Stat(layout.PlistPath); err != nil {
		return state
	}
	if err := verifyWatchdogPaths(layout); err != nil {
		state.State = UnattendedInstallInsecure
		state.Err = err
		return state
	}
	state.State = UnattendedInstallPresent
	return state
}

// warnUnattendedWatchdogResidue reports a root-owned watchdog still installed
// on a host that is no longer configured for the unattended substrate. It is a
// package var so a test can observe the wording, following the
// warnDaemonRefreshFailed idiom.
//
// The wording is deliberately verb-neutral. The same warning fires from
// install, restart and uninstall, and it would be wrong on two of the three if
// it described what THIS command did — the install and restart cases are not a
// leftover orphan but a second supervisor contending for one socket.
var warnUnattendedWatchdogResidue = func(plistPath string) {
	_, _ = fmt.Fprintf(os.Stderr,
		"boss: a root-owned unattended supervision watchdog is still installed at %s while 'daemon_supervision_mode' is not %q, so two supervisors can contend for one bossd socket — run `%s` to remove the root-owned job, or set 'daemon_supervision_mode' back to %q\n",
		plistPath, SupervisionModeUnattended, WatchdogUninstallCommand, SupervisionModeUnattended)
}

// warnIfUnattendedWatchdogInstalled closes the gap that routing by the
// CONFIGURED mode leaves: an operator who sets the key back to the default
// leaves a root-owned LaunchDaemon that nothing afterwards reports, because
// status reads the configured mode and probes only the gui/<uid> domain and
// LoadSupervisionModeStatus stops observing the watchdog entirely.
//
// It is called from all three LaunchAgent paths, and the install and restart
// ones are the sharper cases: uninstall leaves one orphan, while install and
// restart bootstrap a SECOND supervisor next to a root job still loaded with
// KeepAlive=true.
//
// It warns rather than acting. Removing the watchdog needs root, and these
// commands are normally run as the user, so doing it here would either fail or
// demand a password from a command that did not ask for one — and a command
// that errors out is worse than one that finishes and tells the operator what
// is left.
// removeStrandedWatchdog tears down a root-owned watchdog left behind on a host
// no longer configured for the unattended substrate, when this process can.
//
// It exists because the warning alone named a remedy that did not work.
// platformUninstall routes teardown by the CONFIGURED mode, so on a host whose
// key has been set back to the default, `sudo boss daemon uninstall` took the
// LaunchAgent path and left the root-owned job exactly where it was — while the
// warning told the operator to run that very command. The advice was inert in
// precisely the state that produced it, which left AC8's "no residue requiring
// manual root cleanup" false unless the operator first restored a settings
// value they had deliberately changed.
//
// Removing rather than only warning is the same asymmetry platformUninstall
// already encodes: creating a substrate must fail closed, but destroying one
// must never be blocked by what a settings file currently says. A stranded root
// job with KeepAlive=true is not inert — it keeps respawning a second
// supervisor for one socket.
//
// Without root it still only warns. `boss daemon uninstall` is normally run as
// the user, and a command that errors out is worse than one that finishes and
// says what is left; the warning's remedy is now true, because under `sudo`
// this function is what runs.
func removeStrandedWatchdog(st SupervisionModeStatus) error {
	if st.Mode == SupervisionModeUnattended {
		return nil
	}
	layout := watchdogPaths()
	if _, err := os.Stat(layout.PlistPath); err != nil {
		return nil
	}
	if currentEUID() != 0 {
		warnUnattendedWatchdogResidue(layout.PlistPath)
		return nil
	}
	// The artifact removal WITHOUT the closing notice: this function also runs
	// from `boss daemon install`, which replaces the watchdog with a
	// LaunchAgent rather than leaving the host unsupervised.
	return uninstallUnattendedArtifacts()
}

func warnIfUnattendedWatchdogInstalled(st SupervisionModeStatus) {
	if st.Mode == SupervisionModeUnattended {
		return
	}
	layout := watchdogPaths()
	if _, err := os.Stat(layout.PlistPath); err != nil {
		return
	}
	warnUnattendedWatchdogResidue(layout.PlistPath)
}
