package daemon

import (
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// watchdogTestSpec is the fixed spec every rendering assertion below uses. It
// is fully explicit — no HOME, no settings, no uid of the running process — so
// the expected artifact is a constant rather than something the test has to
// re-derive from the code under test.
func watchdogTestSpec() watchdogSpec {
	return watchdogSpec{
		UID:       501,
		User:      "dave",
		Home:      "/Users/dave",
		BossdPath: "/usr/local/libexec/bossanova/bossd",
		Path:      "/usr/local/bin:/usr/bin:/bin",
		LogDir:    "/var/log/bossanova",
	}
}

// TestWatchdogProgramArgumentsPinTheUidDrop is the single most important
// assertion about the artifact: the exact argv, in order.
//
// Every element is load-bearing and a plausible-looking edit to any of them
// breaks the mode in a way no other test would catch:
//
//   - dropping `sudo -u dave` leaves bossd running as ROOT, because `man
//     launchctl` says asuser "does not modify the process' credentials". That
//     is a privilege escalation that would still start, still serve, and still
//     pass every other test here.
//   - dropping `/usr/bin/env HOME=… PATH=…` leaves bossd with whatever
//     environment sudo's env_reset decides to hand it — a HOME of /var/root
//     means a different profile entirely, and a stripped PATH is BOS-880.
//   - a relative `launchctl` or `sudo` would be resolved through the root job's
//     PATH, which is a root-execution primitive for whoever controls it.
func TestWatchdogProgramArgumentsPinTheUidDrop(t *testing.T) {
	got := watchdogProgramArguments(watchdogTestSpec())
	want := []string{
		"/bin/launchctl",
		"asuser",
		"501",
		"/usr/bin/sudo",
		"-u",
		"dave",
		"/usr/bin/env",
		"HOME=/Users/dave",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LC_CTYPE=UTF-8",
		"/usr/local/libexec/bossanova/bossd",
	}
	if len(got) != len(want) {
		t.Fatalf("ProgramArguments = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ProgramArguments[%d] = %q, want %q (full argv: %q)", i, got[i], want[i], got)
		}
	}
}

// TestRenderWatchdogPlistIsTheExpectedArtifact pins the whole rendered plist
// byte for byte.
//
// A structural check ("contains RunAtLoad") would pass for a plist whose
// KeepAlive had been dropped, whose label had drifted, or whose log paths had
// moved back under the user's home. The plist IS the deliverable of this unit,
// so it is compared in full — and the spec is a constant, so there is nothing
// host-derived to make the comparison flaky.
func TestRenderWatchdogPlistIsTheExpectedArtifact(t *testing.T) {
	got, err := renderWatchdogPlist(watchdogTestSpec())
	if err != nil {
		t.Fatalf("renderWatchdogPlist: %v", err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.bossanova.bossd-watchdog</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/launchctl</string>
		<string>asuser</string>
		<string>501</string>
		<string>/usr/bin/sudo</string>
		<string>-u</string>
		<string>dave</string>
		<string>/usr/bin/env</string>
		<string>HOME=/Users/dave</string>
		<string>PATH=/usr/local/bin:/usr/bin:/bin</string>
		<string>LC_CTYPE=UTF-8</string>
		<string>/usr/local/libexec/bossanova/bossd</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/bossanova/bossd-watchdog.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/bossanova/bossd-watchdog.stderr.log</string>
	<key>ExitTimeOut</key>
	<integer>90</integer>
</dict>
</plist>
`
	if got != want {
		t.Fatalf("rendered watchdog plist differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderWatchdogPlistIsWellFormedXML guards the one failure the byte
// comparison above would happily accept if both sides were edited together: a
// template change that produces text launchd cannot parse.
func TestRenderWatchdogPlistIsWellFormedXML(t *testing.T) {
	plist, err := renderWatchdogPlist(watchdogTestSpec())
	if err != nil {
		t.Fatalf("renderWatchdogPlist: %v", err)
	}
	decoder := xml.NewDecoder(strings.NewReader(plist))
	for {
		_, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("rendered watchdog plist is not well-formed XML: %v\n%s", err, plist)
		}
	}
}

// TestRenderWatchdogPlistRejectsUninterpolatableValues covers the injection
// surface. text/template performs no escaping, and the user name arrives from
// the environment (SUDO_USER), so it is genuinely untrusted rather than a
// derived constant. A uid of 0 is refused for a different reason: it is not an
// injection but a job that would run bossd as root.
func TestRenderWatchdogPlistRejectsUninterpolatableValues(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*watchdogSpec)
		wantMsg string
	}{
		{"root uid", func(s *watchdogSpec) { s.UID = 0 }, "root"},
		{"negative uid", func(s *watchdogSpec) { s.UID = -1 }, "root"},
		{"user closes the string element", func(s *watchdogSpec) { s.User = "dave</string><string>evil" }, "user"},
		{"home carries a newline", func(s *watchdogSpec) { s.Home = "/Users/dave\n<key>Program</key>" }, "home"},
		{"bossd path carries an ampersand", func(s *watchdogSpec) { s.BossdPath = "/usr/local/bin/bossd&x" }, "bossd path"},
		{"empty user", func(s *watchdogSpec) { s.User = "" }, "user"},
		{"empty home", func(s *watchdogSpec) { s.Home = "  " }, "home"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := watchdogTestSpec()
			tt.mutate(&spec)
			out, err := renderWatchdogPlist(spec)
			if err == nil {
				t.Fatalf("renderWatchdogPlist accepted a hostile spec and produced:\n%s", out)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("rejection %q does not name %q", err, tt.wantMsg)
			}
		})
	}
}

// TestClassifyWatchdogPathSecurity is the security rule's whole matrix, and the
// most important test in BOS-1184 U3.
//
// The mode ships a root-owned LaunchDaemon. Every row below that expects a
// refusal is a row where letting the install proceed would hand root execution
// to whichever non-root user owns or can write the named path — so a green run
// of this table is the evidence that the posture is ENFORCED rather than
// documented.
//
// trustedOwnerUID is a parameter rather than a hard-coded 0 precisely so this
// table can exist: a test process cannot create root-owned files, and a rule
// that could only be exercised as root would in practice never be exercised.
func TestClassifyWatchdogPathSecurity(t *testing.T) {
	const trusted uint32 = 0
	const attacker uint32 = 501

	dir := func(path string, owner uint32, perm fs.FileMode) pathSecurityFact {
		return pathSecurityFact{Path: path, Role: "a parent directory of the watchdog bossd binary", OwnerUID: owner, Mode: perm | fs.ModeDir}
	}
	file := func(path, role string, owner uint32, perm fs.FileMode) pathSecurityFact {
		return pathSecurityFact{Path: path, Role: role, OwnerUID: owner, Mode: perm}
	}
	plist := func(owner uint32, perm fs.FileMode) pathSecurityFact {
		return file("/Library/LaunchDaemons/com.bossanova.bossd-watchdog.plist", "the watchdog plist", owner, perm)
	}
	binary := func(owner uint32, perm fs.FileMode) pathSecurityFact {
		return file("/usr/local/libexec/bossanova/bossd", "the watchdog bossd binary", owner, perm)
	}
	safeTree := []pathSecurityFact{
		plist(trusted, 0o644),
		binary(trusted, 0o755),
		dir("/Library/LaunchDaemons", trusted, 0o755),
		dir("/Library", trusted, 0o755),
		dir("/usr/local/libexec/bossanova", trusted, 0o755),
		dir("/usr/local/libexec", trusted, 0o755),
		dir("/usr/local", trusted, 0o755),
		dir("/usr", trusted, 0o755),
		dir("/", trusted, 0o755),
	}
	withReplaced := func(replacement pathSecurityFact) []pathSecurityFact {
		out := make([]pathSecurityFact, 0, len(safeTree))
		replaced := false
		for _, fact := range safeTree {
			if fact.Path == replacement.Path {
				out = append(out, replacement)
				replaced = true
				continue
			}
			out = append(out, fact)
		}
		if !replaced {
			t.Fatalf("test bug: %s is not part of the safe tree", replacement.Path)
		}
		return out
	}

	cases := []struct {
		name string
		// facts is the whole observed tree for this row.
		facts []pathSecurityFact
		// wantRefusedPath is empty when the row must be accepted.
		wantRefusedPath string
		// wantReason is a fragment of the refusal that names WHY, so a row
		// cannot pass by being refused for an unrelated reason.
		wantReason string
	}{
		{
			name:  "a fully root-owned tree is accepted",
			facts: safeTree,
		},
		{
			name:            "a user-owned target binary is refused",
			facts:           withReplaced(binary(attacker, 0o755)),
			wantRefusedPath: "/usr/local/libexec/bossanova/bossd",
			wantReason:      "owned by uid 501",
		},
		{
			name:            "a group-writable target binary is refused",
			facts:           withReplaced(binary(trusted, 0o775)),
			wantRefusedPath: "/usr/local/libexec/bossanova/bossd",
			wantReason:      "writable by group or other",
		},
		{
			name:            "a world-writable target binary is refused",
			facts:           withReplaced(binary(trusted, 0o757)),
			wantRefusedPath: "/usr/local/libexec/bossanova/bossd",
			wantReason:      "writable by group or other",
		},
		{
			name:            "a user-owned plist is refused",
			facts:           withReplaced(plist(attacker, 0o644)),
			wantRefusedPath: "com.bossanova.bossd-watchdog.plist",
			wantReason:      "owned by uid 501",
		},
		{
			name:            "a group-writable plist is refused",
			facts:           withReplaced(plist(trusted, 0o664)),
			wantRefusedPath: "com.bossanova.bossd-watchdog.plist",
			wantReason:      "writable by group or other",
		},
		{
			// The Homebrew-on-Intel shape: /usr/local handed to the console
			// user. Nothing about the plist or the binary looks wrong.
			name:            "a user-owned parent directory is refused",
			facts:           withReplaced(dir("/usr/local", attacker, 0o755)),
			wantRefusedPath: "/usr/local",
			wantReason:      "owned by uid 501",
		},
		{
			name:            "a group-writable parent directory is refused",
			facts:           withReplaced(dir("/usr/local", trusted, 0o775)),
			wantRefusedPath: "/usr/local",
			wantReason:      "writable by group or other",
		},
		{
			name:            "a world-writable parent directory is refused",
			facts:           withReplaced(dir("/Library", trusted, 0o777)),
			wantRefusedPath: "/Library",
			wantReason:      "writable by group or other",
		},
		{
			// The exemption, and the reason it exists: some macOS releases
			// ship /Library as drwxrwxr-t. Sticky means a non-owner cannot
			// rename or delete our root-owned entry, so this is a real
			// mitigation rather than a hole — and without it the mode would
			// refuse to install on a stock machine.
			name:  "a sticky group-writable parent directory is accepted",
			facts: withReplaced(dir("/Library", trusted, 0o775|fs.ModeSticky)),
		},
		{
			// The exemption is for DIRECTORIES only. Sticky on a plain file
			// confers no such protection, so it must not launder a writable
			// artifact.
			name:            "sticky does not excuse a group-writable file",
			facts:           withReplaced(plist(trusted, 0o664|fs.ModeSticky)),
			wantRefusedPath: "com.bossanova.bossd-watchdog.plist",
			wantReason:      "writable by group or other",
		},
		{
			name:            "a symlinked parent directory is refused",
			facts:           withReplaced(pathSecurityFact{Path: "/usr/local", Role: "a parent directory of the watchdog bossd binary", OwnerUID: trusted, Mode: 0o755 | fs.ModeSymlink}),
			wantRefusedPath: "/usr/local",
			wantReason:      "symlink",
		},
		{
			name:            "a missing artifact is refused rather than assumed safe",
			facts:           withReplaced(pathSecurityFact{Path: "/usr/local/libexec/bossanova/bossd", Role: "the watchdog bossd binary", Missing: true}),
			wantRefusedPath: "/usr/local/libexec/bossanova/bossd",
			wantReason:      "does not exist",
		},
		{
			name:            "a path that could not be inspected is refused rather than assumed safe",
			facts:           withReplaced(pathSecurityFact{Path: "/Library", Role: "a parent directory of the watchdog plist", StatErr: errors.New("permission denied")}),
			wantRefusedPath: "/Library",
			wantReason:      "could not be inspected",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyWatchdogPathSecurity(tt.facts, trusted)
			if tt.wantRefusedPath == "" {
				if err != nil {
					t.Fatalf("a safe tree was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("an unsafe tree was accepted; a root-owned LaunchDaemon would have been bootstrapped against it")
			}
			if !errors.Is(err, ErrUnattendedInsecurePath) {
				t.Fatalf("error = %v, want ErrUnattendedInsecurePath", err)
			}
			if !strings.Contains(err.Error(), tt.wantRefusedPath) {
				t.Errorf("refusal %q does not name the offending path %q", err, tt.wantRefusedPath)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("refusal %q does not say what is wrong (%q)", err, tt.wantReason)
			}
		})
	}
}

// TestClassifyWatchdogPathSecurityHonoursTheTrustedOwner pins the parameter
// itself. Without this the whole table above could pass over a rule that
// ignored trustedOwnerUID and hard-coded 0 — every fact in it is uid 0 or 501.
func TestClassifyWatchdogPathSecurityHonoursTheTrustedOwner(t *testing.T) {
	fact := pathSecurityFact{Path: "/opt/x", Role: "the watchdog bossd binary", OwnerUID: 501, Mode: 0o755}
	if err := classifyWatchdogPathSecurity([]pathSecurityFact{fact}, 501); err != nil {
		t.Fatalf("uid 501 declared trusted but its own path was refused: %v", err)
	}
	if err := classifyWatchdogPathSecurity([]pathSecurityFact{fact}, 0); err == nil {
		t.Fatal("uid 501 path accepted while uid 0 was the trusted owner")
	}
}

// TestWatchdogSecurityTargetsCoverEveryDirectoryToTheRoot pins the coverage the
// rule is applied over. A rule that is never handed /usr/local cannot refuse a
// user-owned /usr/local, and the table above would still be green.
func TestWatchdogSecurityTargetsCoverEveryDirectoryToTheRoot(t *testing.T) {
	layout := newWatchdogLayout("/")
	covered := map[string]bool{}
	for _, target := range watchdogSecurityTargets(layout) {
		covered[target.Path] = true
	}
	for _, want := range []string{
		"/Library/LaunchDaemons/com.bossanova.bossd-watchdog.plist",
		"/usr/local/libexec/bossanova/bossd",
		"/Library/LaunchDaemons",
		"/Library",
		"/usr/local/libexec/bossanova",
		"/usr/local/libexec",
		"/usr/local",
		"/usr",
		"/",
		// launchd opens the job's StandardOutPath/StandardErrorPath as root
		// with O_CREAT|O_APPEND and follows symlinks, so a log directory
		// anybody else can write is a root-append primitive. A rule never
		// shown it cannot refuse it.
		"/var/log/bossanova",
	} {
		if !covered[want] {
			t.Errorf("the security rule is never shown %s, so it can never refuse it", want)
		}
	}
}

// TestWatchdogSecurityTargetsDoNotWalkTheLogDirectorysAncestors pins the
// deliberate asymmetry between the log chain and the plist/binary chains.
//
// On macOS /var is a SYMLINK to private/var. Rule 3 refuses a symlink anywhere
// in a chain, so walking /var/log/bossanova's ancestors the way the plist and
// binary chains are walked would refuse the install on every healthy host —
// a false refusal, not a caught hole. The log directory is therefore checked as
// itself and nothing above it.
func TestWatchdogSecurityTargetsDoNotWalkTheLogDirectorysAncestors(t *testing.T) {
	covered := map[string]bool{}
	for _, target := range watchdogSecurityTargets(newWatchdogLayout("/")) {
		covered[target.Path] = true
	}
	for _, forbidden := range []string{"/var", "/var/log"} {
		if covered[forbidden] {
			t.Errorf("the rule is handed %s; on macOS /var is a symlink to private/var, so rule 3 would refuse every install on a healthy host", forbidden)
		}
	}
}

// TestWatchdogSecurityTargetsStopAtTheConfiguredRoot pins the other half: the
// walk is bounded by the layout root, so a test rooted in a temp directory does
// not drag /var/folders' ownership into a verdict about the watchdog.
func TestWatchdogSecurityTargetsStopAtTheConfiguredRoot(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "watchdog-root")
	for _, target := range watchdogSecurityTargets(newWatchdogLayout(root)) {
		if !strings.HasPrefix(target.Path, root) {
			t.Errorf("target %s escapes the configured root %s", target.Path, root)
		}
	}
}

// TestNewWatchdogLayoutKeepsTheBinaryOffTheUserWritableStagedPath is the
// security DECISION, asserted rather than only commented.
//
// EnsureStaged puts bossd under the user's ~/Library/Application Support, which
// is user-writable by construction. Reusing it for a root-owned LaunchDaemon
// would be a root-execution primitive for that user, so the unattended mode
// keeps its own copy under a system directory. A future edit that "simplified"
// the two paths back together is exactly what this catches.
func TestNewWatchdogLayoutKeepsTheBinaryOffTheUserWritableStagedPath(t *testing.T) {
	layout := newWatchdogLayout("/")
	if want := "/usr/local/libexec/bossanova/bossd"; layout.BinaryPath != want {
		t.Errorf("BinaryPath = %q, want %q", layout.BinaryPath, want)
	}
	for _, forbidden := range []string{"Application Support", "/Users/", "Library/Logs"} {
		if strings.Contains(layout.BinaryPath, forbidden) {
			t.Errorf("BinaryPath %q sits under a per-user, user-writable location (%q)", layout.BinaryPath, forbidden)
		}
		if strings.Contains(layout.LogDir, forbidden) {
			t.Errorf("LogDir %q sits under a per-user, user-writable location (%q)", layout.LogDir, forbidden)
		}
	}
	if want := "/Library/LaunchDaemons/com.bossanova.bossd-watchdog.plist"; layout.PlistPath != want {
		t.Errorf("PlistPath = %q, want %q", layout.PlistPath, want)
	}
}

// TestWatchdogSecurityTargetsMarkOnlyTheWrittenFilesAsArtifacts pins the field
// the pre-write passes select on.
//
// verifyWatchdogDirs and verifyWatchdogDirsBeforeCreate run the rule over
// directories only, and they used to identify the leaves by comparing each
// target's path against layout.PlistPath and layout.BinaryPath — a coupling by
// string coincidence that silently decided the treatment of any leaf added
// later. The log directory is exactly that case: a leaf of the layout that is a
// DIRECTORY and must be checked before anything is written into it.
func TestWatchdogSecurityTargetsMarkOnlyTheWrittenFilesAsArtifacts(t *testing.T) {
	layout := newWatchdogLayout("/")
	artifacts := map[string]bool{}
	seen := map[string]bool{}
	for _, target := range watchdogSecurityTargets(layout) {
		seen[target.Path] = true
		if target.IsArtifact {
			artifacts[target.Path] = true
		}
	}
	for _, want := range []string{layout.PlistPath, layout.BinaryPath} {
		if !artifacts[want] {
			t.Errorf("%s is not marked IsArtifact, so the pre-write passes would try to check a file that does not exist yet", want)
		}
	}
	if len(artifacts) != 2 {
		t.Errorf("IsArtifact targets = %v, want exactly the plist and the bossd binary", artifacts)
	}
	if !seen[layout.LogDir] {
		t.Fatalf("the log directory %s is not a target at all", layout.LogDir)
	}
	if artifacts[layout.LogDir] {
		t.Errorf("%s is marked IsArtifact, so the pre-write directory passes would skip it — a writable log directory is a root-append primitive", layout.LogDir)
	}
}

// TestClassifyEnsureRunningRouteMatrix is the BOS-1203 routing rule, stated as
// a matrix.
//
// It lives in the UNTAGGED test file on purpose, next to
// TestClassifyWatchdogPathSecurity and for the same two reasons. Both platform
// postures become provable from either platform's run — the rows below describe
// macOS hosts that a Linux CI machine can never be in. And exercising the
// classifier from an untagged test is what makes it live code in the Linux lint
// corpus, which is where BOS-1184 lost six symbols to `unused` after placing
// darwin-only helpers in an untagged file.
//
// The rows that carry the rule's weight are the three where the CONFIGURED mode
// disagrees with the machine (KTD1). A mode-only branch — `Mode ==
// SupervisionModeUnattended` — passes the "present" row and fails all three of
// those, which is exactly why they are here rather than left implied.
func TestClassifyEnsureRunningRouteMatrix(t *testing.T) {
	unrecognised := errors.New("unrecognised daemon_supervision_mode \"unattnded\"")
	unparseable := errors.New("settings.json: unexpected end of JSON input")

	for _, tc := range []struct {
		name  string
		facts ensureRunningFacts
		want  ensureRunningRoute
	}{
		{
			name:  "no watchdog plist on the default host takes today's path",
			facts: ensureRunningFacts{Mode: SupervisionModeLaunchAgent},
			want:  ensureRunningRouteLaunchAgent,
		},
		{
			// KTD7. The operator selected the mode but has not run the root
			// install, so there is no watchdog to contend with. Skipping the
			// load here would strip supervision from a host that has it.
			name: "unattended selected but never installed keeps the LaunchAgent",
			facts: ensureRunningFacts{
				Mode:         SupervisionModeUnattended,
				InstallState: UnattendedInstallAbsent,
			},
			want: ensureRunningRouteLaunchAgent,
		},
		{
			name: "the ordinary unattended host routes to the watchdog",
			facts: ensureRunningFacts{
				Mode:                 SupervisionModeUnattended,
				InstallState:         UnattendedInstallPresent,
				WatchdogPlistPresent: true,
			},
			want: ensureRunningRouteWatchdog,
		},
		{
			// KTD5: verifyWatchdogPaths failing is a durable host fault, so the
			// wait is known-futile before it is spent.
			name: "an insecure watchdog routes away from the LaunchAgent without waiting",
			facts: ensureRunningFacts{
				Mode:                 SupervisionModeUnattended,
				InstallState:         UnattendedInstallInsecure,
				WatchdogPlistPresent: true,
			},
			want: ensureRunningRouteWatchdogNoWait,
		},
		{
			// KTD1, row one. A typo OF "unattended" fails closed to NO mode —
			// on precisely the host that has the watchdog installed.
			name: "an unrecognised settings value still routes to the watchdog",
			facts: ensureRunningFacts{
				ModeErr:              unrecognised,
				InstallState:         UnattendedInstallNotApplicable,
				WatchdogPlistPresent: true,
			},
			want: ensureRunningRouteWatchdog,
		},
		{
			// KTD1, row two. An unparseable settings.json resolves Mode to the
			// DEFAULT while the root job is still loaded.
			name: "an unparseable settings file still routes to the watchdog",
			facts: ensureRunningFacts{
				Mode:                 SupervisionModeLaunchAgent,
				SettingsErr:          unparseable,
				InstallState:         UnattendedInstallNotApplicable,
				WatchdogPlistPresent: true,
			},
			want: ensureRunningRouteWatchdog,
		},
		{
			// KTD1, row three. The key was reverted to the default and the root
			// job was left loaded — the BOS-1184 R6 back-out sequence.
			name: "a reverted key with the root job still loaded routes to the watchdog",
			facts: ensureRunningFacts{
				Mode:                 SupervisionModeLaunchAgent,
				InstallState:         UnattendedInstallNotApplicable,
				WatchdogPlistPresent: true,
			},
			want: ensureRunningRouteWatchdog,
		},
		{
			// The default: arm. A plist is present and the observation says
			// "not applicable" for a reason this code does not know about.
			// Routing to the watchdog costs a bounded wait; routing to the
			// LaunchAgent would create the second supervisor.
			name: "an unknown install state never falls through to the LaunchAgent",
			facts: ensureRunningFacts{
				Mode:                 SupervisionModeUnattended,
				InstallState:         UnattendedInstallState(99),
				WatchdogPlistPresent: true,
			},
			want: ensureRunningRouteWatchdog,
		},
		{
			// A caller that gathered nothing must be routed to the behaviour
			// that predates this classifier, never to one that acts on a
			// root-owned job.
			name:  "the zero-value fact struct is the LaunchAgent route",
			facts: ensureRunningFacts{},
			want:  ensureRunningRouteLaunchAgent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEnsureRunningRoute(tc.facts); got != tc.want {
				t.Errorf("classifyEnsureRunningRoute(%+v) = %s, want %s", tc.facts, got, tc.want)
			}
		})
	}
}

// TestClassifyEnsureRunningRouteIgnoresTheConfiguredMode is KTD1 stated as a
// property rather than as rows.
//
// The matrix above proves the three disagreeing states route correctly. This
// proves the stronger claim they are evidence FOR: with a watchdog plist on
// disk, no combination of Mode, ModeErr and SettingsErr can produce the
// LaunchAgent route. A mode-only branch satisfies every row of a matrix that
// happens to omit one combination; it cannot satisfy this.
func TestClassifyEnsureRunningRouteIgnoresTheConfiguredMode(t *testing.T) {
	modes := []SupervisionMode{"", SupervisionModeLaunchAgent, SupervisionModeUnattended, "unattnded"}
	errs := []error{nil, errors.New("boom")}
	states := []UnattendedInstallState{
		UnattendedInstallNotApplicable,
		UnattendedInstallAbsent,
		UnattendedInstallPresent,
		UnattendedInstallInsecure,
	}
	for _, mode := range modes {
		for _, modeErr := range errs {
			for _, settingsErr := range errs {
				for _, state := range states {
					facts := ensureRunningFacts{
						Mode:                 mode,
						ModeErr:              modeErr,
						SettingsErr:          settingsErr,
						InstallState:         state,
						WatchdogPlistPresent: true,
					}
					if got := classifyEnsureRunningRoute(facts); got == ensureRunningRouteLaunchAgent {
						t.Errorf("classifyEnsureRunningRoute(%+v) = %s — a watchdog plist on disk must never route to the gui LaunchAgent", facts, got)
					}
					// And the mirror: with no plist, nothing routes AWAY from
					// the LaunchAgent, so R5's default path cannot be disturbed
					// by a settings value either.
					facts.WatchdogPlistPresent = false
					if got := classifyEnsureRunningRoute(facts); got != ensureRunningRouteLaunchAgent {
						t.Errorf("classifyEnsureRunningRoute(%+v) = %s — with no watchdog plist the default path must be unchanged", facts, got)
					}
				}
			}
		}
	}
}
