// This file contains the platform-independent half of BOS-1184's `unattended`
// supervision substrate: the root-owned watchdog LaunchDaemon's identity, the
// plist it renders, and the path-security rule that decides whether this host
// may hand that root job a binary at all.
//
// It is deliberately pure. Everything here takes its platform facts as
// arguments — the observed owner uid and mode of a path, the uid and user name
// the job will drop to — rather than reading them from the filesystem or a
// build-tagged const, exactly as ClassifyServingMode and
// resolveSupervisionMode do. That is what lets the security rule and the
// rendered argv, which are the two things worth getting right, be proven on
// EITHER platform's test run. The half that actually touches
// /Library/LaunchDaemons and launchctl lives in watchdog_darwin.go.
package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

const (
	// WatchdogLabel is the launchd label of the root-owned LaunchDaemon that
	// supervises bossd under the `unattended` supervision mode. It is
	// deliberately NOT Label: the watchdog and the LaunchAgent are different
	// jobs in different domains, and giving them one label would make
	// `launchctl print system/<label>` and `launchctl print gui/<uid>/<label>`
	// two readings of what looks like one service.
	WatchdogLabel = "com.bossanova.bossd-watchdog"

	// WatchdogInstallCommand is the exact command an operator must run to
	// install the unattended substrate. Every refusal that stems from not being
	// root names this string rather than paraphrasing it, so an operator can
	// paste what they were told.
	WatchdogInstallCommand = "sudo boss daemon install"

	// WatchdogUninstallCommand is its counterpart.
	WatchdogUninstallCommand = "sudo boss daemon uninstall"

	// The absolute tool paths the watchdog execs. They are absolute and
	// hard-coded because a root LaunchDaemon resolving a tool through PATH is
	// a root-execution primitive for whoever controls that PATH.
	watchdogLaunchctlPath = "/bin/launchctl"
	watchdogSudoPath      = "/usr/bin/sudo"
	watchdogEnvPath       = "/usr/bin/env"

	// The layout, relative to the filesystem root so tests can rebuild the
	// whole tree inside a temp directory (watchdogFilesystemRoot).
	//
	// The binary lives under /usr/local/libexec rather than at the LaunchAgent's
	// staged path, and that is the single most important decision in this file.
	// EnsureStaged puts bossd under the USER's ~/Library/Application Support,
	// which is user-writable by construction; a root LaunchDaemon pointed at a
	// user-writable path hands every user who can write it root execution. The
	// unattended mode therefore keeps its own root-owned copy and never reuses
	// the staged one.
	watchdogPlistDirRel = "Library/LaunchDaemons"
	watchdogBinDirRel   = "usr/local/libexec/bossanova"
	watchdogLogDirRel   = "var/log/bossanova"
)

// watchdogPlistTemplate renders the root LaunchDaemon.
//
// RunAtLoad and KeepAlive are both present and both load-bearing, and they map
// one-to-one onto the two unattended-recovery paths BOS-1184 exists to fix:
// RunAtLoad is the reboot case, KeepAlive is the crash case. The `system`
// domain has no foreground-console concept, so neither depends on which user
// owns /dev/console — which is the whole reason this substrate exists.
//
// KeepAlive respawning bossd rather than spinning rests on `launchctl asuser`
// WAITING for the command it runs. That is verified, not assumed: `man
// launchctl` describes asuser as executing the given command in the target
// user's bootstrap context, and BOS-1184's U1 transcript shows
// `sudo launchctl asuser 501 /usr/bin/security list-keychains` returning the
// command's own stdout and `exit=0` — a wrapper that did not wait could
// propagate neither. So the watchdog process lives exactly as long as the bossd
// it spawned, and launchd's KeepAlive restarts the chain when bossd dies. No
// supervising loop of our own is needed.
//
// The log paths are under a root-owned directory rather than the user's
// ~/Library/Logs: launchd creates these files as root before the job starts,
// and pointing a root job's redirect at a user-writable directory is the same
// class of hole as pointing it at a user-writable binary.
const watchdogPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .ProgramArguments}}
		<string>{{.}}</string>
{{- end}}
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogDir}}/bossd-watchdog.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogDir}}/bossd-watchdog.stderr.log</string>
	<key>ExitTimeOut</key>
	<integer>90</integer>
</dict>
</plist>
`

var (
	// ErrUnattendedInsecurePath is the refusal that keeps the unattended mode
	// from becoming a privilege-escalation surface: a root-owned LaunchDaemon
	// must never exec a path any non-root user can write, and neither may any
	// directory on the way to it be one. It is returned by
	// classifyWatchdogPathSecurity and names the offending path and the reason.
	//
	// This is enforced in code rather than documented as a requirement because
	// the requirement is invisible from the artifact: a plist that looks
	// perfect still hands out root if /usr/local is group-writable, which it is
	// on any Intel Mac where Homebrew took ownership of it.
	ErrUnattendedInsecurePath = errors.New("unattended supervision path is not safe for a root-owned job")

	// ErrUnattendedRequiresRoot is the refusal for an install, uninstall or
	// restart of the unattended substrate attempted without root. It exists so
	// the failure is a named, actionable one — `boss daemon install` is
	// normally run as the user — rather than a permission-denied write halfway
	// through a partial install.
	ErrUnattendedRequiresRoot = errors.New("the unattended supervision mode must be installed as root")

	// ErrUnattendedNotInstalled is returned when an operation needs the
	// watchdog job to already exist and it does not.
	ErrUnattendedNotInstalled = errors.New("the unattended supervision watchdog is not installed")

	// ErrWatchdogRecoveryFailed is the refusal at the end of the ROOT arm of
	// platformEnsureRunning's watchdog route: the socket is still not served
	// after kickstarting the watchdog, and this process is root.
	//
	// It is an error rather than a fall-through to the detached spawn because
	// that spawn under `sudo` would produce a ROOT-owned bossd holding the
	// user's socket, app-data directory and singleton lock — a state the user's
	// own daemon can never reclaim without manual cleanup. "Never leave the
	// host with no daemon" is a claim about the USER's daemon, and a root-owned
	// one is not that daemon (R4).
	ErrWatchdogRecoveryFailed = errors.New("the unattended supervision watchdog did not bring the daemon back up")
)

// watchdogLayout is where every artifact of the unattended mode lives.
type watchdogLayout struct {
	Root       string
	PlistPath  string
	BinDir     string
	BinaryPath string
	LogDir     string
}

// newWatchdogLayout builds the layout under an explicit root, taking the root
// as an argument so the paths are derivable without touching a global.
func newWatchdogLayout(root string) watchdogLayout {
	if root == "" {
		root = "/"
	}
	root = filepath.Clean(root)
	binDir := filepath.Join(root, watchdogBinDirRel)
	return watchdogLayout{
		Root:       root,
		PlistPath:  filepath.Join(root, watchdogPlistDirRel, WatchdogLabel+".plist"),
		BinDir:     binDir,
		BinaryPath: filepath.Join(binDir, "bossd"),
		LogDir:     filepath.Join(root, watchdogLogDirRel),
	}
}

// watchdogSpec is everything the rendered plist needs, gathered by the caller.
type watchdogSpec struct {
	// UID and User are the account bossd runs as. Both are carried because the
	// argv needs both: asuser targets a uid, sudo targets a name.
	UID  int
	User string
	// Home is that user's home directory, pinned explicitly — see
	// watchdogProgramArguments.
	Home string
	// BossdPath is the ROOT-OWNED copy, never the per-user staged one.
	BossdPath string
	// Path is the PATH the daemon runs with, the same serviceEnvPath() the
	// LaunchAgent renders.
	Path   string
	LogDir string
}

// watchdogProgramArguments is the exact argv the root LaunchDaemon execs.
//
//	/bin/launchctl asuser <uid> /usr/bin/sudo -u <user> /usr/bin/env HOME=… PATH=… LC_CTYPE=UTF-8 <bossd>
//
// Three segments, each of which is doing something the others cannot:
//
//   - `launchctl asuser <uid>` moves the process into the target user's Mach
//     bootstrap and security audit session. That is what gives bossd the login
//     keychain, and BOS-1184's U1 verified it reaches a BACKGROUNDED user's
//     session (macOS 26.4.1) — the exact case a gui/<uid> LaunchAgent cannot
//     spawn into.
//
//   - `sudo -u <user>` drops the credentials. `man launchctl` is explicit that
//     asuser "does not modify the process' credentials (UID, GID, etc.)", so
//     without this segment bossd would run as ROOT wearing the user's session.
//     That is a privilege escalation and a behaviour change, not a shortcut.
//     Root's sudo needs no authentication, so this neither prompts nor needs a
//     tty. UNVERIFIED, and recorded as such in the runbook: whether the login
//     keychain survives this uid drop. U1 proved asuser reaches the keychain as
//     ROOT-with-user-context; it did not test the dropped shape.
//
//   - `/usr/bin/env HOME=… PATH=… LC_CTYPE=UTF-8` pins the environment. The
//     same man page sentence says asuser does not "adopt any user-specific
//     environment variables", and sudo's own env_reset behaviour for HOME
//     varies with sudoers configuration — so neither HOME nor PATH can be
//     assumed to arrive correctly. Leaving PATH to chance is how BOS-880
//     happened (launchd never sources a shell config, so node/claude were
//     invisible to bossd); leaving HOME to chance is worse, because a bossd
//     that resolves HOME to /var/root reads and writes an entirely different
//     profile. U1's own transcript had to say `/usr/bin/env HOME=/Users/dave`
//     for exactly this reason. A plist EnvironmentVariables dict would not do:
//     it applies to launchctl, and sudo resets what it hands on.
func watchdogProgramArguments(spec watchdogSpec) []string {
	return []string{
		watchdogLaunchctlPath,
		"asuser",
		strconv.Itoa(spec.UID),
		watchdogSudoPath,
		"-u",
		spec.User,
		watchdogEnvPath,
		"HOME=" + spec.Home,
		"PATH=" + spec.Path,
		"LC_CTYPE=UTF-8",
		spec.BossdPath,
	}
}

// plistHostileChars are the characters an interpolated value may not contain
// before it reaches the plist template. text/template performs NO escaping —
// the same hazard servicePathHostileChars exists for on the PATH — and the
// user name here comes from the environment (SUDO_USER), so it is genuinely
// untrusted input rather than a derived constant.
//
// `:` is deliberately absent: PATH is interpolated whole and is built of
// colon-separated entries that joinServicePath has already filtered.
const plistHostileChars = "<>&\"'\n\r\x00"

// validateWatchdogSpec rejects a spec whose interpolated values would corrupt
// the plist, before anything is written.
func validateWatchdogSpec(spec watchdogSpec) error {
	if spec.UID <= 0 {
		return fmt.Errorf("watchdog target uid %d is not a real user; refusing to render a job that would run bossd as root", spec.UID)
	}
	for _, field := range []struct{ name, value string }{
		{"user", spec.User},
		{"home", spec.Home},
		{"bossd path", spec.BossdPath},
		{"PATH", spec.Path},
		{"log dir", spec.LogDir},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("watchdog %s is empty", field.name)
		}
		if strings.ContainsAny(field.value, plistHostileChars) {
			return fmt.Errorf("watchdog %s %q contains characters that cannot be interpolated into a plist", field.name, field.value)
		}
	}
	return nil
}

type watchdogPlistData struct {
	Label            string
	ProgramArguments []string
	LogDir           string
}

// renderWatchdogPlist renders the root LaunchDaemon plist for a spec.
func renderWatchdogPlist(spec watchdogSpec) (string, error) {
	if err := validateWatchdogSpec(spec); err != nil {
		return "", err
	}
	tmpl, err := template.New("watchdogPlist").Parse(watchdogPlistTemplate)
	if err != nil {
		return "", fmt.Errorf("parse watchdog plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, watchdogPlistData{
		Label:            WatchdogLabel,
		ProgramArguments: watchdogProgramArguments(spec),
		LogDir:           spec.LogDir,
	}); err != nil {
		return "", fmt.Errorf("render watchdog plist: %w", err)
	}
	return buf.String(), nil
}

// pathSecurityFact is one observed filesystem path, reduced to the facts the
// security rule needs. It is a plain struct with no methods so a test can state
// a hostile filesystem — a group-writable /usr/local, a symlinked parent — that
// would otherwise need root to create.
type pathSecurityFact struct {
	// Path is the absolute path observed.
	Path string
	// Role names what this path is FOR in an error message: "the watchdog
	// plist", "a parent directory of the watchdog bossd binary".
	Role string
	// Missing is true when the path does not exist.
	Missing bool
	// StatErr is a non-ENOENT failure to observe the path at all.
	StatErr error
	// OwnerUID is the uid owning the path.
	OwnerUID uint32
	// Mode is the permission bits plus the type/sticky bits, as os.Lstat
	// reports them.
	Mode fs.FileMode
}

// classifyWatchdogPathSecurity is the whole security posture of the unattended
// mode, as a pure function of observed facts.
//
// trustedOwnerUID is passed rather than hard-coded as 0 for the same reason
// resolveSupervisionMode takes `configurable`: it makes the rule provable. A
// test running as an ordinary user can build a real directory tree in a temp
// dir, declare its own uid the trusted one, and then prove that a
// group-writable member of that tree is refused — which is the property that
// matters. Production passes watchdogRootOwnerUID.
//
// The rule, in order, first offender wins:
//
//  1. A path that could not be observed is refused. "I could not check" is
//     never "it is fine".
//  2. A missing path is refused. A root job's target that does not exist yet,
//     in a directory somebody else may be able to write, is the classic
//     create-it-first race.
//  3. A symlink anywhere in the chain is refused. Its own mode bits are
//     ignored by the kernel, so a symlink is a repointable indirection the
//     mode check cannot see through.
//  4. A path not owned by the trusted uid is refused. Ownership is write
//     access on Unix regardless of the mode bits, because the owner can chmod.
//  5. A group- or world-writable path is refused — with ONE exemption, below.
//
// The exemption: a DIRECTORY carrying the sticky bit may be group- or
// world-writable. That is not a loophole, it is the actual semantics — sticky
// means a non-owner cannot rename or delete entries they do not own, so a
// root-owned entry inside one cannot be swapped out. Without this exemption the
// mode would refuse on macOS releases that ship /Library as drwxrwxr-t, which
// is a false refusal rather than a caught hole. It applies to directories only:
// there is no such protection for a file.
func classifyWatchdogPathSecurity(facts []pathSecurityFact, trustedOwnerUID uint32) error {
	for _, fact := range facts {
		switch {
		case fact.StatErr != nil:
			return fmt.Errorf("%w: %s %s could not be inspected (%v), so its ownership cannot be verified",
				ErrUnattendedInsecurePath, fact.Role, fact.Path, fact.StatErr)
		case fact.Missing:
			return fmt.Errorf("%w: %s %s does not exist, so its ownership cannot be verified",
				ErrUnattendedInsecurePath, fact.Role, fact.Path)
		case fact.Mode&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s %s is a symlink, which can be repointed at another target without changing any mode bits",
				ErrUnattendedInsecurePath, fact.Role, fact.Path)
		case fact.OwnerUID != trustedOwnerUID:
			return fmt.Errorf("%w: %s %s is owned by uid %d, not uid %d — its owner can rewrite it, so a root-owned job must not depend on it",
				ErrUnattendedInsecurePath, fact.Role, fact.Path, fact.OwnerUID, trustedOwnerUID)
		case fact.Mode&0o022 != 0 && (!fact.Mode.IsDir() || fact.Mode&fs.ModeSticky == 0):
			return fmt.Errorf("%w: %s %s has mode %04o, which is writable by group or other — any such user could obtain root execution through it",
				ErrUnattendedInsecurePath, fact.Role, fact.Path, fact.Mode.Perm())
		}
	}
	return nil
}

// watchdogAncestorDirs lists the directories from path's parent up to and
// including root, nearest first.
//
// It stops at root rather than at "/" so a test rooted in a temp directory does
// not walk out into /var/folders, whose ownership has nothing to do with the
// property under test. In production root IS "/", so the real chain — for
// example /Library/LaunchDaemons, /Library, / — is walked in full.
func watchdogAncestorDirs(path, root string) []string {
	root = filepath.Clean(root)
	var dirs []string
	for dir := filepath.Dir(filepath.Clean(path)); ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == root || dir == filepath.Dir(dir) {
			return dirs
		}
	}
}

// watchdogSecurityTarget is one path the security rule is applied to.
//
// IsArtifact is a FIELD rather than something a consumer re-derives, and that
// is the point of the type existing. The pre-write passes run the rule over
// directories only, and they used to select them by comparing each target's
// path against layout.PlistPath and layout.BinaryPath — which made the
// constructor and its consumers agree only by string coincidence, and silently
// handed any newly added leaf whatever treatment the comparison happened to
// give it. Adding the log directory made that live: it is a leaf of the layout
// but a DIRECTORY, and only the constructor can say so.
type watchdogSecurityTarget struct {
	// Path is the absolute path to inspect.
	Path string
	// Role names what the path is FOR in a refusal message.
	Role string
	// IsArtifact marks the two FILES the installer writes — the plist and the
	// bossd copy. They cannot be checked before they exist, so the pre-write
	// passes skip them and only verifyWatchdogPaths covers them.
	IsArtifact bool
}

// watchdogSecurityTargets lists every path the security rule must cover: the
// two artifacts a root job depends on, the directory its logs are redirected
// into, and every directory between the two artifacts and the root. Ordering is
// deliberate — the leaves come first so the most specific offender is the one
// named.
//
// The LOG DIRECTORY is covered because launchd opens StandardOutPath and
// StandardErrorPath as root with O_CREAT|O_APPEND and follows symlinks. A log
// directory a non-root user can write is therefore a root-append primitive of
// exactly the class this rule exists to refuse: that user could plant a symlink
// at either log file, or swap the directory itself, and have root's output
// appended to a file of their choosing.
//
// It is covered WITHOUT its ancestor walk, and that asymmetry with the plist
// and binary chains is deliberate rather than an oversight. On macOS /var is a
// SYMLINK to private/var, so walking /var/log/bossanova's ancestors would hand
// rule 3 a symlink on every healthy host and refuse every install. Checking the
// directory itself loses nothing that matters: every mutation of the log
// directory — replacing it with a symlink, replacing it with a directory
// somebody else owns, loosening its mode — changes what Lstat reports AT THAT
// PATH, so rules 3, 4 and 5 still see it. What the narrower scope gives up is
// only the ability to notice a writable /var/log that has not yet been used,
// and the artifacts a writable /var/log could reach are already covered here
// one level down.
func watchdogSecurityTargets(layout watchdogLayout) []watchdogSecurityTarget {
	targets := []watchdogSecurityTarget{
		{Path: layout.PlistPath, Role: "the watchdog plist", IsArtifact: true},
		{Path: layout.BinaryPath, Role: "the watchdog bossd binary", IsArtifact: true},
		{Path: layout.LogDir, Role: "the watchdog log directory"},
	}
	seen := map[string]bool{layout.PlistPath: true, layout.BinaryPath: true, layout.LogDir: true}
	for _, leaf := range []struct{ path, role string }{
		{layout.PlistPath, "a parent directory of the watchdog plist"},
		{layout.BinaryPath, "a parent directory of the watchdog bossd binary"},
	} {
		for _, dir := range watchdogAncestorDirs(leaf.path, layout.Root) {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			targets = append(targets, watchdogSecurityTarget{Path: dir, Role: leaf.role})
		}
	}
	return targets
}

// UnattendedInstallState is what the unattended substrate looks like ON THIS
// HOST right now, as opposed to what settings.json asked for.
//
// It is an OBSERVATION and belongs to the same family as ServingMode and
// SpawnState, not to SupervisionMode: a host can have `unattended` configured
// and nothing installed, which is exactly the state an operator who edited
// settings.json but has not yet run the root install is in, and exactly the
// state no surface reported before it existed.
type UnattendedInstallState int

const (
	// UnattendedInstallNotApplicable is the zero value: this host is not on the
	// unattended substrate, so no observation was made. It is the zero value on
	// purpose — a status struct built without an observation must never read as
	// an affirmative claim about one.
	UnattendedInstallNotApplicable UnattendedInstallState = iota
	// UnattendedInstallAbsent means the mode is selected and the watchdog is
	// NOT installed. There is no supervision at all on such a host.
	UnattendedInstallAbsent
	// UnattendedInstallPresent means the watchdog plist exists and every path
	// it depends on passed the security rule.
	UnattendedInstallPresent
	// UnattendedInstallInsecure means the watchdog is installed and one of the
	// paths it depends on is NOT safe for a root-owned job. It is a fault, and
	// a sharper one than "not installed": something on this host is already a
	// root-execution primitive.
	UnattendedInstallInsecure
)

// UnattendedInstall carries that observation plus the paths a report needs to
// name, so a reporting surface never re-derives the layout.
type UnattendedInstall struct {
	State      UnattendedInstallState
	PlistPath  string
	BinaryPath string
	// Err is the ErrUnattendedInsecurePath reason when State is
	// UnattendedInstallInsecure, and nil otherwise. It is carried rather than
	// re-derived for the same reason SupervisionModeStatus.SettingsErr is: the
	// surface that must name the offending path is not the one that found it.
	Err error
}

// ensureRunningRoute is the recovery substrate platformEnsureRunning must act
// on once it has found the socket down.
//
// It exists as a named verdict rather than as nested conditionals inside
// platformEnsureRunning for the reason ClassifyServingMode and
// resolveSupervisionMode exist: the interesting half of BOS-1203 is a small
// matrix over facts, and expressing it as a pure function is what lets EVERY
// row of that matrix — including the rows that only occur on a misconfigured
// macOS host — be proven from either platform's test run.
type ensureRunningRoute int

const (
	// ensureRunningRouteLaunchAgent is today's path, unchanged: refresh the
	// staged copy, `launchctl load` the gui/<uid> LaunchAgent, and fall back to
	// the unsupervised detached spawn. It is the zero value because a caller
	// that failed to gather any facts must be routed to the behaviour that
	// predates this classifier, never to one that touches a root-owned job.
	ensureRunningRouteLaunchAgent ensureRunningRoute = iota

	// ensureRunningRouteWatchdog means a watchdog plist is on disk, so the
	// LaunchAgent must not be loaded (R1) and the recovery path belongs to the
	// watchdog. Waiting for that watchdog to serve the socket is worthwhile.
	ensureRunningRouteWatchdog

	// ensureRunningRouteWatchdogNoWait is the same routing decision with the
	// wait suppressed (KTD5). It is reached when the watchdog is installed and
	// one of the paths it depends on is NOT safe for a root-owned job:
	// verifyWatchdogPaths failing is a durable HOST fault — a group-writable
	// /usr/local does not repair itself between two boss commands — so the
	// watchdog is known to be unable to serve and waiting for it only spends
	// the budget before reaching the same fallback.
	ensureRunningRouteWatchdogNoWait
)

func (r ensureRunningRoute) String() string {
	switch r {
	case ensureRunningRouteLaunchAgent:
		return "launch-agent"
	case ensureRunningRouteWatchdog:
		return "watchdog"
	case ensureRunningRouteWatchdogNoWait:
		return "watchdog (wait suppressed)"
	default:
		return "unknown(" + strconv.Itoa(int(r)) + ")"
	}
}

// ensureRunningFacts is everything the routing rule is allowed to look at,
// gathered by the caller so the rule itself touches neither the filesystem nor
// a build-tagged const.
//
// Mode, ModeErr and SettingsErr are carried even though the rule deliberately
// does NOT branch on them, and that is the point rather than an oversight. KTD1
// is the claim that the configured mode must not decide this — and a claim that
// something is ignored is only provable if the thing is present to be ignored.
// With these fields on the struct, the matrix can state "unrecognised value",
// "unparseable settings.json" and "key reverted to launch-agent" as rows and
// pin that the verdict does not move; without them those three states would be
// untestable at this level and the classifier would be indistinguishable from
// the mode-only branch KTD1 rejects.
type ensureRunningFacts struct {
	// Mode is the resolved supervision substrate from
	// SupervisionModeStatus.Mode. Empty when the configured value was refused.
	Mode SupervisionMode
	// ModeErr is SupervisionModeStatus.Err — the fail-closed rejection of a
	// value that is not a supervision mode at all. The likeliest way to produce
	// it is a typo OF "unattended", on precisely the host that has a watchdog.
	ModeErr error
	// SettingsErr is SupervisionModeStatus.SettingsErr — settings.json exists
	// and could not be read or parsed, so Mode carries the DEFAULT while the
	// root job may still be loaded.
	SettingsErr error
	// InstallState is SupervisionModeStatus.Unattended.State. It is
	// UnattendedInstallNotApplicable whenever the resolved mode is not
	// unattended, because no observation is made on those hosts.
	InstallState UnattendedInstallState
	// WatchdogPlistPresent is a stat of watchdogPaths().PlistPath. It is the
	// fact the rule actually turns on.
	WatchdogPlistPresent bool
}

// classifyEnsureRunningRoute decides which substrate platformEnsureRunning
// recovers through, from facts the caller has already gathered.
//
// The discriminating fact is the ARTIFACT, not the resolved mode (KTD1). That
// follows the precedent warnIfUnattendedWatchdogInstalled already sets in this
// package: the states that most need catching are exactly the ones where the
// configured mode has stopped describing the machine, and a mode-only branch
// misses all three of them —
//
//   - a key reverted to "launch-agent" with the root job still loaded, where
//     Mode is the default and InstallState was never observed;
//   - a typo OF "unattended", where ModeErr is set and Mode is empty;
//   - an unparseable settings.json, where SettingsErr is set and Mode silently
//     carries the default.
//
// Every one of those produces the same two-supervisor state, and a single
// os.Stat covers all three.
func classifyEnsureRunningRoute(facts ensureRunningFacts) ensureRunningRoute {
	// No watchdog plist means nothing to contend with, so the LaunchAgent path
	// is byte-identical to what it was before this classifier existed (R5).
	// This arm is also where a host that SELECTED unattended but has not yet
	// run the root install lands (KTD7): it has no watchdog, and skipping the
	// load there would strip supervision from a host that currently has it.
	if !facts.WatchdogPlistPresent {
		return ensureRunningRouteLaunchAgent
	}
	switch facts.InstallState {
	case UnattendedInstallInsecure:
		return ensureRunningRouteWatchdogNoWait
	case UnattendedInstallNotApplicable, UnattendedInstallAbsent, UnattendedInstallPresent:
		// NotApplicable is the reverted/typo'd/unreadable-settings host: no
		// observation was made, but a root job's plist is on disk.
		// Absent means observeUnattendedInstall saw no plist while this
		// caller's own stat did — the two reads are not atomic, and the newer
		// one wins, because the hazard is a plist that EXISTS.
		// Present is the ordinary unattended host.
		return ensureRunningRouteWatchdog
	default:
		// A future UnattendedInstallState must not fall through to the
		// LaunchAgent load. Routing an unknown observation to the watchdog is
		// the direction that cannot create a second supervisor, so the unknown
		// state costs at most a bounded wait.
		return ensureRunningRouteWatchdog
	}
}

// WatchdogOwnershipState is what launchd's `system` domain says about the
// watchdog job RIGHT NOW, as opposed to what is installed on disk.
//
// It is the third observation in the family UnattendedInstallState opened, and
// the two answer genuinely different questions: UnattendedInstallState reads
// the FILESYSTEM (is a plist there, and is every path it depends on safe for a
// root job), while this reads the SERVICE MANAGER (has launchd got that job
// loaded). A host can have a perfectly secure watchdog plist on disk that
// nobody ever bootstrapped, and on such a host nothing is supervising bossd —
// which is precisely the state a reporting surface must not certify as healthy.
//
// BOS-1204: it exists because `boss daemon status` and `boss daemon doctor`
// could only ever see the per-user LaunchAgent, so a correctly supervised
// unattended host was reported uninstalled and unsupervised.
type WatchdogOwnershipState int

const (
	// WatchdogOwnershipNotObserved is the zero value: no probe was made,
	// because this host is not on the unattended substrate or the substrate is
	// not installed. It is the zero value on purpose — a report built without
	// an observation must never read as an affirmative claim about one, the
	// same discipline UnattendedInstallNotApplicable encodes.
	WatchdogOwnershipNotObserved WatchdogOwnershipState = iota
	// WatchdogOwnershipUnknown means the probe was made and could not be read.
	// It is the fail-closed verdict: a launchctl that could not be executed, or
	// that exited for a reason other than "no such service", lands here and
	// NEVER on WatchdogOwnershipLoaded.
	WatchdogOwnershipUnknown
	// WatchdogOwnershipNotLoaded means launchd positively reported that no such
	// service exists in the `system` domain. The watchdog is not supervising
	// anything.
	WatchdogOwnershipNotLoaded
	// WatchdogOwnershipLoaded means launchd has the watchdog job registered in
	// the `system` domain.
	//
	// Note what this does NOT claim: that bossd is healthy. It claims OWNERSHIP
	// — launchd owns the watchdog and the watchdog owns bossd. Liveness stays
	// the recorded-PID probe the reporting surfaces already make.
	WatchdogOwnershipLoaded
)

// WatchdogOwnership carries that observation plus the target it was read from,
// so a reporting surface can name what it asked about rather than re-deriving
// it — the same reason SpawnHistory carries Target.
type WatchdogOwnership struct {
	State WatchdogOwnershipState
	// Target is the service-manager target probed, e.g.
	// "system/com.bossanova.bossd-watchdog". Always set when a probe was made.
	Target string
	// Reason explains a non-Loaded state in human-readable terms. Empty for
	// WatchdogOwnershipLoaded and for the unprobed zero value.
	Reason string
}

// WatchdogDomain is the launchd domain the root-owned watchdog job lives in.
//
// BOS-1222: it is a named constant so every derivation of the watchdog's
// domain has a COMPILE-TIME link to one another. jobDisabledDomain used to
// return the literal "system" while WatchdogTarget derived its own, and this
// codebase has already relocated supervision between launchd domains once
// (BOS-1204) — a second move would have carried WatchdogTarget with it and
// left the disable-override probe confidently answering about a domain that
// no longer held the job.
const WatchdogDomain = "system"

// WatchdogTarget is the launchd service target of the root-owned watchdog job.
//
// It is exported and lives here rather than being spelled out at each call
// site: the ownership probe, the spawn-history target resolver and the
// bootout path all name the same target, and three string concatenations of
// WatchdogDomain + "/" + WatchdogLabel are three places for a future domain
// change to be missed in two of.
func WatchdogTarget() string { return WatchdogDomain + "/" + WatchdogLabel }

// watchdogProbeOutcome is what one `launchctl print <watchdog target>` did,
// reduced to the facts the verdict turns on.
//
// It exists so classifyWatchdogOwnership can be pure and its whole matrix
// provable from EITHER platform's test run, exactly as classifyEnsureRunningRoute
// and classifyWatchdogPathSecurity above are: the launchctl invocation and the
// exit-code knowledge are darwin-only, the fail-closed rule is not.
type watchdogProbeOutcome int

const (
	// watchdogProbeDisabled is the BOSS_DAEMON_SKIP_LAUNCHCTL short-circuit. It
	// is the zero value so a caller that gathered nothing cannot accidentally
	// produce an affirmative ownership claim.
	watchdogProbeDisabled watchdogProbeOutcome = iota
	// watchdogProbeUnexecutable means launchctl could not be run at all.
	watchdogProbeUnexecutable
	// watchdogProbeNoSuchService means launchctl ran and exited with a code
	// that POSITIVELY means the target is not registered
	// (launchctlExitSaysAlreadyGone).
	watchdogProbeNoSuchService
	// watchdogProbeExitedNonZero means launchctl ran and failed for some other
	// reason — a refused read, or an exit code this build does not recognise.
	watchdogProbeExitedNonZero
	// watchdogProbeLoaded means launchctl ran and exited zero.
	watchdogProbeLoaded
)

// classifyWatchdogOwnership maps a probe outcome onto the reported observation.
//
// The fail-closed direction is the whole content of this function: only
// watchdogProbeLoaded produces WatchdogOwnershipLoaded, and only an exit code
// that positively means "no such service" produces WatchdogOwnershipNotLoaded.
// Everything else — including an outcome a future build adds — is unknown with
// a reason, never a fault and never health. Reporting a fault from an
// unreadable probe would invent a second false alarm in the surface BOS-1204
// exists to stop making one.
func classifyWatchdogOwnership(outcome watchdogProbeOutcome, target, detail string) WatchdogOwnership {
	observation := WatchdogOwnership{Target: target}
	switch outcome {
	case watchdogProbeLoaded:
		observation.State = WatchdogOwnershipLoaded
	case watchdogProbeNoSuchService:
		observation.State = WatchdogOwnershipNotLoaded
		observation.Reason = fmt.Sprintf("launchctl reports no such service as %s (%s)", target, detail)
	case watchdogProbeDisabled:
		observation.State = WatchdogOwnershipUnknown
		observation.Reason = "service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL"
	case watchdogProbeUnexecutable:
		observation.State = WatchdogOwnershipUnknown
		observation.Reason = fmt.Sprintf("could not run launchctl print %s: %s", target, detail)
	case watchdogProbeExitedNonZero:
		observation.State = WatchdogOwnershipUnknown
		observation.Reason = fmt.Sprintf("launchctl print %s failed: %s", target, detail)
	default:
		observation.State = WatchdogOwnershipUnknown
		observation.Reason = fmt.Sprintf("unrecognised watchdog probe outcome %d", int(outcome))
	}
	return observation
}

// ObserveWatchdogOwnership reports whether launchd currently has the
// root-owned watchdog job loaded in the `system` domain.
//
// It is the exported form of the platform probe, in the shape
// GetSpawnHistory / platformSpawnHistory already use. Callers must gather it
// LAZILY — only on a host whose reported verdict actually depends on it — so
// the default LaunchAgent substrate performs no extra launchctl invocation at
// all (BOS-1204 R5).
func ObserveWatchdogOwnership() WatchdogOwnership { return observeWatchdogOwnership() }
