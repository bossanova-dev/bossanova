//go:build darwin

package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

// The helpers below are what make this unit testable at all. Installing the
// unattended substrate needs root, writes to /Library/LaunchDaemons and
// /usr/local/libexec, and bootstraps a system-domain launchd job — none of
// which a test may do. Each of those is a package-var seam
// (watchdogFilesystemRoot, currentEUID, chownToRoot, watchdogTrustedOwnerUID,
// lookupUserByUID, runLaunchctl), following the same indirection idiom the
// rest of this package already uses, so the whole path runs against a temp
// directory as an ordinary user.
//
// The seams move WHERE the check looks and WHAT uid it trusts. They never
// weaken the check itself: classifyWatchdogPathSecurity is untouched, so a
// refusal proven here is the refusal production performs.

const watchdogTestUser = "testuser"
const watchdogTestUID = 501

func stubNonRootEUID(t *testing.T) {
	t.Helper()
	original := currentEUID
	currentEUID = func() int { return watchdogTestUID }
	t.Cleanup(func() { currentEUID = original })
}

func stubRootEUID(t *testing.T) {
	t.Helper()
	original := currentEUID
	currentEUID = func() int { return 0 }
	t.Cleanup(func() { currentEUID = original })
}

// useTempWatchdogRoot rebuilds the whole /Library + /usr/local + /var/log
// layout inside a temp directory, declares the test process's own uid the
// trusted owner, and turns the root chown into a no-op.
func useTempWatchdogRoot(t *testing.T) watchdogLayout {
	t.Helper()
	root := t.TempDir()

	originalRoot := watchdogFilesystemRoot
	watchdogFilesystemRoot = root
	t.Cleanup(func() { watchdogFilesystemRoot = originalRoot })

	originalTrusted := watchdogTrustedOwnerUID
	watchdogTrustedOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() { watchdogTrustedOwnerUID = originalTrusted })

	originalChown := chownToRoot
	chownToRoot = func(string) error { return nil }
	t.Cleanup(func() { chownToRoot = originalChown })

	originalLookup := lookupUserByUID
	lookupUserByUID = func(int) (string, string, error) {
		return watchdogTestUser, filepath.Join(root, "Users", watchdogTestUser), nil
	}
	t.Cleanup(func() { lookupUserByUID = originalLookup })

	t.Setenv("SUDO_USER", watchdogTestUser)
	t.Setenv("SUDO_UID", "501")

	// The plist's parent must exist for the install to write into it; the real
	// /Library/LaunchDaemons always does.
	if err := os.MkdirAll(filepath.Join(root, watchdogPlistDirRel), 0o755); err != nil {
		t.Fatalf("create LaunchDaemons dir: %v", err)
	}
	// BOS-1203: so must the TARGET USER's LaunchAgents directory, for the same
	// reason — a real /Users/<user>/Library/LaunchAgents always exists. Without
	// it the supersession step reports "could not establish whether a
	// LaunchAgent is installed" on every install in this file, which is the
	// correct answer for an unmounted home and the wrong fixture for a host.
	if err := os.MkdirAll(filepath.Join(root, "Users", watchdogTestUser, "Library", "LaunchAgents"), 0o755); err != nil {
		t.Fatalf("create target LaunchAgents dir: %v", err)
	}
	return watchdogPaths()
}

// recordLaunchctl captures launchctl invocations instead of running them, so a
// test can assert both the arguments AND that a refusal never reached launchd
// at all.
func recordLaunchctl(t *testing.T) *[][]string {
	t.Helper()
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	calls := &[][]string{}
	original := runLaunchctl
	runLaunchctl = func(args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		return []byte(""), nil
	}
	t.Cleanup(func() { runLaunchctl = original })
	return calls
}

// installTestWatchdog runs a successful install and returns the layout.
func installTestWatchdog(t *testing.T) (watchdogLayout, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubRootEUID(t)
	source := writeFakeCellarBossd(t, home, "watchdog build")
	if err := platformInstallUnattended(source, false); err != nil {
		t.Fatalf("platformInstallUnattended: %v", err)
	}
	return layout, source
}

func TestPlatformInstallUnattendedWritesRootOwnedArtifacts(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	layout, _ := installTestWatchdog(t)

	plistInfo, err := os.Stat(layout.PlistPath)
	if err != nil {
		t.Fatalf("stat watchdog plist: %v", err)
	}
	// 0644 exactly: launchd refuses a group- or world-writable plist, and it
	// still has to be able to READ the file, so 0600 would not do either.
	if got := plistInfo.Mode().Perm(); got != watchdogPlistMode {
		t.Errorf("watchdog plist mode = %#o, want %#o", got, watchdogPlistMode)
	}
	binaryInfo, err := os.Stat(layout.BinaryPath)
	if err != nil {
		t.Fatalf("stat watchdog bossd: %v", err)
	}
	if got := binaryInfo.Mode().Perm(); got != watchdogBinaryMode {
		t.Errorf("watchdog bossd mode = %#o, want %#o", got, watchdogBinaryMode)
	}
	contents, err := os.ReadFile(layout.BinaryPath)
	if err != nil {
		t.Fatalf("read watchdog bossd: %v", err)
	}
	if string(contents) != "watchdog build" {
		t.Errorf("watchdog bossd contents = %q, want the source binary's", contents)
	}

	plist, err := os.ReadFile(layout.PlistPath)
	if err != nil {
		t.Fatalf("read watchdog plist: %v", err)
	}
	for _, want := range []string{
		"<string>com.bossanova.bossd-watchdog</string>",
		"<string>/usr/bin/sudo</string>",
		"<string>-u</string>",
		"<string>" + watchdogTestUser + "</string>",
		"<string>" + layout.BinaryPath + "</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("installed watchdog plist does not contain %q:\n%s", want, plist)
		}
	}
	// The whole point of the root-owned copy: the job must NOT be pointed at
	// the per-user staged binary, which is writable by the user it runs as.
	stagedPath := expectedStagedBossdPath(t)
	if strings.Contains(string(plist), stagedPath) {
		t.Errorf("watchdog plist points at the user-writable staged path %s:\n%s", stagedPath, plist)
	}
}

func TestPlatformInstallUnattendedRefusesWithoutRoot(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubNonRootEUID(t)
	source := writeFakeCellarBossd(t, home, "watchdog build")

	err := platformInstallUnattended(source, false)
	if !errors.Is(err, ErrUnattendedRequiresRoot) {
		t.Fatalf("error = %v, want ErrUnattendedRequiresRoot", err)
	}
	// The refusal has to be actionable: `boss daemon install` is normally run
	// as the user, so an operator who hits this needs the exact command, not a
	// statement that root is required.
	for _, want := range []string{WatchdogInstallCommand, string(SupervisionModeUnattended), "administrator password"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if _, statErr := os.Stat(layout.PlistPath); !os.IsNotExist(statErr) {
		t.Errorf("a refused install still wrote %s (stat err = %v)", layout.PlistPath, statErr)
	}
	if _, statErr := os.Stat(layout.BinDir); !os.IsNotExist(statErr) {
		t.Errorf("a refused install still created %s (stat err = %v)", layout.BinDir, statErr)
	}
}

// TestPlatformInstallUnattendedRefusesARootLoginWithNoSudoTarget covers the
// case that would otherwise install a watchdog running bossd AS ROOT: a genuine
// root shell, where SUDO_USER names nobody.
func TestPlatformInstallUnattendedRefusesARootLoginWithNoSudoTarget(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	for _, tt := range []struct {
		name, user, uid string
		// resolvedName, when set, is the name the uid ACTUALLY resolves to,
		// which is how a mismatched SUDO_USER/SUDO_UID pair is stated.
		resolvedName string
	}{
		{name: "no sudo environment at all", user: "", uid: ""},
		{name: "sudo user without a uid", user: watchdogTestUser, uid: ""},
		{name: "sudo target is root itself", user: "root", uid: "0"},
		{name: "unusable uid", user: watchdogTestUser, uid: "not-a-number"},
		// SUDO_USER and SUDO_UID are two independent environment values, and
		// the rendered argv mixes them: `asuser <uid>` enters one account's
		// bootstrap session while `sudo -u <name>` drops to another's
		// credentials. A pair that disagrees would install a persistent
		// root-owned job doing exactly that.
		{name: "sudo user and sudo uid name different accounts", user: watchdogTestUser, uid: "501", resolvedName: "someoneelse"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			layout := useTempWatchdogRoot(t)
			stubRootEUID(t)
			t.Setenv("SUDO_USER", tt.user)
			t.Setenv("SUDO_UID", tt.uid)
			if tt.resolvedName != "" {
				resolved := tt.resolvedName
				lookupUserByUID = func(int) (string, string, error) {
					return resolved, filepath.Join(layout.Root, "Users", resolved), nil
				}
			}
			source := writeFakeCellarBossd(t, home, "watchdog build")

			err := platformInstallUnattended(source, false)
			if !errors.Is(err, ErrUnattendedRequiresRoot) {
				t.Fatalf("error = %v, want ErrUnattendedRequiresRoot", err)
			}
			if tt.resolvedName != "" {
				for _, want := range []string{tt.user, tt.resolvedName} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not name %q, so an operator cannot see which pair disagreed", err, want)
					}
				}
			}
			if _, statErr := os.Stat(layout.PlistPath); !os.IsNotExist(statErr) {
				t.Errorf("a refused install still wrote %s", layout.PlistPath)
			}
		})
	}
}

// TestPlatformInstallUnattendedRefusesAUserWritableParentDirectory is the
// Homebrew-on-Intel shape, and the reason the check cannot be limited to the
// artifacts the installer writes itself: it CREATES the leaf directories with
// the right mode, so a hole can only be inherited from above.
//
// It also pins that the refusal happens BEFORE launchd is told anything.
func TestPlatformInstallUnattendedRefusesAUserWritableParentDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubRootEUID(t)
	calls := recordLaunchctl(t)

	hostile := filepath.Join(layout.Root, "usr", "local")
	if err := os.MkdirAll(hostile, 0o777); err != nil {
		t.Fatalf("create hostile parent: %v", err)
	}
	if err := os.Chmod(hostile, 0o777); err != nil {
		t.Fatalf("chmod hostile parent: %v", err)
	}
	source := writeFakeCellarBossd(t, home, "watchdog build")

	err := platformInstallUnattended(source, false)
	if !errors.Is(err, ErrUnattendedInsecurePath) {
		t.Fatalf("error = %v, want ErrUnattendedInsecurePath", err)
	}
	if !strings.Contains(err.Error(), hostile) {
		t.Errorf("refusal %q does not name the offending directory %s", err, hostile)
	}
	if len(*calls) != 0 {
		t.Errorf("an insecure tree still reached launchctl: %v", *calls)
	}
	if _, statErr := os.Stat(layout.PlistPath); !os.IsNotExist(statErr) {
		t.Errorf("an insecure tree still had a plist written at %s", layout.PlistPath)
	}
}

// TestVerifyWatchdogPathsRefusesWritableArtifacts covers the two leaves the
// install writes itself. They cannot be exercised through platformInstall —
// it chmods them to the right mode — but they are exactly what a later `chmod`
// on the host, or a package manager, can loosen, and the observation path
// (observeUnattendedInstall) reads them on every status call.
func TestVerifyWatchdogPathsRefusesWritableArtifacts(t *testing.T) {
	for _, tt := range []struct {
		name string
		// pick returns the path to loosen, from an already-installed layout.
		pick func(watchdogLayout) string
		mode os.FileMode
	}{
		{"a group-writable plist", func(l watchdogLayout) string { return l.PlistPath }, 0o664},
		{"a world-writable plist", func(l watchdogLayout) string { return l.PlistPath }, 0o666},
		{"a group-writable target binary", func(l watchdogLayout) string { return l.BinaryPath }, 0o775},
		{"a world-writable target binary", func(l watchdogLayout) string { return l.BinaryPath }, 0o777},
		{"a world-writable binary directory", func(l watchdogLayout) string { return l.BinDir }, 0o777},
		// launchd creates and appends the job's stdout/stderr redirects as
		// root and follows symlinks, so a writable log directory is the same
		// class of hole as a writable binary directory.
		{"a group-writable log directory", func(l watchdogLayout) string { return l.LogDir }, 0o775},
		{"a world-writable log directory", func(l watchdogLayout) string { return l.LogDir }, 0o777},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			layout, _ := installTestWatchdog(t)
			if err := verifyWatchdogPaths(layout); err != nil {
				t.Fatalf("a freshly installed watchdog was already refused, so this row proves nothing: %v", err)
			}

			target := tt.pick(layout)
			if err := os.Chmod(target, tt.mode); err != nil {
				t.Fatalf("chmod %s: %v", target, err)
			}
			err := verifyWatchdogPaths(layout)
			if !errors.Is(err, ErrUnattendedInsecurePath) {
				t.Fatalf("error = %v, want ErrUnattendedInsecurePath", err)
			}
			if !strings.Contains(err.Error(), target) {
				t.Errorf("refusal %q does not name %s", err, target)
			}
			// The observation surfaces must agree: a host in this state has a
			// root-execution primitive on it and must not be reported healthy.
			observed := observeUnattendedInstall()
			if observed.State != UnattendedInstallInsecure {
				t.Errorf("observeUnattendedInstall().State = %d, want UnattendedInstallInsecure", observed.State)
			}
			if !errors.Is(observed.Err, ErrUnattendedInsecurePath) {
				t.Errorf("observed Err = %v, want the insecure-path reason carried through for reporting", observed.Err)
			}
		})
	}
}

func TestPlatformInstallUnattendedRefusesToOverwriteWithoutForce(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	layout, source := installTestWatchdog(t)

	if err := platformInstallUnattended(source, false); err == nil {
		t.Fatal("a second install without --force succeeded")
	} else if !strings.Contains(err.Error(), layout.PlistPath) {
		t.Errorf("refusal %q does not name the existing plist", err)
	}
	if err := platformInstallUnattended(source, true); err != nil {
		t.Fatalf("platformInstallUnattended(force): %v", err)
	}
}

func TestPlatformInstallUnattendedBootstrapsIntoTheSystemDomain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubRootEUID(t)
	calls := recordLaunchctl(t)
	source := writeFakeCellarBossd(t, home, "watchdog build")

	if err := platformInstallUnattended(source, false); err != nil {
		t.Fatalf("platformInstallUnattended: %v", err)
	}
	// BOS-1203 added a second call: the supersession bootout of the per-user
	// agent this substrate replaces. Asserting the exact pair rather than
	// loosening the count keeps the test able to catch a THIRD, unintended
	// service operation appearing here.
	if len(*calls) != 2 {
		t.Fatalf("launchctl calls = %v, want the bootstrap followed by the supersession bootout", *calls)
	}
	// The bootstrap must come FIRST. Booting the agent out before the watchdog
	// is loaded would leave a window with no supervisor at all, which is the
	// BOS-1181 net-loss-of-service shape; the overlap in this order is bounded
	// by bossd's singleton lock instead.
	// `system`, not `gui/<uid>`: the domain with no foreground-console concept
	// is the entire reason this substrate exists.
	want := []string{"bootstrap", "system", layout.PlistPath}
	got := (*calls)[0]
	if len(got) != len(want) {
		t.Fatalf("bootstrap args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bootstrap args = %v, want %v", got, want)
		}
	}
}

func TestPlatformUninstallUnattendedRemovesEverythingItInstalled(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	layout, _ := installTestWatchdog(t)
	// launchd writes these; the uninstall has to clear them too or it leaves
	// root-owned files behind under a root-owned directory.
	for _, name := range []string{"bossd-watchdog.stdout.log", "bossd-watchdog.stderr.log"} {
		if err := os.WriteFile(filepath.Join(layout.LogDir, name), []byte("log"), 0o644); err != nil {
			t.Fatalf("write fake log: %v", err)
		}
	}

	if err := platformUninstallUnattended(); err != nil {
		t.Fatalf("platformUninstallUnattended: %v", err)
	}
	for _, path := range []string{layout.PlistPath, layout.BinaryPath, layout.BinDir, layout.LogDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("uninstall left root-owned residue at %s (stat err = %v)", path, err)
		}
	}
}

func TestPlatformUninstallUnattendedBootsOutTheSystemJob(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	installTestWatchdog(t)
	calls := recordLaunchctl(t)

	if err := platformUninstallUnattended(); err != nil {
		t.Fatalf("platformUninstallUnattended: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0][0] != "bootout" || (*calls)[0][1] != "system/"+WatchdogLabel {
		t.Fatalf("launchctl calls = %v, want a single bootout of system/%s", *calls, WatchdogLabel)
	}
}

// TestPlatformUninstallUnattendedToleratesAbsenceAndPartialInstalls is the
// "no root-owned residue" criterion's real shape. A half-removed root job is
// worse than an installed one because nothing reports it afterwards, so every
// step has to tolerate its piece already being gone.
func TestPlatformUninstallUnattendedToleratesAbsenceAndPartialInstalls(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setUp func(t *testing.T, layout watchdogLayout)
	}{
		{"nothing installed at all", func(*testing.T, watchdogLayout) {}},
		{
			"plist only",
			func(t *testing.T, layout watchdogLayout) {
				if err := os.WriteFile(layout.PlistPath, []byte("<plist/>"), 0o644); err != nil {
					t.Fatalf("write plist: %v", err)
				}
			},
		},
		{
			"binary only",
			func(t *testing.T, layout watchdogLayout) {
				if err := os.MkdirAll(layout.BinDir, 0o755); err != nil {
					t.Fatalf("create bin dir: %v", err)
				}
				if err := os.WriteFile(layout.BinaryPath, []byte("bossd"), 0o755); err != nil {
					t.Fatalf("write binary: %v", err)
				}
			},
		},
		{
			"log directory only",
			func(t *testing.T, layout watchdogLayout) {
				if err := os.MkdirAll(layout.LogDir, 0o755); err != nil {
					t.Fatalf("create log dir: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			layout := useTempWatchdogRoot(t)
			stubRootEUID(t)
			tt.setUp(t, layout)

			for attempt := 1; attempt <= 2; attempt++ {
				if err := platformUninstallUnattended(); err != nil {
					t.Fatalf("uninstall attempt %d: %v", attempt, err)
				}
			}
			for _, path := range []string{layout.PlistPath, layout.BinDir, layout.LogDir} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("residue left at %s (stat err = %v)", path, err)
				}
			}
		})
	}
}

// TestPlatformUninstallUnattendedLeavesForeignLogsAlone pins the one thing the
// teardown must NOT do: a non-empty log directory belongs to whoever put the
// other file there.
func TestPlatformUninstallUnattendedLeavesForeignLogsAlone(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	layout, _ := installTestWatchdog(t)
	foreign := filepath.Join(layout.LogDir, "someone-elses.log")
	if err := os.WriteFile(foreign, []byte("not ours"), 0o644); err != nil {
		t.Fatalf("write foreign log: %v", err)
	}

	if err := platformUninstallUnattended(); err != nil {
		t.Fatalf("platformUninstallUnattended: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("uninstall deleted a file it did not create: %v", err)
	}
}

func TestPlatformUninstallUnattendedRefusesWithoutRoot(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	layout, _ := installTestWatchdog(t)
	stubNonRootEUID(t)

	err := platformUninstallUnattended()
	if !errors.Is(err, ErrUnattendedRequiresRoot) {
		t.Fatalf("error = %v, want ErrUnattendedRequiresRoot", err)
	}
	if !strings.Contains(err.Error(), WatchdogUninstallCommand) {
		t.Errorf("refusal %q does not name %q", err, WatchdogUninstallCommand)
	}
	if _, statErr := os.Stat(layout.PlistPath); statErr != nil {
		t.Errorf("a refused uninstall still removed the plist: %v", statErr)
	}
}

func TestPlatformRestartUnattendedKickstartsTheInstalledJob(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	installTestWatchdog(t)
	calls := recordLaunchctl(t)

	if err := platformRestartUnattended(); err != nil {
		t.Fatalf("platformRestartUnattended: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("launchctl calls = %v, want exactly one", *calls)
	}
	// kickstart -k, not bootout-then-bootstrap: restarting must not rewrite a
	// root-owned artifact (that is install's job, and the step that needs an
	// admin prompt), and must not leave a window with the job unloaded.
	want := []string{"kickstart", "-k", "system/" + WatchdogLabel}
	got := (*calls)[0]
	if len(got) != len(want) {
		t.Fatalf("restart args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart args = %v, want %v", got, want)
		}
	}
}

func TestPlatformRestartUnattendedRefusalPaths(t *testing.T) {
	t.Run("without root", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		installTestWatchdog(t)
		stubNonRootEUID(t)
		if err := platformRestartUnattended(); !errors.Is(err, ErrUnattendedRequiresRoot) {
			t.Fatalf("error = %v, want ErrUnattendedRequiresRoot", err)
		}
	})

	t.Run("when the watchdog was never installed", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		layout := useTempWatchdogRoot(t)
		stubRootEUID(t)
		err := platformRestartUnattended()
		if !errors.Is(err, ErrUnattendedNotInstalled) {
			t.Fatalf("error = %v, want ErrUnattendedNotInstalled", err)
		}
		for _, want := range []string{layout.PlistPath, WatchdogInstallCommand} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %q", err, want)
			}
		}
	})
}

func TestObserveUnattendedInstallReportsTheHostState(t *testing.T) {
	t.Run("absent when nothing is installed", func(t *testing.T) {
		useTempWatchdogRoot(t)
		got := observeUnattendedInstall()
		if got.State != UnattendedInstallAbsent {
			t.Fatalf("State = %d, want UnattendedInstallAbsent", got.State)
		}
		if got.PlistPath == "" || got.BinaryPath == "" {
			t.Error("the observation must carry the paths a report needs to name")
		}
	})

	t.Run("present after a clean install", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		installTestWatchdog(t)
		if got := observeUnattendedInstall(); got.State != UnattendedInstallPresent || got.Err != nil {
			t.Fatalf("State, Err = %d, %v; want UnattendedInstallPresent, nil", got.State, got.Err)
		}
	})

	t.Run("absent when the plist is there but the binary is not", func(t *testing.T) {
		// A partial install must not read as healthy: the plist exists, so the
		// naive "does the plist exist" check would call it installed.
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		layout, _ := installTestWatchdog(t)
		if err := os.RemoveAll(layout.BinDir); err != nil {
			t.Fatalf("remove binary: %v", err)
		}
		got := observeUnattendedInstall()
		if got.State != UnattendedInstallInsecure {
			t.Fatalf("State = %d, want UnattendedInstallInsecure for a plist pointing at a missing binary", got.State)
		}
	})
}

// TestPlatformInstallRoutesTheUnattendedModeToTheWatchdog pins the wiring: the
// mode is what selects the artifact, and selecting `unattended` must not leave
// a gui/<uid> LaunchAgent behind on the host as well.
func TestPlatformInstallRoutesTheUnattendedModeToTheWatchdog(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{DaemonSupervisionMode: "unattended"}, nil
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubRootEUID(t)
	source := writeFakeCellarBossd(t, home, "watchdog build")

	if err := platformInstall(source, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	if _, err := os.Stat(layout.PlistPath); err != nil {
		t.Fatalf("the unattended mode did not install the watchdog: %v", err)
	}
	agentPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if _, statErr := os.Stat(agentPath); !os.IsNotExist(statErr) {
		t.Errorf("the unattended mode also wrote a gui/<uid> LaunchAgent at %s — the substrate it exists to escape", agentPath)
	}
}

// TestPlatformInstallDefaultModeTouchesNoWatchdogArtifact is the R5 companion:
// a host on the default substrate must not acquire any part of the root-owned
// one, and must not need root.
func TestPlatformInstallDefaultModeTouchesNoWatchdogArtifact(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }

	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubNonRootEUID(t)
	source := writeFakeCellarBossd(t, home, "version one")

	if err := platformInstall(source, false); err != nil {
		t.Fatalf("platformInstall on the default substrate: %v", err)
	}
	for _, path := range []string{layout.PlistPath, layout.BinDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the default substrate created a watchdog artifact at %s (stat err = %v)", path, err)
		}
	}
}

// TestPlatformUninstallWarnsAboutRootOwnedResidue covers the gap that routing
// teardown by the CONFIGURED mode leaves: an operator who sets the key back to
// the default and then uninstalls removes only the LaunchAgent, and nothing
// would otherwise ever mention the root-owned job still running.
func TestPlatformUninstallWarnsAboutRootOwnedResidue(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	layout, _ := installTestWatchdog(t)
	// Switch the configured mode back to the default AFTER installing.
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }

	var warned []string
	originalWarn := warnUnattendedWatchdogResidue
	warnUnattendedWatchdogResidue = func(path string) { warned = append(warned, path) }
	t.Cleanup(func() { warnUnattendedWatchdogResidue = originalWarn })

	// Without root there is nothing this command may do but say what is left.
	stubNonRootEUID(t)

	if err := platformUninstall(); err != nil {
		t.Fatalf("platformUninstall: %v", err)
	}
	if len(warned) != 1 || warned[0] != layout.PlistPath {
		t.Fatalf("residue warnings = %v, want one naming %s", warned, layout.PlistPath)
	}
	if _, err := os.Stat(layout.PlistPath); err != nil {
		t.Errorf("the non-root path removed the root-owned plist: %v", err)
	}
}

// TestPlatformUninstallRemovesStrandedWatchdogWhenRoot pins the other half, and
// it is the half AC8 turns on: "uninstall removes the root-owned job
// completely, leaving no residue requiring manual root cleanup".
//
// The warning above names `sudo boss daemon uninstall` as the remedy. Teardown
// routes by the CONFIGURED mode, so on this host — key back at the default,
// root job still installed — that command reaches this same branch. If it only
// warned here it would print its own advice back at the operator and change
// nothing, and the only way out would be restoring a settings value they had
// deliberately changed. Under sudo it must actually remove the job.
func TestPlatformUninstallRemovesStrandedWatchdogWhenRoot(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	layout, _ := installTestWatchdog(t)
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }

	var warned []string
	originalWarn := warnUnattendedWatchdogResidue
	warnUnattendedWatchdogResidue = func(path string) { warned = append(warned, path) }
	t.Cleanup(func() { warnUnattendedWatchdogResidue = originalWarn })

	stubRootEUID(t)

	if err := platformUninstall(); err != nil {
		t.Fatalf("platformUninstall: %v", err)
	}
	if _, err := os.Stat(layout.PlistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("watchdog plist survived a root uninstall: stat err = %v, want ErrNotExist", err)
	}
	if _, err := os.Stat(layout.BinDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("watchdog binary dir survived a root uninstall: stat err = %v, want ErrNotExist", err)
	}
	// Having removed it, there is no residue left to warn about.
	if len(warned) != 0 {
		t.Errorf("residue warnings = %v, want none once the job was removed", warned)
	}
}

// TestPlatformUninstallDoesNotWarnOnACleanDefaultHost keeps the warning from
// becoming noise every operator learns to ignore.
func TestPlatformUninstallDoesNotWarnOnACleanDefaultHost(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }

	home := t.TempDir()
	t.Setenv("HOME", home)
	useTempWatchdogRoot(t)
	mkdirLaunchAgents(t)

	var warned []string
	originalWarn := warnUnattendedWatchdogResidue
	warnUnattendedWatchdogResidue = func(path string) { warned = append(warned, path) }
	t.Cleanup(func() { warnUnattendedWatchdogResidue = originalWarn })

	if err := platformUninstall(); err != nil {
		t.Fatalf("platformUninstall: %v", err)
	}
	if len(warned) != 0 {
		t.Fatalf("residue warnings = %v on a host with no watchdog, want none", warned)
	}
}

// TestPlatformUninstallRoutesTheUnattendedModeToTheWatchdog pins that teardown
// follows the same mode routing install does.
func TestPlatformUninstallRoutesTheUnattendedModeToTheWatchdog(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	layout, _ := installTestWatchdog(t)
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{DaemonSupervisionMode: "unattended"}, nil
	}

	if err := platformUninstall(); err != nil {
		t.Fatalf("platformUninstall: %v", err)
	}
	if _, err := os.Stat(layout.PlistPath); !os.IsNotExist(err) {
		t.Errorf("uninstall left the watchdog plist behind (stat err = %v)", err)
	}
}

// TestPlatformUninstallStillRunsWhenTheModeIsRefused pins the deliberate
// asymmetry: install and restart CREATE a substrate and so must fail closed on
// a bad settings value, while uninstall DESTROYS one and must never be blocked
// by a typo.
func TestPlatformUninstallStillRunsWhenTheModeIsRefused(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{DaemonSupervisionMode: "system-daemon"}, nil
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	useTempWatchdogRoot(t)
	mkdirLaunchAgents(t)
	agentPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if err := os.WriteFile(agentPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("write LaunchAgent: %v", err)
	}

	if err := platformUninstall(); err != nil {
		t.Fatalf("platformUninstall with a refused mode: %v", err)
	}
	if _, statErr := os.Stat(agentPath); !os.IsNotExist(statErr) {
		t.Errorf("a refused settings value blocked the LaunchAgent teardown (stat err = %v)", statErr)
	}
}

// pathOwnerUID reads the uid owning path WITHOUT following a symlink, so a test
// can prove a link's target was not re-owned.
func pathOwnerUID(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat_t for %s", path)
	}
	return stat.Uid
}

// TestPlatformInstallUnattendedRefusesASymlinkedLayoutDirectory is the hole
// TestPlatformInstallUnattendedRefusesAUserWritableParentDirectory does NOT
// cover: that test plants a hostile parent with no child, so os.MkdirAll makes
// a real directory and the ancestor rule fires. Here the layout directory
// itself already EXISTS, as a symlink.
//
// That distinction is the whole vulnerability. os.MkdirAll stats through a
// symlink and returns nil, and neither os.Chmod nor os.Chown is symlink-safe,
// so an installer that created the directories before verifying them would
// chmod 0755 and chown root:wheel whatever the link pointed at — a path chosen
// by whoever could write the parent, which on a Homebrew-owned /usr/local is an
// ordinary user. The refusal must therefore land BEFORE the mutation, not
// after it.
func TestPlatformInstallUnattendedRefusesASymlinkedLayoutDirectory(t *testing.T) {
	for _, tt := range []struct {
		name string
		// pick names the layout directory to replace with a symlink.
		pick func(watchdogLayout) string
	}{
		{"the watchdog binary directory", func(l watchdogLayout) string { return l.BinDir }},
		{"the watchdog log directory", func(l watchdogLayout) string { return l.LogDir }},
		{"the LaunchDaemons directory", func(l watchdogLayout) string { return filepath.Dir(l.PlistPath) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			layout := useTempWatchdogRoot(t)
			stubRootEUID(t)
			calls := recordLaunchctl(t)

			// Record rather than no-op, so "the target was never chowned" is an
			// assertion instead of an artefact of the stub. A test process
			// cannot chown to root, so this is the only way to observe it.
			var chowned []string
			chownToRoot = func(path string) error {
				chowned = append(chowned, path)
				return nil
			}

			victim := filepath.Join(layout.Root, "victim")
			if err := os.MkdirAll(victim, 0o700); err != nil {
				t.Fatalf("create victim: %v", err)
			}
			if err := os.Chmod(victim, 0o700); err != nil {
				t.Fatalf("chmod victim: %v", err)
			}
			victimOwner := pathOwnerUID(t, victim)

			planted := tt.pick(layout)
			if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
				t.Fatalf("create the planted link's parent: %v", err)
			}
			// useTempWatchdogRoot pre-creates LaunchDaemons, as the real host
			// has it; the attacker's move is to have the layout path already
			// be a link, so clear whatever is there first.
			_ = os.Remove(planted)
			if err := os.Symlink(victim, planted); err != nil {
				t.Fatalf("plant symlink at %s: %v", planted, err)
			}

			source := writeFakeCellarBossd(t, home, "watchdog build")
			err := platformInstallUnattended(source, false)
			if !errors.Is(err, ErrUnattendedInsecurePath) {
				t.Fatalf("error = %v, want ErrUnattendedInsecurePath", err)
			}
			if !strings.Contains(err.Error(), planted) {
				t.Errorf("refusal %q does not name the planted symlink %s", err, planted)
			}

			// (a) the symlink itself is still a symlink — the install did not
			// quietly replace it — and (b) its TARGET was not mutated.
			info, lerr := os.Lstat(planted)
			if lerr != nil {
				t.Fatalf("lstat %s: %v", planted, lerr)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s is no longer a symlink; the install rewrote it", planted)
			}
			victimInfo, verr := os.Lstat(victim)
			if verr != nil {
				t.Fatalf("lstat victim: %v", verr)
			}
			if got := victimInfo.Mode().Perm(); got != 0o700 {
				t.Errorf("the symlink target's mode = %#o, want %#o — the install chmod'd through the link", got, 0o700)
			}
			if got := pathOwnerUID(t, victim); got != victimOwner {
				t.Errorf("the symlink target's owner = %d, want %d — the install chown'd through the link", got, victimOwner)
			}
			for _, path := range chowned {
				if path == victim || path == planted {
					t.Errorf("the install handed %s to chown despite refusing the tree (chowned = %v)", path, chowned)
				}
			}

			if len(*calls) != 0 {
				t.Errorf("a symlinked layout directory still reached launchctl: %v", *calls)
			}
			if _, statErr := os.Stat(layout.PlistPath); !os.IsNotExist(statErr) {
				t.Errorf("a refused install still wrote %s (stat err = %v)", layout.PlistPath, statErr)
			}
		})
	}
}

// TestPlatformInstallUnattendedToleratesASymlinkedVarAncestor is the guard on
// the log directory's asymmetric treatment.
//
// /var on macOS is a symlink to private/var. If the log directory were covered
// the way the plist and binary are — leaf plus a full ancestor walk — rule 3
// would find that symlink and refuse the install on every healthy host. This
// reproduces that exact shape inside the temp root and asserts the install
// SUCCEEDS, while TestVerifyWatchdogPathsRefusesWritableArtifacts keeps the
// directory itself genuinely checked.
func TestPlatformInstallUnattendedToleratesASymlinkedVarAncestor(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	stubRootEUID(t)

	// The real thing: /var -> private/var, with nothing at /var itself.
	if err := os.MkdirAll(filepath.Join(layout.Root, "private", "var"), 0o755); err != nil {
		t.Fatalf("create private/var: %v", err)
	}
	if err := os.Symlink("private/var", filepath.Join(layout.Root, "var")); err != nil {
		t.Fatalf("symlink var -> private/var: %v", err)
	}

	source := writeFakeCellarBossd(t, home, "watchdog build")
	if err := platformInstallUnattended(source, false); err != nil {
		t.Fatalf("a healthy host whose /var is a symlink to private/var was refused: %v", err)
	}
	if _, err := os.Stat(layout.PlistPath); err != nil {
		t.Fatalf("stat watchdog plist: %v", err)
	}
	if err := verifyWatchdogPaths(layout); err != nil {
		t.Fatalf("verifyWatchdogPaths on a symlinked-/var host: %v", err)
	}
	if got := observeUnattendedInstall(); got.State != UnattendedInstallPresent {
		t.Errorf("observeUnattendedInstall().State = %d, want UnattendedInstallPresent (err = %v)", got.State, got.Err)
	}
}

// TestBootoutWatchdogJobVerifiesADivergentExitCode is the regression for a
// helper that copy-pasted bootoutLaunchdService's exit-code knowledge (3 and
// 113 mean "already gone") but dropped its VERIFICATION.
//
// Exit 5 is a generic EIO launchd also returns when it has not finished tearing
// the job down. Treating it as a hard failure aborts platformUninstallUnattended
// before the plist, the binary directory and the logs are removed, which leaves
// the half-removed root job the teardown exists to prevent.
func TestBootoutWatchdogJobVerifiesADivergentExitCode(t *testing.T) {
	for _, tt := range []struct {
		name        string
		bootoutCode int
		stillLoaded bool
		wantErr     bool
	}{
		{name: "clean bootout", bootoutCode: 0},
		{name: "exit 3 means the job was never loaded", bootoutCode: 3},
		{name: "exit 113 means the service could not be found", bootoutCode: 113},
		{name: "exit 5 but the job is verifiably gone", bootoutCode: 5, stillLoaded: false},
		{name: "exit 5 and the job is still there", bootoutCode: 5, stillLoaded: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			layout, _ := func() (watchdogLayout, string) {
				t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
				return installTestWatchdog(t)
			}()

			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			originalRun := runLaunchctl
			runLaunchctl = func(args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "bootout" && tt.bootoutCode != 0 {
					return []byte("Boot-out failed"), fakeExitError(t, tt.bootoutCode)
				}
				return []byte(""), nil
			}
			t.Cleanup(func() { runLaunchctl = originalRun })

			originalProbe := watchdogJobStillLoaded
			watchdogJobStillLoaded = func() bool { return tt.stillLoaded }
			t.Cleanup(func() { watchdogJobStillLoaded = originalProbe })

			originalTimeout := bootoutVerifyTimeout
			bootoutVerifyTimeout = 50 * time.Millisecond
			t.Cleanup(func() { bootoutVerifyTimeout = originalTimeout })

			err := platformUninstallUnattended()
			if tt.wantErr {
				if err == nil {
					t.Fatal("a bootout that left the job loaded reported success")
				}
				return
			}
			if err != nil {
				t.Fatalf("platformUninstallUnattended: %v", err)
			}
			// The teardown has to have RUN, not merely not errored: a bootout
			// failure that aborts here is what leaves root-owned residue.
			for _, path := range []string{layout.PlistPath, layout.BinDir, layout.LogDir} {
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("teardown aborted and left root-owned residue at %s (stat err = %v)", path, statErr)
				}
			}
		})
	}
}

// TestWatchdogJobStillLoadedFailsClosed pins the probe's direction. Only an
// exit code that positively means "no such service" reads as gone; anything
// else is "cannot tell", which must never be reported as stopped.
func TestWatchdogJobStillLoadedFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name string
		code int
		want bool
	}{
		{name: "print succeeded, so the job is loaded", code: 0, want: true},
		{name: "no such process", code: 3, want: false},
		{name: "could not find service", code: 113, want: false},
		{name: "a generic EIO cannot tell, so assume loaded", code: 5, want: true},
		{name: "an unrecognised failure cannot tell either", code: 1, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := runLaunchctl
			runLaunchctl = func(...string) ([]byte, error) {
				if tt.code == 0 {
					return []byte("state = running"), nil
				}
				return []byte("failed"), fakeExitError(t, tt.code)
			}
			t.Cleanup(func() { runLaunchctl = original })

			if got := watchdogJobStillLoaded(); got != tt.want {
				t.Errorf("watchdogJobStillLoaded() = %v, want %v", got, tt.want)
			}
		})
	}
}

// captureResidueWarnings swaps the residue warning for a recorder and returns
// the slice it appends to.
func captureResidueWarnings(t *testing.T) *[]string {
	t.Helper()
	warned := &[]string{}
	original := warnUnattendedWatchdogResidue
	warnUnattendedWatchdogResidue = func(path string) { *warned = append(*warned, path) }
	t.Cleanup(func() { warnUnattendedWatchdogResidue = original })
	return warned
}

// TestPlatformInstallWarnsAboutARootWatchdogStillLoaded covers the sequence an
// operator performs to back the unattended mode out: install the watchdog, set
// 'daemon_supervision_mode' back to the default, run `boss daemon install`.
//
// Without a residue check that stages the per-user binary and bootstraps the
// gui/<uid> LaunchAgent while the root system/<label> job is STILL loaded with
// KeepAlive=true. Two supervisors then contend for one socket and nothing
// reports it — strictly worse than the teardown orphan the warning was written
// for, because install creates a duplicate rather than leaving one behind.
func TestPlatformInstallWarnsAboutARootWatchdogStillLoaded(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	layout, source := installTestWatchdog(t)
	// The operator's second step: the key goes back to the default while the
	// root job stays loaded.
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	mkdirLaunchAgents(t)
	stubNonRootEUID(t)
	warned := captureResidueWarnings(t)

	if err := platformInstall(source, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	if len(*warned) != 1 || (*warned)[0] != layout.PlistPath {
		t.Fatalf("residue warnings = %v, want one naming %s", *warned, layout.PlistPath)
	}
	// The LaunchAgent is still installed — the warning reports, it does not
	// block the command.
	agentPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if _, statErr := os.Stat(agentPath); statErr != nil {
		t.Errorf("the warning blocked the LaunchAgent install: %v", statErr)
	}
	if _, statErr := os.Stat(layout.PlistPath); statErr != nil {
		t.Errorf("the warning path removed the root-owned plist: %v", statErr)
	}
}

// TestPlatformRestartWarnsAboutARootWatchdogStillLoaded is the same shape for
// restart, which re-bootstraps the gui/<uid> LaunchAgent and so creates the
// same duplicate-supervisor state.
func TestPlatformRestartWarnsAboutARootWatchdogStillLoaded(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	layout, source := installTestWatchdog(t)
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	mkdirLaunchAgents(t)
	stubNonRootEUID(t)
	stubExecutableNextTo(t, source)
	if err := platformInstall(source, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}

	warned := captureResidueWarnings(t)
	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart: %v", err)
	}
	if len(*warned) != 1 || (*warned)[0] != layout.PlistPath {
		t.Fatalf("residue warnings = %v, want one naming %s", *warned, layout.PlistPath)
	}
}

// TestPlatformInstallAndRestartDoNotWarnOnACleanHost keeps the two new call
// sites from becoming noise every operator learns to ignore.
func TestPlatformInstallAndRestartDoNotWarnOnACleanHost(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }

	home := t.TempDir()
	t.Setenv("HOME", home)
	useTempWatchdogRoot(t)
	mkdirLaunchAgents(t)
	stubNonRootEUID(t)
	source := writeFakeCellarBossd(t, home, "version one")
	stubExecutableNextTo(t, source)
	warned := captureResidueWarnings(t)

	if err := platformInstall(source, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart: %v", err)
	}
	if len(*warned) != 0 {
		t.Fatalf("residue warnings = %v on a host with no watchdog, want none", *warned)
	}
}

// TestPlatformInstallUnattendedRefusesAPrePlantedWritableLayoutDirectory pins
// the PRE-create half of the directory gate on its own.
//
// The two halves — verifyWatchdogDirsBeforeCreate and mkdirRootOwned's own
// symlink check — were mutually redundant for the one hostile shape the suite
// exercised (a planted symlink), so removing either alone left the suite green
// and neither guard's individual necessity was pinned. This is the shape only
// the pre-create pass catches.
//
// The layout directory already exists, is a REAL directory (not a symlink), is
// owned by the trusted uid, and is group- and world-writable. mkdirRootOwned
// does not refuse it: os.MkdirAll is a no-op on an existing directory and the
// chmod that follows silently REPAIRS the mode to 0755. The post-write
// verification then sees a well-owned 0755 directory and passes too. So without
// the pre-create pass the install adopts a directory that was writable by
// everyone right up until the moment we chmod'd it — long enough for anything
// to have been planted inside it, which is the point of refusing rather than
// repairing.
func TestPlatformInstallUnattendedRefusesAPrePlantedWritableLayoutDirectory(t *testing.T) {
	for _, tt := range []struct {
		name string
		pick func(watchdogLayout) string
	}{
		{"the watchdog binary directory", func(l watchdogLayout) string { return l.BinDir }},
		{"the watchdog log directory", func(l watchdogLayout) string { return l.LogDir }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			layout := useTempWatchdogRoot(t)
			stubRootEUID(t)
			calls := recordLaunchctl(t)
			source := writeFakeCellarBossd(t, home, "watchdog build")

			planted := tt.pick(layout)
			if err := os.MkdirAll(planted, 0o777); err != nil {
				t.Fatalf("create the planted directory: %v", err)
			}
			// MkdirAll honours the umask, so set the hostile mode explicitly.
			if err := os.Chmod(planted, 0o777); err != nil {
				t.Fatalf("chmod the planted directory: %v", err)
			}

			err := platformInstallUnattended(source, false)
			if !errors.Is(err, ErrUnattendedInsecurePath) {
				t.Fatalf("error = %v, want ErrUnattendedInsecurePath", err)
			}
			if !strings.Contains(err.Error(), planted) {
				t.Errorf("refusal %q does not name the offending path %s", err, planted)
			}
			// The refusal must land before the mode is quietly repaired —
			// otherwise the evidence that anything was ever wrong is gone.
			info, statErr := os.Lstat(planted)
			if statErr != nil {
				t.Fatalf("lstat the planted directory: %v", statErr)
			}
			if got := info.Mode().Perm(); got != 0o777 {
				t.Errorf("planted directory mode = %#o, want it left at 0777 — the install repaired it instead of refusing", got)
			}
			if _, statErr := os.Stat(layout.PlistPath); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a refused install still wrote the plist: stat err = %v", statErr)
			}
			if len(*calls) != 0 {
				t.Errorf("a refused install reached launchctl: %v", *calls)
			}
		})
	}
}

// TestMkdirRootOwnedRefusesASymlink pins the OTHER half on its own.
//
// verifyWatchdogDirsBeforeCreate cannot stand in for this one. It runs once,
// before any directory is created, so it can only refuse what was already
// planted at that instant; a link planted after it returns and before the mkdir
// would sail past it. mkdirRootOwned's own Lstat is what narrows that window,
// and it has to refuse rather than proceed because neither os.Chmod nor
// os.Chown is symlink-safe — chmod 0755 / chown root:wheel through a link
// rewrites whatever it points at.
func TestMkdirRootOwnedRefusesASymlink(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if err := os.Chmod(victim, 0o700); err != nil {
		t.Fatalf("chmod victim: %v", err)
	}

	planted := filepath.Join(root, "planted")
	if err := os.Symlink(victim, planted); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	var chowned []string
	original := chownToRoot
	chownToRoot = func(path string) error {
		chowned = append(chowned, path)
		return nil
	}
	t.Cleanup(func() { chownToRoot = original })

	err := mkdirRootOwned(planted, 0o755)
	if !errors.Is(err, ErrUnattendedInsecurePath) {
		t.Fatalf("error = %v, want ErrUnattendedInsecurePath", err)
	}
	if !strings.Contains(err.Error(), planted) {
		t.Errorf("refusal %q does not name the symlink %s", err, planted)
	}
	// The victim must be untouched: this is the escalation itself, not a
	// hygiene point.
	info, statErr := os.Lstat(victim)
	if statErr != nil {
		t.Fatalf("lstat victim: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("victim mode = %#o, want 0700 — chmod went through the symlink", got)
	}
	if len(chowned) != 0 {
		t.Errorf("chown ran on %v — it went through the symlink", chowned)
	}
}

// supersededLaunchAgentPath is where the unattended install must look for the
// agent it replaces: under the TARGET user's home, which useTempWatchdogRoot
// stubs to a directory inside the temp root.
//
// It is deliberately not platformServicePath(). That helper reads
// os.UserHomeDir(), so in these tests it resolves under $HOME and in production
// under `sudo` it resolves under /var/root — and the two differing here is
// exactly the trap the assertions below exist to catch.
func supersededLaunchAgentPath(t *testing.T, uid int) string {
	t.Helper()
	_, home, err := lookupUserByUID(uid)
	if err != nil {
		t.Fatalf("lookupUserByUID: %v", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// writeSupersededLaunchAgent plants the per-user LaunchAgent plist the
// unattended substrate is supposed to supersede.
func writeSupersededLaunchAgent(t *testing.T, uid int) string {
	t.Helper()
	path := supersededLaunchAgentPath(t, uid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create target LaunchAgents dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("write superseded LaunchAgent: %v", err)
	}
	return path
}

// retargetUnattendedUser points SUDO_UID at a uid that is guaranteed NOT to be
// this process's own.
//
// Without it the "the bootout carries the target's uid" assertion is vacuous on
// any developer machine whose uid happens to be 501 — which on macOS is the
// first admin account, i.e. most of them. The value below can never collide.
func retargetUnattendedUser(t *testing.T, root string) int {
	t.Helper()
	uid := os.Getuid() + 4242
	t.Setenv("SUDO_UID", strconv.Itoa(uid))
	original := lookupUserByUID
	lookupUserByUID = func(int) (string, string, error) {
		return watchdogTestUser, filepath.Join(root, "Users", watchdogTestUser), nil
	}
	t.Cleanup(func() { lookupUserByUID = original })
	return uid
}

// captureSupersessionWarnings observes the BOS-1203 supersession warning.
func captureSupersessionWarnings(t *testing.T) *[]string {
	t.Helper()
	warned := &[]string{}
	original := warnLaunchAgentNotSuperseded
	warnLaunchAgentNotSuperseded = func(subject, reason string) {
		*warned = append(*warned, subject+": "+reason)
	}
	t.Cleanup(func() { warnLaunchAgentNotSuperseded = original })
	return warned
}

// TestPlatformInstallUnattendedSupersedesThePerUserLaunchAgent closes the
// BOS-1203 window at its source.
//
// Nothing removed the LaunchAgent before, so it stayed on disk with RunAtLoad
// and was bootstrapped again at the target user's next login — a gui/<uid>
// supervisor beside a root job with KeepAlive=true, permanently.
//
// Both assertions guard a specific wrong implementation. The removal is checked
// at the TARGET's home rather than at platformServicePath(), which under `sudo`
// is root's home. And the bootout is checked to carry the TARGET's uid rather
// than the calling process's, which bootoutLaunchdService would have supplied
// as 0 under `sudo` — booting out a job that does not exist, reporting success,
// and leaving the operator's own agent loaded.
func TestPlatformInstallUnattendedSupersedesThePerUserLaunchAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	uid := retargetUnattendedUser(t, layout.Root)
	stubRootEUID(t)
	calls := recordLaunchctl(t)
	warned := captureSupersessionWarnings(t)

	agentPath := writeSupersededLaunchAgent(t, uid)
	// The plist under $HOME must survive: it is the file platformServicePath()
	// resolves, and touching it would mean the step keyed on the wrong home.
	ownPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(ownPath), 0o700); err != nil {
		t.Fatalf("create own LaunchAgents dir: %v", err)
	}
	if err := os.WriteFile(ownPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("write own LaunchAgent: %v", err)
	}

	source := writeFakeCellarBossd(t, home, "watchdog build")
	if err := platformInstallUnattended(source, false); err != nil {
		t.Fatalf("platformInstallUnattended: %v", err)
	}

	if _, err := os.Stat(agentPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the target user's LaunchAgent survived the install at %s (stat err = %v)", agentPath, err)
	}
	if agentPath == ownPath {
		t.Fatal("the target home and the process home coincide, so this test cannot discriminate")
	}
	if _, err := os.Stat(ownPath); err != nil {
		t.Errorf("the install removed a plist under the CALLING process's home (%s): %v — it keyed on os.UserHomeDir(), which is root's home under sudo", ownPath, err)
	}

	wantTarget := "gui/" + strconv.Itoa(uid) + "/" + Label
	bootouts := 0
	for _, call := range *calls {
		if call[0] != "bootout" {
			continue
		}
		bootouts++
		if len(call) < 2 || call[1] != wantTarget {
			t.Errorf("bootout target = %v, want %q — the domain must carry the TARGET's uid, not the caller's", call, wantTarget)
		}
	}
	if bootouts != 1 {
		t.Errorf("bootout invocations = %d, want exactly 1", bootouts)
	}
	if len(*warned) != 0 {
		t.Errorf("supersession warnings = %v, want none on the happy path", *warned)
	}
}

// TestPlatformInstallUnattendedRemovesTheLaunchAgentWithoutLaunchctl pins the
// split around the skipLaunchctl gate.
//
// The file removal sits ABOVE that gate and the bootout below it, matching how
// every other file write in platformInstallUnattended is ordered. Below the
// gate the removal would be unreachable through this package's own test seam,
// which is the only way the step is exercised at all.
func TestPlatformInstallUnattendedRemovesTheLaunchAgentWithoutLaunchctl(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	layout := useTempWatchdogRoot(t)
	uid := retargetUnattendedUser(t, layout.Root)
	stubRootEUID(t)

	var calls [][]string
	originalRun := runLaunchctl
	runLaunchctl = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}
	t.Cleanup(func() { runLaunchctl = originalRun })

	agentPath := writeSupersededLaunchAgent(t, uid)
	source := writeFakeCellarBossd(t, home, "watchdog build")
	if err := platformInstallUnattended(source, false); err != nil {
		t.Fatalf("platformInstallUnattended: %v", err)
	}
	if _, err := os.Stat(agentPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the LaunchAgent survived under BOSS_DAEMON_SKIP_LAUNCHCTL (stat err = %v) — the removal is below the gate", err)
	}
	if len(calls) != 0 {
		t.Errorf("launchctl calls = %v, want none under BOSS_DAEMON_SKIP_LAUNCHCTL", calls)
	}
}

// TestPlatformInstallUnattendedSupersessionNeverFailsTheInstall is R8's other
// half: every way the step can go wrong leaves the install succeeding and fires
// a warning that names what was left behind.
//
// Failing the install to report a leftover file would be strictly worse than
// completing it and reporting the leftover file — by this point the watchdog is
// written, verified and bootstrapped, and it is the thing that actually keeps
// the host supervised.
func TestPlatformInstallUnattendedSupersessionNeverFailsTheInstall(t *testing.T) {
	t.Run("a bootout error leaves the install succeeding", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		layout := useTempWatchdogRoot(t)
		uid := retargetUnattendedUser(t, layout.Root)
		stubRootEUID(t)
		warned := captureSupersessionWarnings(t)
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

		originalRun := runLaunchctl
		runLaunchctl = func(args ...string) ([]byte, error) {
			if args[0] == "bootout" {
				// Exit 5 is a generic failure, NOT one of the "already gone"
				// codes, so bootoutLaunchdTarget's nil-probe rung fails closed.
				return []byte("Bootout failed"), fakeExitError(t, 5)
			}
			return nil, nil
		}
		t.Cleanup(func() { runLaunchctl = originalRun })

		writeSupersededLaunchAgent(t, uid)
		source := writeFakeCellarBossd(t, home, "watchdog build")
		if err := platformInstallUnattended(source, false); err != nil {
			t.Fatalf("a bootout failure aborted the install: %v", err)
		}
		if len(*warned) != 1 {
			t.Fatalf("supersession warnings = %v, want exactly one", *warned)
		}
		if !strings.Contains((*warned)[0], "gui/"+strconv.Itoa(uid)+"/"+Label) {
			t.Errorf("warning %q does not name the job that may still be loaded", (*warned)[0])
		}
	})

	t.Run("a symlinked LaunchAgents parent is refused rather than followed", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		home := t.TempDir()
		t.Setenv("HOME", home)
		layout := useTempWatchdogRoot(t)
		uid := retargetUnattendedUser(t, layout.Root)
		stubRootEUID(t)
		warned := captureSupersessionWarnings(t)

		agentPath := supersededLaunchAgentPath(t, uid)
		launchAgentsDir := filepath.Dir(agentPath)
		// Repoint LaunchAgents at a directory of the invoking user's choosing —
		// they own the whole chain and could have done this before running
		// `sudo`. os.Remove follows the link, so this is the same class
		// mkdirRootOwned refuses at length.
		elsewhere := t.TempDir()
		decoy := filepath.Join(elsewhere, Label+".plist")
		if err := os.WriteFile(decoy, []byte("<plist/>"), 0o600); err != nil {
			t.Fatalf("write decoy plist: %v", err)
		}
		if err := os.RemoveAll(launchAgentsDir); err != nil {
			t.Fatalf("remove real LaunchAgents dir: %v", err)
		}
		if err := os.Symlink(elsewhere, launchAgentsDir); err != nil {
			t.Fatalf("symlink LaunchAgents: %v", err)
		}

		source := writeFakeCellarBossd(t, home, "watchdog build")
		if err := platformInstallUnattended(source, false); err != nil {
			t.Fatalf("a symlinked parent aborted the install: %v", err)
		}
		if _, err := os.Stat(decoy); err != nil {
			t.Errorf("the install unlinked THROUGH the symlink and removed %s: %v", decoy, err)
		}
		if len(*warned) != 1 {
			t.Fatalf("supersession warnings = %v, want exactly one", *warned)
		}
		if !strings.Contains((*warned)[0], "symlink") {
			t.Errorf("warning %q does not say why the plist was left alone", (*warned)[0])
		}
	})

	t.Run("an absent home cannot verify and says so", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		home := t.TempDir()
		t.Setenv("HOME", home)
		layout := useTempWatchdogRoot(t)
		retargetUnattendedUser(t, layout.Root)
		stubRootEUID(t)
		warned := captureSupersessionWarnings(t)

		// An unmounted or network home: the whole Library tree is missing.
		if err := os.RemoveAll(filepath.Join(layout.Root, "Users", watchdogTestUser)); err != nil {
			t.Fatalf("remove target home: %v", err)
		}

		source := writeFakeCellarBossd(t, home, "watchdog build")
		if err := platformInstallUnattended(source, false); err != nil {
			t.Fatalf("an absent home aborted the install: %v", err)
		}
		if len(*warned) != 1 {
			t.Fatalf("supersession warnings = %v, want exactly one", *warned)
		}
		// "could not be established", not silence: reporting a clean
		// supersession for a home that was never readable would claim an
		// observation nobody made.
		if !strings.Contains((*warned)[0], "could not be established") {
			t.Errorf("warning %q reports a fault rather than an unverifiable state", (*warned)[0])
		}
	})

	t.Run("no LaunchAgent at all is silent", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		home := t.TempDir()
		t.Setenv("HOME", home)
		layout := useTempWatchdogRoot(t)
		retargetUnattendedUser(t, layout.Root)
		stubRootEUID(t)
		warned := captureSupersessionWarnings(t)

		source := writeFakeCellarBossd(t, home, "watchdog build")
		if err := platformInstallUnattended(source, false); err != nil {
			t.Fatalf("platformInstallUnattended: %v", err)
		}
		// This is the ordinary run AND the `--force` reinstall path. A warning
		// here would be noise on the commonest install of all.
		if len(*warned) != 0 {
			t.Errorf("supersession warnings = %v, want none when there is no LaunchAgent to supersede", *warned)
		}
	})
}

// captureUnattendedRemovalNotices observes the closing line of a successful
// unattended uninstall.
func captureUnattendedRemovalNotices(t *testing.T) *int {
	t.Helper()
	count := new(int)
	original := noticeUnattendedSupervisionRemoved
	noticeUnattendedSupervisionRemoved = func() { *count++ }
	t.Cleanup(func() { noticeUnattendedSupervisionRemoved = original })
	return count
}

// TestPlatformInstallRemovesAStrandedWatchdogWhenRoot is the mirror image of
// TestPlatformUninstallRemovesStrandedWatchdogWhenRoot, and the asymmetry it
// closes is the one that made the back-out sequence permanent.
//
// platformUninstall has removed a stranded watchdog under root since BOS-1184;
// platformInstall only warned. So an operator who set the key back to
// launch-agent and ran `sudo boss daemon install` got a freshly bootstrapped
// gui agent BESIDE a root job still loaded with KeepAlive=true — two
// supervisors for one socket, surviving every reboot, with the warning naming a
// remedy that was a separate command they had to notice and run.
func TestPlatformInstallRemovesAStrandedWatchdogWhenRoot(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	layout, source := installTestWatchdog(t)
	// The operator's second step: the key goes back to the default while the
	// root job stays loaded.
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	mkdirLaunchAgents(t)
	stubRootEUID(t)
	warned := captureResidueWarnings(t)
	notices := captureUnattendedRemovalNotices(t)

	if err := platformInstall(source, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	if _, err := os.Stat(layout.PlistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stranded watchdog plist survived a root install: stat err = %v, want ErrNotExist", err)
	}
	if _, err := os.Stat(layout.BinDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stranded watchdog binary dir survived a root install: stat err = %v, want ErrNotExist", err)
	}
	// The install still does its own job.
	agentPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if _, statErr := os.Stat(agentPath); statErr != nil {
		t.Errorf("removing the watchdog blocked the LaunchAgent install: %v", statErr)
	}
	// Having removed it there is no residue left to warn about — the warning
	// and the removal are alternatives, not both.
	if len(*warned) != 0 {
		t.Errorf("residue warnings = %v, want none once the job was removed", *warned)
	}
	// And the uninstall's closing notice must NOT fire here: this command is
	// replacing the watchdog with a LaunchAgent, so telling the operator the
	// host has no supervisor and to run `boss daemon install` would contradict
	// the command they just ran.
	if *notices != 0 {
		t.Errorf("unattended-removal notices = %d during an install, want 0", *notices)
	}
}

// TestPlatformUninstallUnattendedAnnouncesTheHostHasNoSupervisor pins R8 on
// teardown.
//
// Before supersession existed, `sudo boss daemon uninstall` left the per-user
// LaunchAgent behind and the host kept working. Now the install removes that
// agent, so the same command leaves a machine with NO supervisor — a change an
// operator would otherwise discover at the next reboot.
func TestPlatformUninstallUnattendedAnnouncesTheHostHasNoSupervisor(t *testing.T) {
	t.Run("success announces it once", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		installTestWatchdog(t)
		notices := captureUnattendedRemovalNotices(t)

		if err := platformUninstallUnattended(); err != nil {
			t.Fatalf("platformUninstallUnattended: %v", err)
		}
		if *notices != 1 {
			t.Errorf("unattended-removal notices = %d, want exactly 1", *notices)
		}
	})

	t.Run("a refused teardown announces nothing", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		installTestWatchdog(t)
		// Without root the teardown refuses before removing anything, so
		// claiming the supervisor is gone would be false.
		stubNonRootEUID(t)
		notices := captureUnattendedRemovalNotices(t)

		if err := platformUninstallUnattended(); !errors.Is(err, ErrUnattendedRequiresRoot) {
			t.Fatalf("error = %v, want one wrapping ErrUnattendedRequiresRoot", err)
		}
		if *notices != 0 {
			t.Errorf("unattended-removal notices = %d on a refused teardown, want 0", *notices)
		}
	})
}
