//go:build darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
)

func writeFakeCellarBossd(t *testing.T, home, contents string) string {
	t.Helper()
	path := filepath.Join(home, "homebrew", "Cellar", "bossanova", "1.2.3", "bin", "bossd")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create fake Cellar bin dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake Cellar bossd: %v", err)
	}
	return path
}

func expectedStagedBossdPath(t *testing.T) string {
	t.Helper()
	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir: %v", err)
	}
	return daemonbin.StagedPath(appDataDir)
}

// stubExecutableNextTo points executablePath at a sibling `boss` of sourcePath,
// which is what makes ResolveBossdPath resolve sourcePath inside a hermetic
// sandbox instead of falling through to the host's PATH.
func stubExecutableNextTo(t *testing.T, sourcePath string) {
	t.Helper()
	original := executablePath
	executablePath = func() (string, error) {
		return filepath.Join(filepath.Dir(sourcePath), "boss"), nil
	}
	t.Cleanup(func() { executablePath = original })
}

func TestPlatformInstallPointsPlistAtStagedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	sourcePath := writeFakeCellarBossd(t, home, "version one")

	if err := platformInstall(sourcePath, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	stagedPath := expectedStagedBossdPath(t)
	if !strings.Contains(string(plist), "<string>"+stagedPath+"</string>") {
		t.Errorf("plist does not point at staged path %q:\n%s", stagedPath, plist)
	}
	if strings.Contains(string(plist), "/Cellar/") {
		t.Errorf("plist still contains versioned Cellar path:\n%s", plist)
	}
}

func TestPlatformInstallStagesTheBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	sourceContents := "version one"
	sourcePath := writeFakeCellarBossd(t, home, sourceContents)

	if err := platformInstall(sourcePath, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	stagedPath := expectedStagedBossdPath(t)
	stagedContents, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged bossd: %v", err)
	}
	if string(stagedContents) != sourceContents {
		t.Errorf("staged bossd contents = %q, want %q", stagedContents, sourceContents)
	}
	info, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("stat staged bossd: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("staged bossd mode = %#o, want 0755", got)
	}
}

func TestPlatformInstallWithoutForceDoesNotMutateStagedBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	stagedPath := expectedStagedBossdPath(t)
	if err := os.MkdirAll(filepath.Dir(stagedPath), 0o700); err != nil {
		t.Fatalf("create staged bin dir: %v", err)
	}
	originalContents := []byte("currently installed version")
	if err := os.WriteFile(stagedPath, originalContents, 0o755); err != nil {
		t.Fatalf("write existing staged bossd: %v", err)
	}

	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("create LaunchAgents dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("existing plist"), 0o600); err != nil {
		t.Fatalf("write existing plist: %v", err)
	}

	sourcePath := writeFakeCellarBossd(t, home, "new version must not be staged")
	err = platformInstall(sourcePath, false)
	if err == nil || !strings.Contains(err.Error(), "plist already exists") {
		t.Fatalf("platformInstall error = %v, want existing-plist refusal", err)
	}
	gotContents, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged bossd after refused install: %v", err)
	}
	if string(gotContents) != string(originalContents) {
		t.Errorf("staged bossd changed on refused install: got %q, want %q", gotContents, originalContents)
	}
}

func TestPlatformRestartRestagesAndRewritesPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	sourcePath := writeFakeCellarBossd(t, home, "version one")

	if err := platformInstall(sourcePath, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("version two"), 0o755); err != nil {
		t.Fatalf("update source bossd: %v", err)
	}
	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	legacyPlist, err := generatePlist(sourcePath)
	if err != nil {
		t.Fatalf("generate legacy plist: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(legacyPlist), 0o600); err != nil {
		t.Fatalf("write legacy plist: %v", err)
	}

	stubExecutableNextTo(t, sourcePath)

	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart: %v", err)
	}
	stagedPath := expectedStagedBossdPath(t)
	stagedContents, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read restaged bossd: %v", err)
	}
	if got, want := string(stagedContents), "version two"; got != want {
		t.Errorf("restaged bossd contents = %q, want %q", got, want)
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read rewritten plist: %v", err)
	}
	if !strings.Contains(string(plist), "<string>"+stagedPath+"</string>") {
		t.Errorf("rewritten plist does not point at staged path %q:\n%s", stagedPath, plist)
	}
	if strings.Contains(string(plist), "/Cellar/") {
		t.Errorf("rewritten plist still contains versioned Cellar path:\n%s", plist)
	}
}

func TestPlatformEnsureRunningStagesFallbackDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	sourcePath := writeFakeCellarBossd(t, home, "fallback version")
	// Unix-domain sockets have a short path limit; t.TempDir() paths exceed it.
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("bossd-%d.sock", time.Now().UnixNano()))

	originalExecutablePath := executablePath
	originalStartDetachedBossd := startDetachedBossd
	executablePath = func() (string, error) {
		return filepath.Join(filepath.Dir(sourcePath), "boss"), nil
	}
	t.Cleanup(func() {
		executablePath = originalExecutablePath
		startDetachedBossd = originalStartDetachedBossd
	})

	var startedPath string
	var listener net.Listener
	startDetachedBossd = func(path string) error {
		startedPath = path
		var err error
		listener, err = net.Listen("unix", socketPath)
		return err
	}
	t.Cleanup(func() {
		if listener != nil {
			_ = listener.Close()
		}
	})

	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	// BOS-1183: this is the unsupervised path, and the caller can only tell
	// because of this verdict — the error is nil either way.
	if mode != StartModeDetached {
		t.Errorf("StartMode = %v, want %v for the direct-spawn fallback", mode, StartModeDetached)
	}
	if got, want := startedPath, expectedStagedBossdPath(t); got != want {
		t.Errorf("fallback started %q, want stable staged path %q", got, want)
	}
	if got, err := os.ReadFile(expectedStagedBossdPath(t)); err != nil || string(got) != "fallback version" {
		t.Errorf("staged fallback contents = %q, err = %v", got, err)
	}
}

// prepareEnsureRunningEnvironment installs a LaunchAgent whose plist names the
// staged copy and leaves the daemon "installed but not running" — the state a
// post-upgrade `boss daemon start` finds, and the only state in which
// platformEnsureRunning takes its LaunchAgent branch.
//
// platformGetStatus reports Installed && !Running when the plist exists and
// BOSS_DAEMON_SKIP_LAUNCHCTL is set. That skip does NOT cover
// platformEnsureRunning's own `load`, so the load is still recorded.
//
// The installed build is always "version one"; a caller that needs the upgrade
// case overwrites the returned sourcePath afterwards, which is what makes the
// source-newer-than-staged state the point of the test rather than setup.
func prepareEnsureRunningEnvironment(t *testing.T) (sourcePath, plistPath, socketPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	// BOS-1203: pin BOTH facts platformEnsureRunning now routes on, so no
	// verdict below can be a property of the developer's machine.
	//
	// Neither was pinned before and both were live exposures rather than
	// theoretical ones. loadServiceSettings survived only because
	// t.Setenv("HOME", …) incidentally redirects config.Path() — protection that
	// is void the moment BOSS_SETTINGS_PATH is set in the environment, which is
	// a supported way to run a second daemon. And watchdogFilesystemRoot was
	// still "/", so on a machine that genuinely has the unattended watchdog
	// installed every test in this family would take the watchdog route and buy
	// a real wait, silently proving something other than what it says.
	stubDefaultServiceSettings(t)
	useTempWatchdogRoot(t)

	sourcePath = writeFakeCellarBossd(t, home, "version one")
	stubExecutableNextTo(t, sourcePath)

	if err := platformInstall(sourcePath, false); err != nil {
		t.Fatalf("platformInstall: %v", err)
	}
	var err error
	if plistPath, err = platformServicePath(); err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	// Unix-domain sockets have a short path limit; t.TempDir() paths exceed it.
	socketPath = filepath.Join(os.TempDir(), fmt.Sprintf("bossd-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	return sourcePath, plistPath, socketPath
}

// stubLoadServesSocket installs a runLaunchctl fake whose `load` starts serving
// socketPath, so platformEnsureRunning's waitForSocket returns at once instead
// of burning LifecycleStartupTimeout. onLoad observes the world at the instant
// the load is issued, which is what makes an ordering assertion possible.
func stubLoadServesSocket(t *testing.T, socketPath string, onLoad func()) *[][]string {
	t.Helper()
	return stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		if args[0] != "load" {
			return nil, nil
		}
		if onLoad != nil {
			onLoad()
		}
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Errorf("serve fake daemon socket: %v", err)
			return nil, err
		}
		t.Cleanup(func() { _ = listener.Close() })
		return nil, nil
	})
}

// TestPlatformEnsureRunningStagesBeforeLoadingTheLaunchAgent is the BOS-977
// regression. The plist names the STAGED copy, so a start that hands that plist
// to launchctl before refreshing the copy brings up the previous build and
// reports success — which is why every `brew upgrade` needed a manual
// `boss daemon restart`.
//
// The assertion is deliberately about order, not occurrence: it reads the
// staged file at the instant the load is issued. A "both happened" assertion
// passes against the broken ordering this ticket exists to fix.
func TestPlatformEnsureRunningStagesBeforeLoadingTheLaunchAgent(t *testing.T) {
	sourcePath, _, socketPath := prepareEnsureRunningEnvironment(t)

	// The upgrade: the installed source moves ahead of the staged copy.
	if err := os.WriteFile(sourcePath, []byte("version two"), 0o755); err != nil {
		t.Fatalf("upgrade source bossd: %v", err)
	}

	stagedPath := expectedStagedBossdPath(t)
	var stagedAtLoad string
	calls := stubLoadServesSocket(t, socketPath, func() {
		contents, err := os.ReadFile(stagedPath)
		if err != nil {
			t.Errorf("read staged bossd at load time: %v", err)
			return
		}
		stagedAtLoad = string(contents)
	})

	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeServiceManager {
		t.Errorf("StartMode = %v, want %v when the LaunchAgent served the socket", mode, StartModeServiceManager)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 1 {
		t.Fatalf("`launchctl load` invocations = %d, want 1", got)
	}
	if want := "version two"; stagedAtLoad != want {
		t.Errorf("staged bossd when `launchctl load` was issued = %q, want the upgraded %q", stagedAtLoad, want)
	}
}

// TestPlatformEnsureRunningDoesNotRestageACurrentBinary keeps the steady-state
// cost of the fix a daemonbin.NeedsStage digest comparison rather than a 38 MB
// copy on every cold start. That comparison is not free — NeedsStage reads both
// files to SHA-256 them — so this pins the copy away, not the read.
func TestPlatformEnsureRunningDoesNotRestageACurrentBinary(t *testing.T) {
	_, _, socketPath := prepareEnsureRunningEnvironment(t)

	stagedPath := expectedStagedBossdPath(t)
	before, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("stat staged bossd before start: %v", err)
	}

	calls := stubLoadServesSocket(t, socketPath, nil)
	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeServiceManager {
		t.Errorf("StartMode = %v, want %v when the LaunchAgent served the socket", mode, StartModeServiceManager)
	}

	after, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("stat staged bossd after start: %v", err)
	}
	// daemonbin.Stage renames a fresh temp file into place, so a re-copy is a
	// new inode. os.SameFile compares device+inode, which content equality
	// (identical bytes either way) could never distinguish.
	if !os.SameFile(before, after) {
		t.Error("staged bossd was re-copied although it already matched the source")
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 1 {
		t.Fatalf("`launchctl load` invocations = %d, want 1 — the load must still happen", got)
	}
}

// TestPlatformEnsureRunningLoadsAnywayWhenStagingFails pins the no-regression
// half of the fix. Before BOS-977 this path started the old binary; a staging
// failure must therefore degrade back to exactly that, never to no start at all.
func TestPlatformEnsureRunningLoadsAnywayWhenStagingFails(t *testing.T) {
	_, _, socketPath := prepareEnsureRunningEnvironment(t)

	// Break staging without inventing a seam: replace the staged bin DIRECTORY
	// with a regular file, so daemonbin.NeedsStage's lstat of <bin>/bossd fails
	// with ENOTDIR. That is the shape a corrupted app-data dir actually takes.
	binDir := filepath.Dir(expectedStagedBossdPath(t))
	if err := os.RemoveAll(binDir); err != nil {
		t.Fatalf("remove staged bin dir: %v", err)
	}
	if err := os.WriteFile(binDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write file over staged bin dir: %v", err)
	}

	var warned error
	originalWarn := warnDaemonRefreshFailed
	warnDaemonRefreshFailed = func(err error) { warned = err }
	t.Cleanup(func() { warnDaemonRefreshFailed = originalWarn })

	calls := stubLoadServesSocket(t, socketPath, nil)
	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("a staging failure turned a working start into a hard failure: %v", err)
	}
	if mode != StartModeServiceManager {
		t.Errorf("StartMode = %v, want %v — a staging failure must not downgrade a supervised start", mode, StartModeServiceManager)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 1 {
		t.Fatalf("`launchctl load` invocations = %d, want 1 — a staging failure must not stop the start", got)
	}
	if warned == nil {
		t.Fatal("staging failure was swallowed; the operator is never told why the start used the old build")
	}
	// "staged" alone would also match the plist-refresh errors, which name the
	// staged path; this pins the reason to the staging check that really failed.
	if !strings.Contains(warned.Error(), "check staged bossd") {
		t.Errorf("surfaced reason %q does not name the staging failure", warned.Error())
	}
}

// TestPlatformEnsureRunningReusesTheRefreshedStagingForTheFallback pins that a
// successful pre-load refresh is not thrown away when the `launchctl load`
// fails and the direct-spawn fallback takes over. daemonbin.NeedsStage
// short-circuits on a SHA-256 of both the source and the staged copy, so
// resolving and staging a second time re-hashes ~38 MB twice rather than
// re-running a stat.
//
// The discriminator is deliberately destructive: at the instant the load is
// issued, the resolved source is replaced with a DIRECTORY. ResolveBossdPath
// only stats its candidate, so it still resolves, but NeedsStage rejects a
// non-regular source — meaning a second EnsureStaged fails outright and the
// old shape returns a hard error instead of starting the copy it had already
// staged. A "both paths work" assertion could not tell the two apart.
func TestPlatformEnsureRunningReusesTheRefreshedStagingForTheFallback(t *testing.T) {
	sourcePath, _, socketPath := prepareEnsureRunningEnvironment(t)

	// Upgrade the source so the pre-load refresh definitely stages something.
	if err := os.WriteFile(sourcePath, []byte("version two"), 0o755); err != nil {
		t.Fatalf("upgrade source bossd: %v", err)
	}

	originalStartDetachedBossd := startDetachedBossd
	var startedPath string
	var listener net.Listener
	startDetachedBossd = func(path string) error {
		startedPath = path
		var err error
		listener, err = net.Listen("unix", socketPath)
		return err
	}
	t.Cleanup(func() {
		startDetachedBossd = originalStartDetachedBossd
		if listener != nil {
			_ = listener.Close()
		}
	})

	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		if args[0] != "load" {
			return nil, nil
		}
		if err := os.Remove(sourcePath); err != nil {
			t.Errorf("remove source bossd: %v", err)
			return nil, err
		}
		if err := os.Mkdir(sourcePath, 0o700); err != nil {
			t.Errorf("replace source bossd with a directory: %v", err)
			return nil, err
		}
		return []byte("Load failed"), fakeExitError(t, 1)
	})

	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("fallback re-resolved and re-staged instead of reusing the refresh it had just done: %v", err)
	}
	if mode != StartModeDetached {
		t.Errorf("StartMode = %v, want %v — a failed load falls through to the unsupervised spawn", mode, StartModeDetached)
	}
	// The whole discriminating power of this test lives in the stub's `load`
	// branch, which is where the source is sabotaged. If the LaunchAgent branch
	// ever stops being entered, that branch never runs, the plain fallback
	// stages "version two" for itself, and every assertion below still holds —
	// so pin the load, exactly as this test's siblings do.
	if got := countLaunchctlVerb(*calls, "load"); got != 1 {
		t.Fatalf("`launchctl load` invocations = %d, want 1 — without the load the source is never sabotaged and this test proves nothing", got)
	}
	if info, err := os.Stat(sourcePath); err != nil || !info.IsDir() {
		t.Fatalf("source bossd is not the sabotaged directory (err %v) — a second EnsureStaged would have succeeded", err)
	}
	if got, want := startedPath, expectedStagedBossdPath(t); got != want {
		t.Errorf("fallback started %q, want the staged path %q", got, want)
	}
	if got, err := os.ReadFile(expectedStagedBossdPath(t)); err != nil || string(got) != "version two" {
		t.Fatalf("staged bossd = %q (err %v), want the upgraded contents — otherwise the reuse proves nothing", got, err)
	}
}

// TestPlatformEnsureRunningLeavesACurrentPlistAlone asserts the compare-then-
// write half: the plist names the same stable staged path on every start, so
// re-staging must not churn a file launchd watches.
func TestPlatformEnsureRunningLeavesACurrentPlistAlone(t *testing.T) {
	sourcePath, plistPath, socketPath := prepareEnsureRunningEnvironment(t)

	// Upgrade the source so the refresh definitely runs; only then does "the
	// plist was left alone" say anything.
	if err := os.WriteFile(sourcePath, []byte("version two"), 0o755); err != nil {
		t.Fatalf("upgrade source bossd: %v", err)
	}

	before, err := os.Stat(plistPath)
	if err != nil {
		t.Fatalf("stat plist before start: %v", err)
	}

	stubLoadServesSocket(t, socketPath, nil)
	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeServiceManager {
		t.Errorf("StartMode = %v, want %v when the LaunchAgent served the socket", mode, StartModeServiceManager)
	}

	after, err := os.Stat(plistPath)
	if err != nil {
		t.Fatalf("stat plist after start: %v", err)
	}
	// mtime, not content: os.WriteFile truncates in place, so identical bytes
	// and an unchanged inode are both consistent with a needless rewrite.
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("plist was rewritten (mtime %s -> %s) although its generated bytes are unchanged",
			before.ModTime(), after.ModTime())
	}
	staged, err := os.ReadFile(expectedStagedBossdPath(t))
	if err != nil || string(staged) != "version two" {
		t.Fatalf("staged bossd = %q (err %v), want the upgraded contents — otherwise the check above proves nothing", staged, err)
	}
}

func TestPlatformRestartBootstrapsExistingPlistWhenResolutionFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("create LaunchAgents dir: %v", err)
	}
	existingPlist := []byte("existing plist remains usable")
	if err := os.WriteFile(plistPath, existingPlist, 0o600); err != nil {
		t.Fatalf("write existing plist: %v", err)
	}

	originalExecutablePath := executablePath
	executablePath = func() (string, error) { return "", errors.New("executable unavailable") }
	originalRunLaunchctl := runLaunchctl
	var calls [][]string
	runLaunchctl = func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}
	t.Cleanup(func() {
		executablePath = originalExecutablePath
		runLaunchctl = originalRunLaunchctl
	})

	err = platformRestart()
	if err == nil || !strings.Contains(err.Error(), "bossd not found") {
		t.Fatalf("platformRestart error = %v, want bossd resolution error", err)
	}
	if len(calls) != 2 || calls[0][0] != "bootout" || calls[1][0] != "bootstrap" {
		t.Fatalf("launchctl calls = %v, want bootout then bootstrap", calls)
	}
	gotPlist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read existing plist: %v", err)
	}
	if string(gotPlist) != string(existingPlist) {
		t.Errorf("existing plist changed on resolution failure: got %q, want %q", gotPlist, existingPlist)
	}
}

// TestPlatformRestartRetriesTransientBootstrapFailure converts the BOS-864
// incident — `bootstrap` returning exit 5 immediately after a bootout, then
// succeeding on a plain retry moments later — into an overall success.
func TestPlatformRestartRetriesTransientBootstrapFailure(t *testing.T) {
	prepareRestartEnvironment(t)
	bootstraps := 0
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		if args[0] != "bootstrap" {
			return nil, nil
		}
		bootstraps++
		if bootstraps == 1 {
			return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
		}
		return nil, nil
	})

	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart after a transient bootstrap failure = %v, want nil", err)
	}
	if got := countBootstrapCalls(*calls); got != 2 {
		t.Fatalf("bootstrap invocations = %d, want 2 (one failure, one success)", got)
	}
}

// TestPlatformRestartReportsTheFirstBootstrapFailure pins which of the bounded
// attempts' errors reaches the operator. The first failure is the informative
// one — it is the cause; anything a later attempt reports is a consequence of
// the state the first one left behind. Overwriting it per attempt would make
// the retry diagnose worse than the single attempt it replaced.
func TestPlatformRestartReportsTheFirstBootstrapFailure(t *testing.T) {
	prepareRestartEnvironment(t)
	bootstraps := 0
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "bootstrap":
			bootstraps++
			if bootstraps == 1 {
				return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			// A different, non-already-loaded failure on every later attempt, so
			// this test isolates first-versus-last from the short-circuit below.
			return []byte("Bootstrap failed: 1: Operation not permitted"), fakeExitError(t, 1)
		case "list":
			return nil, fakeExitError(t, 113)
		default:
			return nil, nil
		}
	})

	err := platformRestart()
	if err == nil {
		t.Fatal("platformRestart with a permanently failing bootstrap returned nil")
	}
	if !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("platformRestart error %q dropped the first (informative) failure", err.Error())
	}
	if strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("platformRestart error %q reported a later attempt instead of the first", err.Error())
	}
	// The verified outcome and the bounded count are unchanged by this.
	if !strings.Contains(err.Error(), "the daemon is now stopped") ||
		!strings.Contains(err.Error(), "boss daemon start") {
		t.Errorf("platformRestart error %q lost the verified outcome", err.Error())
	}
	if got := countBootstrapCalls(*calls); got != launchdBootstrapAttempts {
		t.Fatalf("bootstrap invocations = %d, want exactly %d", got, launchdBootstrapAttempts)
	}
}

// TestPlatformRestartStopsRetryingWhenAlreadyBootstrapped covers the exact
// shape the BOS-864 incident takes when attempt 1 loses the transition race but
// launchd registers the job anyway: every later attempt fails with
// already-loaded noise that no amount of retrying can clear.
func TestPlatformRestartStopsRetryingWhenAlreadyBootstrapped(t *testing.T) {
	prepareRestartEnvironment(t)
	if launchdBootstrapAttempts < 3 {
		t.Fatalf("launchdBootstrapAttempts = %d; this test needs >= 3 to prove a short-circuit", launchdBootstrapAttempts)
	}
	bootstraps := 0
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "bootstrap":
			bootstraps++
			if bootstraps == 1 {
				return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			// EEXIST: the job attempt 1 appeared to fail on is in fact loaded.
			return []byte("Bootstrap failed: 17: File exists"), fakeExitError(t, 17)
		case "list":
			return []byte("{\n\t\"PID\" = 4242;\n}\n"), nil
		default:
			return nil, nil
		}
	})

	err := platformRestart()
	if err == nil {
		t.Fatal("platformRestart with a permanently failing bootstrap returned nil")
	}
	// Two attempts, not launchdBootstrapAttempts: the already-loaded exit ends
	// the loop instead of sleeping through the remaining backoffs.
	if got := countBootstrapCalls(*calls); got != 2 {
		t.Fatalf("bootstrap invocations = %d, want exactly 2 (the already-loaded exit short-circuits)", got)
	}
	if !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("platformRestart error %q dropped the first (informative) failure", err.Error())
	}
	if strings.Contains(err.Error(), "File exists") {
		t.Errorf("platformRestart error %q showed already-loaded noise instead of the real cause", err.Error())
	}
	// AC-mandated: still an error, still carrying the verified outcome.
	if !strings.Contains(err.Error(), "a daemon is still running") {
		t.Errorf("platformRestart error %q does not report the surviving daemon", err.Error())
	}
	if strings.Contains(err.Error(), "the daemon is now stopped") {
		t.Fatalf("platformRestart claimed a stopped daemon while one is running: %v", err)
	}
	if !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Errorf("platformRestart error %q lost the underlying launchctl detail", err.Error())
	}
}

func TestPlatformRestartExhaustedRetriesReportsStoppedDaemon(t *testing.T) {
	prepareRestartEnvironment(t)
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "bootstrap":
			return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
		case "list":
			// Not loaded: platformGetStatus reads this as not running.
			return nil, fakeExitError(t, 113)
		default:
			return nil, nil
		}
	})

	err := platformRestart()
	if err == nil {
		t.Fatal("platformRestart with a permanently failing bootstrap returned nil")
	}
	for _, want := range []string{
		"launchctl bootstrap",
		"Input/output error",
		"the daemon is now stopped",
		"boss daemon start",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("platformRestart error %q missing %q", err.Error(), want)
		}
	}
	// Bounded: a regression to an unbounded loop fails here.
	if got := countBootstrapCalls(*calls); got != launchdBootstrapAttempts {
		t.Fatalf("bootstrap invocations = %d, want exactly %d", got, launchdBootstrapAttempts)
	}
}

func TestPlatformRestartExhaustedRetriesReportsStillRunningDaemon(t *testing.T) {
	prepareRestartEnvironment(t)
	stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "bootstrap":
			return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
		case "list":
			return []byte("{\n\t\"PID\" = 4242;\n}\n"), nil
		default:
			return nil, nil
		}
	})

	err := platformRestart()
	if err == nil {
		t.Fatal("platformRestart with a permanently failing bootstrap returned nil")
	}
	if !strings.Contains(err.Error(), "a daemon is still running") {
		t.Errorf("platformRestart error %q does not report the surviving daemon", err.Error())
	}
	if strings.Contains(err.Error(), "the daemon is now stopped") {
		t.Fatalf("platformRestart claimed a stopped daemon while one is running: %v", err)
	}
	if !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Errorf("platformRestart error %q lost the underlying launchctl detail", err.Error())
	}
}

// TestPlatformRestartSkipLaunchctlContractUnchanged pins the test-mode
// contract: no service-manager calls, no error, and the file-refresh path
// still runs.
func TestPlatformRestartSkipLaunchctlContractUnchanged(t *testing.T) {
	prepareRestartEnvironment(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	calls := stubRestartLaunchctl(t, func([]string) ([]byte, error) { return nil, nil })

	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart under BOSS_DAEMON_SKIP_LAUNCHCTL = %v, want nil", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("launchctl calls under BOSS_DAEMON_SKIP_LAUNCHCTL = %v, want none", *calls)
	}
	stagedPath := expectedStagedBossdPath(t)
	staged, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged bossd: %v", err)
	}
	if string(staged) != "version one" {
		t.Fatalf("staged bossd = %q, want the refreshed source contents", staged)
	}
}

// prepareRestartEnvironment gives platformRestart a resolvable source and an
// existing plist so only the bootstrap outcome varies between tests.
func prepareRestartEnvironment(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

	// PATH is deliberately left intact: executablePath below resolves bossd
	// before ResolveBossdPath ever consults PATH, and fakeExitError needs `sh`.
	sourcePath := writeFakeCellarBossd(t, home, "version one")
	originalExecutablePath := executablePath
	executablePath = func() (string, error) { return filepath.Join(filepath.Dir(sourcePath), "boss"), nil }
	originalDelay := launchdBootstrapRetryDelay
	launchdBootstrapRetryDelay = 0
	t.Cleanup(func() {
		executablePath = originalExecutablePath
		launchdBootstrapRetryDelay = originalDelay
	})

	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("create LaunchAgents dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("existing plist"), 0o600); err != nil {
		t.Fatalf("write existing plist: %v", err)
	}
	return plistPath
}

// stubRestartLaunchctl installs a recording runLaunchctl fake, following the
// save / reassign / t.Cleanup-restore shape the rest of this file uses.
func stubRestartLaunchctl(t *testing.T, respond func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	original := runLaunchctl
	calls := &[][]string{}
	runLaunchctl = func(args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if len(args) == 0 {
			return nil, nil
		}
		return respond(args)
	}
	t.Cleanup(func() { runLaunchctl = original })
	return calls
}

// countLaunchctlVerb counts recorded runLaunchctl invocations of one verb.
func countLaunchctlVerb(calls [][]string, verb string) int {
	count := 0
	for _, call := range calls {
		if len(call) > 0 && call[0] == verb {
			count++
		}
	}
	return count
}

// countBootstrapCalls counts the recorded `launchctl bootstrap` invocations,
// which is what pins platformRestart's retry as bounded.
func countBootstrapCalls(calls [][]string) int {
	return countLaunchctlVerb(calls, "bootstrap")
}

// stubLaunchdClock replaces the retry's wait with a simulated clock so a test
// can drive the real backoff schedule without spending real time. The returned
// pointer accumulates the simulated wait.
func stubLaunchdClock(t *testing.T) *time.Duration {
	t.Helper()
	original := launchdSleep
	var elapsed time.Duration
	launchdSleep = func(d time.Duration) { elapsed += d }
	t.Cleanup(func() { launchdSleep = original })
	return &elapsed
}

// TestPlatformRestartRetryWindowOutlastsTheOldFlatBudget drives the production
// backoff on a simulated clock: a bootout that has still not released the job
// past the old flat ~750ms budget now converges instead of erroring, which is
// the second half of the BOS-977 report.
func TestPlatformRestartRetryWindowOutlastsTheOldFlatBudget(t *testing.T) {
	const oldFlatWindow = 750 * time.Millisecond

	// prepareRestartEnvironment zeroes the base delay so the other retry tests
	// never sleep; this one needs the shipped schedule, kept free by the
	// simulated clock below. Its cleanup still restores the same value.
	productionDelay := launchdBootstrapRetryDelay
	prepareRestartEnvironment(t)
	launchdBootstrapRetryDelay = productionDelay
	simulated := stubLaunchdClock(t)

	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "bootstrap":
			if *simulated <= oldFlatWindow {
				return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			return nil, nil
		case "list":
			return nil, fakeExitError(t, 113)
		default:
			return nil, nil
		}
	})

	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart lost a bootout race that outlived the old %s budget: %v", oldFlatWindow, err)
	}
	if *simulated <= oldFlatWindow {
		t.Fatalf("retry converged after %s, inside the old %s budget — the test would pass against the old code", *simulated, oldFlatWindow)
	}
	if *simulated > launchdBootstrapRetryWindow {
		t.Fatalf("retry waited %s in total, over the %s bound", *simulated, launchdBootstrapRetryWindow)
	}
	if got := countBootstrapCalls(*calls); got > launchdBootstrapAttempts {
		t.Fatalf("bootstrap invocations = %d, want at most the %d bound", got, launchdBootstrapAttempts)
	}
}

// TestPlatformRestartBoundsTheWidenedRetryWindow pins the other direction:
// widening the window must not let a genuinely broken bootstrap run long, and
// it must not disturb which failure the operator is shown.
func TestPlatformRestartBoundsTheWidenedRetryWindow(t *testing.T) {
	productionDelay := launchdBootstrapRetryDelay
	prepareRestartEnvironment(t)
	launchdBootstrapRetryDelay = productionDelay
	simulated := stubLaunchdClock(t)

	bootstraps := 0
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "bootstrap":
			bootstraps++
			if bootstraps == 1 {
				return []byte("Bootstrap failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			// A different, non-already-loaded failure afterwards, so this
			// isolates first-versus-last from the short-circuit.
			return []byte("Bootstrap failed: 1: Operation not permitted"), fakeExitError(t, 1)
		case "list":
			return nil, fakeExitError(t, 113)
		default:
			return nil, nil
		}
	})

	err := platformRestart()
	if err == nil {
		t.Fatal("platformRestart with a permanently failing bootstrap returned nil")
	}
	if !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("platformRestart error %q dropped the first (informative) failure", err.Error())
	}
	if strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("platformRestart error %q reported a later attempt instead of the first", err.Error())
	}
	if *simulated > launchdBootstrapRetryWindow {
		t.Errorf("permanently failing bootstrap waited %s, over the %s bound", *simulated, launchdBootstrapRetryWindow)
	}
	if got := countBootstrapCalls(*calls); got != launchdBootstrapAttempts {
		t.Errorf("bootstrap invocations = %d, want exactly the %d bound", got, launchdBootstrapAttempts)
	}
}

// TestLaunchdBootstrapDelaysBackOffWithinTheBound pins the schedule itself: the
// waits grow, their total stays inside the explicit window, and that total
// clears the old flat budget the incident outran. Bounding the window rather
// than only the attempt count is the property BOS-977 asked for.
func TestLaunchdBootstrapDelaysBackOffWithinTheBound(t *testing.T) {
	const oldFlatWindow = 750 * time.Millisecond

	delays := launchdBootstrapDelays()
	if len(delays) == 0 {
		t.Fatal("launchdBootstrapDelays returned no waits; the retry would never pause")
	}
	// Exact, not an upper bound: the retry loop now counts attempts as
	// len(delays)+1 and never reads launchdBootstrapAttempts, so a window
	// narrowed below the base delay would silently shrink the schedule while
	// the constant still advertised six attempts.
	if got := len(delays) + 1; got != launchdBootstrapAttempts {
		t.Errorf("schedule allows %d attempts, want exactly the %d bound", got, launchdBootstrapAttempts)
	}

	var total time.Duration
	for i, delay := range delays {
		if delay <= 0 {
			t.Fatalf("delay %d = %s, want a positive wait", i, delay)
		}
		// The final step is clamped to whatever is left of the window rather
		// than dropped, so only the steps before it must grow.
		if i > 0 && i < len(delays)-1 && delay <= delays[i-1] {
			t.Errorf("delay %d = %s does not back off from %s", i, delay, delays[i-1])
		}
		total += delay
	}
	if total > launchdBootstrapRetryWindow {
		t.Errorf("schedule totals %s, over the %s bound", total, launchdBootstrapRetryWindow)
	}
	if total <= oldFlatWindow {
		t.Errorf("schedule totals %s, still inside the old %s budget the incident outran", total, oldFlatWindow)
	}
}

func TestGeneratePlist(t *testing.T) {
	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	checks := []string{
		"<string>com.bossanova.bossd</string>",
		"<string>/usr/local/bin/bossd</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"bossd.stdout.log",
		"bossd.stderr.log",
		// BOS-457: raise the FD limit so setup scripts bossd spawns don't
		// inherit macOS's low default (256) and die with EMFILE.
		"<key>SoftResourceLimits</key>",
		"<key>HardResourceLimits</key>",
		"<key>NumberOfFiles</key>",
		"<integer>65536</integer>",
	}

	for _, check := range checks {
		if !strings.Contains(plist, check) {
			t.Errorf("plist missing %q", check)
		}
	}
}

// TestGeneratedPlistExitTimeOutCoversShutdownBudget pins the macOS half of the
// BOS-888 ceiling chain. launchd SIGKILLs bossd at ExitTimeOut (default 20s, so
// this key must be present at all), and a hard kill there skips the deferred
// database.Close and the socket cleanup. If it does not exceed
// LifecycleShutdownTimeout, the CLI is still politely waiting for a socket
// launchd has already destroyed mid-drain.
func TestGeneratedPlistExitTimeOutCoversShutdownBudget(t *testing.T) {
	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	marker := "<key>ExitTimeOut</key>"
	at := strings.Index(plist, marker)
	if at < 0 {
		t.Fatalf("plist has no %s; launchd would fall back to its 20s default", marker)
	}
	rest := plist[at+len(marker):]
	open := strings.Index(rest, "<integer>")
	shut := strings.Index(rest, "</integer>")
	if open < 0 || shut < open {
		t.Fatalf("ExitTimeOut is not followed by an <integer> value: %q", rest[:min(len(rest), 80)])
	}
	secs, err := strconv.Atoi(strings.TrimSpace(rest[open+len("<integer>") : shut]))
	if err != nil {
		t.Fatalf("ExitTimeOut value: %v", err)
	}

	if got := time.Duration(secs) * time.Second; got <= LifecycleShutdownTimeout {
		t.Fatalf("plist ExitTimeOut = %v, want > LifecycleShutdownTimeout = %v so the CLI's wait, not launchd's SIGKILL, bounds a stuck shutdown", got, LifecycleShutdownTimeout)
	}
}

func TestGenerateMcpPlist(t *testing.T) {
	plist, err := generateMcpPlist("/usr/local/bin/mcp", 8765)
	if err != nil {
		t.Fatalf("generateMcpPlist: %v", err)
	}

	checks := []string{
		"<string>com.bossanova.mcp</string>",
		"<string>/usr/local/bin/mcp</string>",
		"<string>--http</string>",
		"<string>127.0.0.1:8765</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<true/>",
		"mcp.stdout.log",
		"mcp.stderr.log",
	}
	for _, check := range checks {
		if !strings.Contains(plist, check) {
			t.Errorf("plist missing %q", check)
		}
	}

	// Acceptance criterion: the MCP plist PATH must include the agent-runner
	// shim dirs. BOS-880 made the bossd plist render from the same helper, so
	// TestGeneratePlistIncludesShimDirectories asserts the mirror of this.
	if !strings.Contains(plist, "/.nodenv/shims") {
		t.Error("MCP plist PATH missing ~/.nodenv/shims")
	}
	if !strings.Contains(plist, "/.local/bin") {
		t.Error("MCP plist PATH missing ~/.local/bin")
	}

	// BOS-457: the FD-limit raise is scoped to bossd only; the MCP server does
	// not spawn FD-hungry setup scripts, so its plist must not carry the keys.
	if strings.Contains(plist, "NumberOfFiles") {
		t.Error("MCP plist should not contain NumberOfFiles (bossd-only FD raise)")
	}
}

func TestMcpServicePath(t *testing.T) {
	path, err := mcpServicePath()
	if err != nil {
		t.Fatalf("mcpServicePath: %v", err)
	}
	if !strings.HasSuffix(path, "Library/LaunchAgents/com.bossanova.mcp.plist") {
		t.Errorf("unexpected mcp service path: %s", path)
	}
}

func TestServicePath(t *testing.T) {
	path, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	if !strings.HasSuffix(path, "Library/LaunchAgents/com.bossanova.bossd.plist") {
		t.Errorf("unexpected service path: %s", path)
	}
}

// fakeExitError fabricates a genuine *exec.ExitError with the given exit
// code, so errors.As in bootoutLaunchdService sees the real type launchctl
// invocations produce rather than a hand-rolled stand-in.
func fakeExitError(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if err == nil {
		t.Fatalf("`sh -c exit %d` unexpectedly succeeded", code)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		exitErr = ee
	} else {
		t.Fatalf("expected *exec.ExitError from `sh -c exit %d`, got %v (%T)", code, err, err)
	}
	return exitErr
}

// assertBootoutArgs asserts the recorded runLaunchctl args are exactly the
// label-form bootout target (BOS-627: never a plist path).
func assertBootoutArgs(t *testing.T, args []string, label string) {
	t.Helper()
	uid := strconv.Itoa(os.Getuid())
	want := []string{"bootout", "gui/" + uid + "/" + label}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("bootout args = %v, want %v", args, want)
	}
	for _, a := range args {
		if strings.Contains(a, ".plist") {
			t.Errorf("bootout arg %q must not reference a plist path (BOS-627: label form only)", a)
		}
	}
}

// TestBootoutLaunchdService exercises bootoutLaunchdService directly (with an
// injected stillRunning probe) for both the MCP label and the daemon label,
// per the measured launchctl exit codes in BOS-627.
func TestBootoutLaunchdService(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	for _, label := range []string{McpLabel, Label} {
		label := label

		t.Run(label+"/exit_0_is_nil", func(t *testing.T) {
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return []byte(""), nil
			}
			if err := bootoutLaunchdService(label, func() bool {
				t.Fatal("stillRunning must not be consulted on exit 0")
				return true
			}); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
			assertBootoutArgs(t, gotArgs, label)
		})

		t.Run(label+"/exit_3_no_such_process_is_nil", func(t *testing.T) {
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return []byte("Boot-out failed: 3: No such process"), fakeExitError(t, 3)
			}
			if err := bootoutLaunchdService(label, func() bool {
				t.Fatal("stillRunning must not be consulted on exit 3")
				return true
			}); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
			assertBootoutArgs(t, gotArgs, label)
		})

		t.Run(label+"/exit_113_is_nil", func(t *testing.T) {
			runLaunchctl = func(args ...string) ([]byte, error) {
				return []byte("Could not find service"), fakeExitError(t, 113)
			}
			if err := bootoutLaunchdService(label, func() bool {
				t.Fatal("stillRunning must not be consulted on exit 113")
				return true
			}); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
		})

		// This is the reporter's regression: exit 5 ("Input/output error") is
		// what launchctl bootout actually returns for an already-stopped
		// service on this build. Treating every non-{0,3,113} exit as a hard
		// failure (the old exit-113-only check) meant `boss mcp stop` against
		// an already-stopped service always errored. Verifying stillRunning()
		// lets it report success instead.
		t.Run(label+"/exit_5_not_running_is_the_reporters_regression", func(t *testing.T) {
			bootoutVerifyTimeout = 50 * time.Millisecond
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			if err := bootoutLaunchdService(label, func() bool { return false }); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
			assertBootoutArgs(t, gotArgs, label)
		})

		t.Run(label+"/exit_5_still_running_is_an_error", func(t *testing.T) {
			bootoutVerifyTimeout = 50 * time.Millisecond
			runLaunchctl = func(args ...string) ([]byte, error) {
				return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			err := bootoutLaunchdService(label, func() bool { return true })
			if err == nil {
				t.Fatal("expected an error when stillRunning stays true after bootout exit 5")
			}
			if !strings.Contains(err.Error(), "Boot-out failed: 5: Input/output error") {
				t.Errorf("error %q does not contain the launchctl output verbatim", err)
			}
		})

		// M7: a nil stillRunning probe leaves bootoutLaunchdService with no way
		// to verify the job actually stopped, so it must fail closed --
		// surfacing the launchctl error rather than reporting success for an
		// exit code (5, a generic EIO) that can also mean the job is still up.
		t.Run(label+"/nil_stillRunning_fails_closed", func(t *testing.T) {
			runLaunchctl = func(args ...string) ([]byte, error) {
				return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			err := bootoutLaunchdService(label, nil)
			if err == nil {
				t.Fatal("expected an error when stillRunning is nil and bootout did not exit 0/3/113")
			}
			if !strings.Contains(err.Error(), "Boot-out failed: 5: Input/output error") {
				t.Errorf("error %q does not contain the launchctl output verbatim", err)
			}
		})
	}
}

// TestStillRunningProbesFailClosed covers the probe-error path of
// platformMcpStop/platformStop's verification callbacks: a status read that
// itself failed is "cannot tell", not "stopped". Reporting "not running" there
// would let bootoutLaunchdService turn an unverifiable end state into a silent
// success after a bootout that really failed -- the same fail-open hazard the
// nil-probe guard closes.
//
// The error is induced by pointing HOME at a tree where Library/LaunchAgents is
// a regular FILE, so os.Stat of the plist beneath it returns ENOTDIR rather than
// ENOENT (ENOENT is the ordinary not-installed case and must stay a nil error).
func TestStillRunningProbesFailClosed(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
		t.Fatalf("mkdir Library: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "Library", "LaunchAgents"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write LaunchAgents-as-file: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

	// Sanity: the induced failure really is a non-ENOENT status error, so the
	// assertions below are not vacuous.
	if _, err := platformMcpGetStatus(); err == nil {
		t.Fatal("platformMcpGetStatus: want a non-nil error when LaunchAgents is not a directory")
	}
	if _, err := platformGetStatus(); err == nil {
		t.Fatal("platformGetStatus: want a non-nil error when LaunchAgents is not a directory")
	}

	if !mcpStillRunningProbe() {
		t.Error("mcpStillRunningProbe() = false on a probe error, want true (fail closed)")
	}
	if !bossdStillRunningProbe() {
		t.Error("bossdStillRunningProbe() = false on a probe error, want true (fail closed)")
	}

	// End to end: a non-{0,3,113} bootout whose verification cannot be read
	// must surface the launchctl error, not report success.
	bootoutVerifyTimeout = 20 * time.Millisecond
	runLaunchctl = func(_ ...string) ([]byte, error) {
		return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
	}
	for name, stop := range map[string]func() error{
		"platformMcpStop": platformMcpStop,
		"platformStop":    platformStop,
	} {
		err := stop()
		if err == nil {
			t.Errorf("%s() = nil after bootout exit 5 with an unreadable status probe, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "Boot-out failed: 5: Input/output error") {
			t.Errorf("%s() error %q does not carry the launchctl output", name, err)
		}
	}
}

// TestPlatformMcpStopRealProbeAlreadyStopped is the counterpart boundary to
// TestStillRunningProbesFailClosed: with the REAL mcpStillRunningProbe (not an
// injected `return false`) and no plist installed, a bootout exit 5 must still
// resolve to success. That is acceptance criterion 1's neighbourhood, and it is
// the path a future edit to mcpStillRunningProbe could most easily break --
// making `boss mcp stop` error again when nothing is running, which is the
// original BOS-627 bug.
func TestPlatformMcpStopRealProbeAlreadyStopped(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	// A real (empty) LaunchAgents directory: os.Stat of the plist beneath it
	// returns ENOENT, which platformMcpGetStatus must report as a nil error
	// with Installed=false -- not as a probe failure.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o700); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

	st, err := platformMcpGetStatus()
	if err != nil {
		t.Fatalf("platformMcpGetStatus: want nil error for an absent plist, got %v", err)
	}
	if st.Installed {
		t.Fatalf("platformMcpGetStatus: Installed = true, want false for an absent plist")
	}
	if mcpStillRunningProbe() {
		t.Fatal("mcpStillRunningProbe() = true with no plist installed, want false")
	}

	bootoutVerifyTimeout = 20 * time.Millisecond
	runLaunchctl = func(_ ...string) ([]byte, error) {
		return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
	}
	if err := platformMcpStop(); err != nil {
		t.Errorf("platformMcpStop() = %v, want nil (bootout exit 5 with nothing running is the already-stopped case)", err)
	}
}

// TestPlatformMcpStopWiring exercises platformMcpStop/platformStop
// themselves (rather than bootoutLaunchdService directly) so the label
// selection and the skipLaunchctl() short-circuit are covered end to end.
func TestPlatformMcpStopWiring(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	t.Run("skip_launchctl_short_circuits_both", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		runLaunchctl = func(args ...string) ([]byte, error) {
			t.Fatal("runLaunchctl must not be called when BOSS_DAEMON_SKIP_LAUNCHCTL is set")
			return nil, nil
		}
		if err := platformMcpStop(); err != nil {
			t.Fatalf("platformMcpStop: %v", err)
		}
		if err := platformStop(); err != nil {
			t.Fatalf("platformStop: %v", err)
		}
	})

	t.Run("wiring_targets_the_correct_label", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		var gotArgs []string
		runLaunchctl = func(args ...string) ([]byte, error) {
			gotArgs = args
			return []byte(""), nil
		}

		if err := platformMcpStop(); err != nil {
			t.Fatalf("platformMcpStop: %v", err)
		}
		assertBootoutArgs(t, gotArgs, McpLabel)

		if err := platformStop(); err != nil {
			t.Fatalf("platformStop: %v", err)
		}
		assertBootoutArgs(t, gotArgs, Label)
	})
}

// TestGeneratePlistIncludesShimDirectories is the direct mirror of the MCP
// assertion in TestGenerateMcpPlist. BOS-880: bossd and the MCP agent must
// render their PATH from the same helper, so this test and that one fail
// together the moment the two diverge again.
func TestGeneratePlistIncludesShimDirectories(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{})

	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	for _, want := range []string{"/.nodenv/shims", "/.local/bin"} {
		if !strings.Contains(plist, want) {
			t.Errorf("bossd plist PATH missing %q", want)
		}
	}

	// The baseline must survive: daemon_path_extra prepends, it never replaces.
	for _, want := range []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"} {
		if !strings.Contains(plist, want) {
			t.Errorf("bossd plist PATH missing baseline entry %q", want)
		}
	}
}

// TestGeneratePlistAndMcpPlistShareOnePath is the parity invariant itself: a
// change that feeds one template from a different helper fails here.
func TestGeneratePlistAndMcpPlistShareOnePath(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"~/.asdf/shims"}})

	bossdPlist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}
	mcpPlist, err := generateMcpPlist("/usr/local/bin/mcp", 8765)
	if err != nil {
		t.Fatalf("generateMcpPlist: %v", err)
	}

	want := serviceEnvPath()
	for name, plist := range map[string]string{"bossd": bossdPlist, "mcp": mcpPlist} {
		if !strings.Contains(plist, "<string>"+want+"</string>") {
			t.Errorf("%s plist does not render the shared service PATH %q", name, want)
		}
	}
}

func TestGeneratePlistPlacesConfiguredExtraAheadOfBaseline(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"~/.asdf/shims"}})

	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	extra := strings.Index(plist, "/stub/home/.asdf/shims")
	baseline := strings.Index(plist, "/usr/local/bin:")
	switch {
	case extra < 0:
		t.Fatalf("plist missing the configured extra:\n%s", plist)
	case baseline < 0:
		t.Fatalf("plist missing the baseline:\n%s", plist)
	case extra > baseline:
		t.Errorf("configured extra at %d is behind the baseline at %d; it must be prepended", extra, baseline)
	}
}

// TestGeneratePlistRejectsHostileExtras is why sanitizing is load-bearing:
// text/template does not escape, so an XML-special character in an entry would
// otherwise corrupt the plist itself.
func TestGeneratePlistRejectsHostileExtras(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{
		`/opt/a&b`,
		`/opt/c<d`,
		`/opt/e"f`,
		"/opt/g\nh",
		"relative/bin",
	}})

	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	for _, unwanted := range []string{"/opt/a", "/opt/c", "/opt/e", "/opt/g", "relative/bin"} {
		if strings.Contains(plist, unwanted) {
			t.Errorf("plist contains rejected entry %q", unwanted)
		}
	}

	var parsed any
	if err := xml.Unmarshal([]byte(plist), &parsed); err != nil {
		t.Fatalf("rendered plist is not valid XML: %v\n%s", err, plist)
	}
}

// TestPlatformRestartRewritesPreChangePlist covers the upgrade moment for every
// existing install: after BOS-880 the on-disk plist differs from the new render
// exactly once, so the comparison branch must take the rewrite path and
// succeed rather than erroring or leaving the stale PATH in place.
func TestPlatformRestartRewritesPreChangePlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	stubServiceSettings(t, config.Settings{})

	// Stage a resolvable bossd and point executablePath at its sibling `boss`,
	// exactly as TestPlatformRestartRestagesAndRewritesPlist does. Without this
	// the restart's own ResolveBossdPath fails inside a hermetic sandbox, the
	// rewrite branch is never reached, and the assertion below would be
	// measuring the sandbox rather than the behaviour under test.
	sourcePath := writeFakeCellarBossd(t, home, "version one")
	stubExecutableNextTo(t, sourcePath)

	launchAgents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0o700); err != nil {
		t.Fatalf("create LaunchAgents dir: %v", err)
	}
	plistPath := filepath.Join(launchAgents, Label+".plist")

	// The pre-change plist: the hardcoded PATH literal this ticket removed.
	preChange := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>EnvironmentVariables</key>
	<dict><key>PATH</key><string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string></dict>
</dict></plist>
`
	if err := os.WriteFile(plistPath, []byte(preChange), 0o600); err != nil {
		t.Fatalf("write pre-change plist: %v", err)
	}

	if err := platformRestart(); err != nil {
		t.Fatalf("platformRestart against a pre-change plist: %v", err)
	}

	rewritten, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read rewritten plist: %v", err)
	}
	if string(rewritten) == preChange {
		t.Fatal("platformRestart left the pre-change plist in place; it must rewrite it")
	}
	if !strings.Contains(string(rewritten), "/.nodenv/shims") {
		t.Errorf("rewritten plist does not carry the repaired PATH:\n%s", rewritten)
	}
}

func TestPlistEnvironmentPath(t *testing.T) {
	cases := []struct {
		name  string
		plist string
		want  string
		ok    bool
	}{
		{
			name: "reads the environment PATH",
			plist: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/a:/b</string><key>LC_CTYPE</key><string>UTF-8</string></dict>
</dict></plist>`,
			want: "/a:/b",
			ok:   true,
		},
		{
			name: "PATH after other keys in the same dict",
			plist: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>EnvironmentVariables</key><dict><key>LC_CTYPE</key><string>UTF-8</string><key>PATH</key><string>/a:/b</string></dict>
</dict></plist>`,
			want: "/a:/b",
			ok:   true,
		},
		{
			// An unrelated PATH key OUTSIDE EnvironmentVariables must not be
			// mistaken for the service PATH.
			name: "no environment PATH but an unrelated PATH key later",
			plist: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>EnvironmentVariables</key><dict><key>LC_CTYPE</key><string>UTF-8</string></dict>
<key>PATH</key><string>/not/the/service/path</string>
</dict></plist>`,
			want: "",
			ok:   false,
		},
		{
			name: "no EnvironmentVariables at all",
			plist: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>ProgramArguments</key><array><string>/bin/bossd</string></array>
</dict></plist>`,
			want: "",
			ok:   false,
		},
		{
			name:  "malformed XML",
			plist: `<?xml version="1.0"?><plist><dict><key>EnvironmentVariables`,
			want:  "",
			ok:    false,
		},
		{
			name: "empty PATH is not a usable value",
			plist: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>EnvironmentVariables</key><dict><key>PATH</key><string></string></dict>
</dict></plist>`,
			want: "",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := plistEnvironmentPath([]byte(tc.plist))
			if got != tc.want || ok != tc.ok {
				t.Errorf("plistEnvironmentPath() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestPlistEnvironmentPathRoundTripsGeneratedPlist keeps the reader and the
// writer honest about each other: the parser is read back against the exact
// plist this package renders, so a template change cannot silently break the
// stale-PATH comparison in `boss daemon doctor`.
func TestPlistEnvironmentPathRoundTripsGeneratedPlist(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"/opt/my tools/bin"}})

	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	got, ok := plistEnvironmentPath([]byte(plist))
	if !ok {
		t.Fatalf("plistEnvironmentPath could not read the plist this package renders:\n%s", plist)
	}
	if want := serviceEnvPath(); got != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}
}

// TestPlatformSpawnHistory covers the launchd wiring around
// parseLaunchdSpawnHistory: the service target it asks about, the
// BOSS_DAEMON_SKIP_LAUNCHCTL short-circuit, and the two launchctl failure
// shapes. BOS-1183: the classification itself is exercised platform-agnostically
// in spawnhistory_test.go against the same fixtures, so what is left to prove
// here is that the probe asks launchd the right question and fails closed when
// it does not get an answer.
func TestPlatformSpawnHistory(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	t.Cleanup(func() { runLaunchctl = origRunLaunchctl })

	wantTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + Label

	t.Run("never_spawned_job", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		var gotArgs []string
		runLaunchctl = func(args ...string) ([]byte, error) {
			gotArgs = args
			return readSpawnFixture(t, "never-spawned.txt"), nil
		}

		got, err := platformSpawnHistory()
		if err != nil {
			t.Fatalf("platformSpawnHistory: %v", err)
		}
		wantArgs := []string{"print", wantTarget}
		if len(gotArgs) != len(wantArgs) || gotArgs[0] != wantArgs[0] || gotArgs[1] != wantArgs[1] {
			t.Errorf("runLaunchctl args = %q, want %q", gotArgs, wantArgs)
		}
		if got.Target != wantTarget {
			t.Errorf("Target = %q, want %q", got.Target, wantTarget)
		}
		if got.State != SpawnStateNeverSpawned {
			t.Errorf("State = %q, want %q", got.State, SpawnStateNeverSpawned)
		}
		if !got.RunsKnown || got.Runs != 0 || !got.NeverExited {
			t.Errorf("RunsKnown=%v Runs=%d NeverExited=%v, want true/0/true", got.RunsKnown, got.Runs, got.NeverExited)
		}
	})

	t.Run("launchctl_exit_error_is_unknown_not_healthy", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		runLaunchctl = func(_ ...string) ([]byte, error) {
			return []byte("Could not find service \"com.bossanova.bossd\" in domain for login\n"), fakeExitError(t, 113)
		}

		got, err := platformSpawnHistory()
		if err != nil {
			t.Fatalf("platformSpawnHistory: want a nil error for a launchctl that ran and refused, got %v", err)
		}
		if got.State == SpawnStateHealthy {
			t.Fatal("State = healthy after a failed launchctl print; must fail closed")
		}
		if got.State != SpawnStateUnknown {
			t.Errorf("State = %q, want %q", got.State, SpawnStateUnknown)
		}
		if got.Target != wantTarget {
			t.Errorf("Target = %q, want %q", got.Target, wantTarget)
		}
		if !strings.Contains(got.Reason, "Could not find service") {
			t.Errorf("Reason = %q, want it to carry the launchctl output", got.Reason)
		}
	})

	t.Run("launchctl_not_executable_returns_an_error", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		runLaunchctl = func(_ ...string) ([]byte, error) {
			return nil, errors.New(`exec: "launchctl": executable file not found in $PATH`)
		}

		got, err := platformSpawnHistory()
		if err == nil {
			t.Fatal("platformSpawnHistory() = nil error when launchctl could not be executed, want an error")
		}
		if got.State != SpawnStateUnknown {
			t.Errorf("State = %q, want %q even on the error path", got.State, SpawnStateUnknown)
		}
		if got.Target != wantTarget {
			t.Errorf("Target = %q, want %q", got.Target, wantTarget)
		}
	})

	t.Run("skip_launchctl_does_not_shell_out", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		runLaunchctl = func(_ ...string) ([]byte, error) {
			t.Fatal("runLaunchctl must not be called when BOSS_DAEMON_SKIP_LAUNCHCTL is set")
			return nil, nil
		}

		got, err := platformSpawnHistory()
		if err != nil {
			t.Fatalf("platformSpawnHistory: %v", err)
		}
		if got.State != SpawnStateUnknown {
			t.Errorf("State = %q, want %q", got.State, SpawnStateUnknown)
		}
		if !strings.Contains(got.Reason, "BOSS_DAEMON_SKIP_LAUNCHCTL") {
			t.Errorf("Reason = %q, want it to name BOSS_DAEMON_SKIP_LAUNCHCTL", got.Reason)
		}
		if got.Target != wantTarget {
			t.Errorf("Target = %q, want %q", got.Target, wantTarget)
		}
	})
}

// TestGetSpawnHistoryDelegates proves the exported entry point is wired to the
// platform probe rather than to a default-valued zero struct -- a zero
// SpawnHistory has an empty State, which no caller should ever be handed.
func TestGetSpawnHistoryDelegates(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	t.Cleanup(func() { runLaunchctl = origRunLaunchctl })

	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	runLaunchctl = func(_ ...string) ([]byte, error) {
		return readSpawnFixture(t, "crash-loop.txt"), nil
	}

	got, err := GetSpawnHistory()
	if err != nil {
		t.Fatalf("GetSpawnHistory: %v", err)
	}
	if got.State != SpawnStateFailing {
		t.Errorf("State = %q, want %q", got.State, SpawnStateFailing)
	}
	if got.Runs != 47 || got.LastExitCode != 1 {
		t.Errorf("Runs = %d, LastExitCode = %d, want 47 and 1", got.Runs, got.LastExitCode)
	}
	if got.Target != "gui/"+strconv.Itoa(os.Getuid())+"/"+Label {
		t.Errorf("Target = %q, want the gui/<uid>/<label> form", got.Target)
	}
}

// TestPlatformInstallDefaultSupervisionModeMatchesAbsentKey is the BOS-1184 R5
// pin: introducing a supervision-mode seam must leave the default install path
// byte-identical to what it produced before the key existed.
//
// It compares artifacts rather than asserting "we did not branch", because the
// branch is genuinely there now — an absent key and an explicit "launch-agent"
// take the same path only because the resolver maps them to the same mode, and
// that mapping is the thing worth pinning.
func TestPlatformInstallDefaultSupervisionModeMatchesAbsentKey(t *testing.T) {
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	// One HOME for both renders, not one per subtest. The rendered plist
	// embeds HOME-derived PATH entries, so a per-subtest temp directory makes
	// the two artifacts differ for a reason that has nothing to do with the
	// supervision mode — and the comparison this test exists to make would be
	// meaningless noise rather than a signal.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	sourcePath := writeFakeCellarBossd(t, home, "version one")

	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	renderWith := func(t *testing.T, configured string) []byte {
		t.Helper()
		loadServiceSettings = func() (config.Settings, error) {
			return config.Settings{DaemonSupervisionMode: configured}, nil
		}
		if err := platformInstall(sourcePath, true); err != nil {
			t.Fatalf("platformInstall(%q): %v", configured, err)
		}
		plist, err := os.ReadFile(plistPath)
		if err != nil {
			t.Fatalf("read plist: %v", err)
		}
		return plist
	}

	absent := renderWith(t, "")
	explicit := renderWith(t, "launch-agent")

	if !bytes.Equal(absent, explicit) {
		t.Errorf("default install path differs between an absent key and an explicit launch-agent:\nabsent:\n%s\nexplicit:\n%s", absent, explicit)
	}
}

// TestPlatformInstallFailsClosedOnUnusableSupervisionMode pins that a mode the
// resolver refuses stops the install BEFORE anything is written.
//
// Both halves matter. Returning an error while still writing the LaunchAgent
// would leave a multi-user host with exactly the substrate it configured a mode
// to escape — installed, loadable, and reported by every later status read as
// the thing the operator asked for.
func TestPlatformInstallFailsClosedOnUnusableSupervisionMode(t *testing.T) {
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	for _, tt := range []struct {
		name       string
		configured string
		wantErr    error
	}{
		{"recognised but needing root", "unattended", ErrUnattendedRequiresRoot},
		{"unrecognised", "system-daemon", ErrUnknownSupervisionMode},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			// Pinned rather than inherited: were this suite ever run as root,
			// the unattended row would take the install path instead of the
			// refusal and this test would silently stop asserting anything.
			stubNonRootEUID(t)
			loadServiceSettings = func() (config.Settings, error) {
				return config.Settings{DaemonSupervisionMode: tt.configured}, nil
			}
			sourcePath := writeFakeCellarBossd(t, home, "version one")

			err := platformInstall(sourcePath, false)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("platformInstall error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.configured) {
				t.Errorf("install refusal %q does not name the configured value %q", err, tt.configured)
			}

			plistPath, pathErr := platformServicePath()
			if pathErr != nil {
				t.Fatalf("platformServicePath: %v", pathErr)
			}
			if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
				t.Errorf("a refused supervision mode still wrote a LaunchAgent at %s (stat err = %v)", plistPath, statErr)
			}
		})
	}
}

// TestPlatformRestartFailsClosedOnUnusableSupervisionMode is the companion pin
// for the OTHER path that creates the substrate. BOS-1184 U3 changed WHY the
// unattended row is refused — it needs root now, rather than being unbuilt —
// and left the property intact: restart must not fall back to writing a
// gui/<uid> LaunchAgent on a host that asked for something else.
//
// platformInstall's gate alone did not make the seam fail closed: refreshStagedPlist
// writes the plist when it is ABSENT and platformRestart then bootstraps it into
// gui/<uid>, so on a host with a refused mode `boss daemon install` correctly
// installed nothing and `boss daemon restart` went on to install and load
// exactly the substrate the configuration refused — and reported success.
func TestPlatformRestartFailsClosedOnUnusableSupervisionMode(t *testing.T) {
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	for _, tt := range []struct {
		name       string
		configured string
		wantErr    error
	}{
		{"recognised but needing root", "unattended", ErrUnattendedRequiresRoot},
		{"unrecognised", "system-daemon", ErrUnknownSupervisionMode},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			sourcePath := writeFakeCellarBossd(t, home, "version one")
			stubExecutableNextTo(t, sourcePath)
			// The LaunchAgents directory has to exist, or the plist write
			// would fail for a reason that has nothing to do with the gate and
			// the assertion below would pass vacuously.
			mkdirLaunchAgents(t)
			stubNonRootEUID(t)
			loadServiceSettings = func() (config.Settings, error) {
				return config.Settings{DaemonSupervisionMode: tt.configured}, nil
			}

			err := platformRestart()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("platformRestart error = %v, want %v", err, tt.wantErr)
			}

			plistPath, pathErr := platformServicePath()
			if pathErr != nil {
				t.Fatalf("platformServicePath: %v", pathErr)
			}
			if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
				t.Errorf("a refused supervision mode still had restart write a LaunchAgent at %s (stat err = %v)", plistPath, statErr)
			}
		})
	}
}

// TestPlatformRestartDefaultSupervisionModeIsUnchanged is the AC6 half of the
// gate above: with no mode configured, and with the default named explicitly,
// restart still restages and rewrites the plist exactly as before. A gate that
// regressed the default path would be worse than the gap it closes.
func TestPlatformRestartDefaultSupervisionModeIsUnchanged(t *testing.T) {
	originalSettings := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = originalSettings })

	for _, configured := range []string{"", "launch-agent"} {
		t.Run("configured="+configured, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			sourcePath := writeFakeCellarBossd(t, home, "version one")
			stubExecutableNextTo(t, sourcePath)
			mkdirLaunchAgents(t)
			loadServiceSettings = func() (config.Settings, error) {
				return config.Settings{DaemonSupervisionMode: configured}, nil
			}

			if err := platformRestart(); err != nil {
				t.Fatalf("platformRestart: %v", err)
			}
			plistPath, pathErr := platformServicePath()
			if pathErr != nil {
				t.Fatalf("platformServicePath: %v", pathErr)
			}
			plist, readErr := os.ReadFile(plistPath)
			if readErr != nil {
				t.Fatalf("read plist after restart: %v", readErr)
			}
			stagedPath := expectedStagedBossdPath(t)
			if !strings.Contains(string(plist), "<string>"+stagedPath+"</string>") {
				t.Errorf("restart plist does not point at staged path %q:\n%s", stagedPath, plist)
			}
		})
	}
}

// mkdirLaunchAgents creates the per-user LaunchAgents directory the plist lives
// in. platformInstall does this itself; platformRestart does not, and a
// restart-only test that skips it watches the plist write fail for the wrong
// reason.
func mkdirLaunchAgents(t *testing.T) {
	t.Helper()
	plistPath, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("create LaunchAgents dir: %v", err)
	}
}

// stubLaunchctlWithDeadAgent records launchctl invocations and answers `list`
// the way launchd answers for a job that is NOT loaded: a non-zero exit.
//
// That detail decides what every zero-`load` assertion in this file is
// asserting ABOUT. It was originally load-bearing for non-vacuity:
// platformGetStatus set Running = true on ANY successful `list`, so a stub
// answering every verb with (nil, nil) reported the LaunchAgent as already
// running, platformEnsureRunning skipped its LaunchAgent arm for that reason
// instead of the routing one, and `countLaunchctlVerb(calls, "load") == 0`
// held on a build with no routing in it at all — measured: with the permissive
// stub, TestPlatformEnsureRunningLoadsTheLaunchAgentWhenNoWatchdogIsInstalled
// recorded zero loads.
//
// BOS-1218 moved that hazard rather than removing the need for this helper. A
// permissive (nil, nil) stub now yields an EMPTY answer, which parses to no
// PID and no PIDKnown — Status's "could not tell" state — so it no longer
// fakes a running agent, but it does silently swap which host the test
// describes: "launchd does not have this job" (what these tests mean, and what
// the non-zero exit says) for "launchctl answered and we could not read it".
// Those route the same today and are exactly the pair BOS-1218 R2a made
// distinguishable, so the fixture must keep saying which one it means.
// TestPlatformEnsureRunningLoadsTheLaunchAgentWhenNoWatchdogIsInstalled still
// pins the counterfactual directly, which is what keeps the zero-load
// assertions honest either way.
func stubLaunchctlWithDeadAgent(t *testing.T, onCall func(args []string)) *[][]string {
	t.Helper()
	return stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		if onCall != nil {
			onCall(args)
		}
		if args[0] == "list" {
			return []byte("Could not find service"), fakeExitError(t, 113)
		}
		return nil, nil
	})
}

// stubDefaultServiceSettings pins the supervision mode to the default, so a
// test's route is stated by the test rather than read off the host's
// settings.json (or off BOSS_SETTINGS_PATH, which overrides HOME entirely).
func stubDefaultServiceSettings(t *testing.T) {
	t.Helper()
	original := loadServiceSettings
	loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	t.Cleanup(func() { loadServiceSettings = original })
}

// stubUnreadableServiceSettings makes the settings seam FAIL, which is the
// SettingsErr != nil state: settings.json exists and could not be read or
// parsed, so LoadSupervisionModeStatus resolves Mode to the DEFAULT while the
// root job may still be loaded.
//
// It is a distinct fixture from stubConfiguredSupervisionMode because the two
// states reach classifyEnsureRunningRoute by different fields — an unparseable
// file never produces a raw value to resolve — and a mode-only branch loads the
// LaunchAgent on this one.
func stubUnreadableServiceSettings(t *testing.T) {
	t.Helper()
	original := loadServiceSettings
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{}, errors.New("settings.json: unexpected end of JSON input")
	}
	t.Cleanup(func() { loadServiceSettings = original })
}

// stubConfiguredSupervisionMode repoints the settings seam at one raw value,
// including values that are not modes at all — which is how the KTD1 rows below
// state "the operator typed a typo OF unattended".
func stubConfiguredSupervisionMode(t *testing.T, raw string) {
	t.Helper()
	original := loadServiceSettings
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{DaemonSupervisionMode: raw}, nil
	}
	t.Cleanup(func() { loadServiceSettings = original })
}

// stubDialReachableWhen replaces the dial seam with a predicate the test owns,
// so "the socket came back" is an event the test causes rather than a count of
// probes it has to keep in step with the implementation.
//
// It is the darwin counterpart of systemd_test.go's stubSocketReachableAfter,
// which is //go:build linux and therefore not visible here. A net.Pipe end is
// used rather than a real listener because unix socket paths have a length
// limit that t.TempDir() paths already exceed.
func stubDialReachableWhen(t *testing.T, reachable func() bool) {
	t.Helper()
	original := dialUnixSocket
	t.Cleanup(func() { dialUnixSocket = original })
	dialUnixSocket = func(string, string, time.Duration) (net.Conn, error) {
		if reachable() {
			conn, _ := net.Pipe()
			return conn, nil
		}
		return nil, errors.New("socket not reachable")
	}
}

// shrinkWatchdogRespawnWait removes the watchdog arm's wait budget, so a test
// that expects the wait to expire cannot sleep for it.
func shrinkWatchdogRespawnWait(t *testing.T, d time.Duration) {
	t.Helper()
	original := watchdogRespawnWait
	watchdogRespawnWait = d
	t.Cleanup(func() { watchdogRespawnWait = original })
}

// captureWatchdogCannotServeWarnings observes the KTD5 warning.
func captureWatchdogCannotServeWarnings(t *testing.T) *[]UnattendedInstall {
	t.Helper()
	warned := &[]UnattendedInstall{}
	original := warnUnattendedWatchdogCannotServe
	warnUnattendedWatchdogCannotServe = func(install UnattendedInstall) {
		*warned = append(*warned, install)
	}
	t.Cleanup(func() { warnUnattendedWatchdogCannotServe = original })
	return warned
}

// prepareWatchdogEnsureRunningEnvironment builds the host state BOS-1203 is
// about: a per-user LaunchAgent installed and NOT running — the only state in
// which platformEnsureRunning would load it — with a root-owned watchdog
// installed beside it, and a configured mode the caller names.
//
// The watchdog is really installed rather than planted as a file, because
// observeUnattendedInstall runs verifyWatchdogPaths over the entire layout: a
// bare plist reads UnattendedInstallInsecure, not Present, and would prove the
// wrong row.
//
// BOSS_DAEMON_SKIP_LAUNCHCTL is CLEARED on the way out. It has to be, because
// platformRestartUnattended's kickstart sits behind that gate and the root arm's
// whole assertion is that the kickstart happened.
//
// Clearing it puts the burden of the "installed and NOT running" state on the
// launchctl stub, so callers MUST use stubLaunchctlWithDeadAgent rather than a
// permissive one — see the reason written out there. TestPlatformEnsureRunningLoadsTheLaunchAgentWhenNoWatchdogIsInstalled
// pins the resulting counterfactual directly, and is what caught the permissive
// stub in the first place.
func prepareWatchdogEnsureRunningEnvironment(t *testing.T, configuredMode string) (socketPath string, layout watchdogLayout) {
	t.Helper()
	_, _, socketPath = prepareEnsureRunningEnvironment(t)
	layout = watchdogPaths()

	// Root only for the duration of the install; the test states its own euid.
	originalEUID := currentEUID
	currentEUID = func() int { return 0 }
	watchdogSource := writeFakeCellarBossd(t, t.TempDir(), "watchdog build")
	if err := platformInstallUnattended(watchdogSource, false); err != nil {
		currentEUID = originalEUID
		t.Fatalf("install the watchdog beside the LaunchAgent: %v", err)
	}
	currentEUID = originalEUID

	if _, err := os.Stat(layout.PlistPath); err != nil {
		t.Fatalf("watchdog plist is not on disk after install: %v", err)
	}
	if _, err := platformServicePath(); err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	stubConfiguredSupervisionMode(t, configuredMode)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	return socketPath, layout
}

// TestPlatformEnsureRunningLoadsTheLaunchAgentWhenNoWatchdogIsInstalled is the
// vacuity guard for every zero-`load` assertion below.
//
// "Zero loads" only means anything if this fixture is capable of producing a
// load at all. This is the identical fixture with the watchdog left off, and it
// must record exactly one — so a change that stopped entering the LaunchAgent
// arm for some unrelated reason fails HERE rather than silently turning the
// watchdog tests green for the wrong reason.
func TestPlatformEnsureRunningLoadsTheLaunchAgentWhenNoWatchdogIsInstalled(t *testing.T) {
	_, _, socketPath := prepareEnsureRunningEnvironment(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

	served := false
	stubDialReachableWhen(t, func() bool { return served })
	calls := stubLaunchctlWithDeadAgent(t, func(args []string) {
		if args[0] == "load" {
			served = true
		}
	})

	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeServiceManager {
		t.Errorf("StartMode = %v, want %v", mode, StartModeServiceManager)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 1 {
		t.Fatalf("`launchctl load` invocations = %d, want 1 — without this the zero-load assertions below prove nothing", got)
	}
}

// TestPlatformEnsureRunningNeverLoadsTheLaunchAgentBesideAWatchdog is the
// ticket, at the platformEnsureRunning level: on a host running the root-owned
// watchdog, no boss command may bootstrap the gui/<uid> LaunchAgent (R1).
//
// The rows are the configured-mode states, and the three where the mode
// DISAGREES with the machine are the ones that carry KTD1. A mode-only branch
// passes the first row and loads the LaunchAgent on the other three.
func TestPlatformEnsureRunningNeverLoadsTheLaunchAgentBesideAWatchdog(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		// unreadableSettings states the SettingsErr != nil row, which no raw
		// mode value can express: the file is unparseable, so Mode silently
		// carries the default while the root job is still loaded.
		unreadableSettings bool
	}{
		{name: "the ordinary unattended host", mode: "unattended"},
		{name: "an unrecognised value (a typo OF unattended)", mode: "unattnded"},
		{name: "the key reverted to the default", mode: "launch-agent"},
		{name: "no key at all, with the root job still loaded", mode: ""},
		{name: "an unparseable settings.json", unreadableSettings: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketPath, _ := prepareWatchdogEnsureRunningEnvironment(t, tc.mode)
			if tc.unreadableSettings {
				// AFTER the fixture: it stubs the seam itself, and this row
				// replaces that stub with a failing one. Cleanups run LIFO, so
				// the fixture's restore still lands last.
				stubUnreadableServiceSettings(t)
			}
			shrinkWatchdogRespawnWait(t, 0)
			captureResidueWarnings(t)
			captureWatchdogCannotServeWarnings(t)

			served := false
			stubDialReachableWhen(t, func() bool { return served })
			calls := stubLaunchctlWithDeadAgent(t, nil)

			originalStart := startDetachedBossd
			startDetachedBossd = func(string) error {
				served = true
				return nil
			}
			t.Cleanup(func() { startDetachedBossd = originalStart })

			mode, err := platformEnsureRunning(socketPath)
			if err != nil {
				t.Fatalf("platformEnsureRunning: %v", err)
			}
			// R3: the fail-open fallback still runs, so the host is never left
			// with no daemon over a settings value that could not be honoured.
			if mode != StartModeDetached {
				t.Errorf("StartMode = %v, want %v — the unsupervised fallback must stay reachable", mode, StartModeDetached)
			}
			if got := countLaunchctlVerb(*calls, "load"); got != 0 {
				t.Errorf("`launchctl load` invocations = %d, want 0 — a second supervisor was bootstrapped beside the watchdog", got)
			}
		})
	}
}

// TestPlatformEnsureRunningReportsTheWatchdogsRecoveryAsAlreadyRunning covers
// the non-root arm's success case.
//
// The verdict is StartModeAlreadyRunning rather than StartModeServiceManager on
// purpose: this process started nothing, and StartModeServiceManager's contract
// claims the service manager started the daemon and that it is therefore
// supervised — a claim this arm cannot support and one `boss daemon doctor`
// would contradict.
func TestPlatformEnsureRunningReportsTheWatchdogsRecoveryAsAlreadyRunning(t *testing.T) {
	socketPath, _ := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
	shrinkWatchdogRespawnWait(t, 5*time.Second)

	// Unreachable at the entry probe, served by the time the wait polls.
	probes := 0
	stubDialReachableWhen(t, func() bool {
		probes++
		return probes > 1
	})
	calls := stubLaunchctlWithDeadAgent(t, nil)

	originalStart := startDetachedBossd
	startDetachedBossd = func(path string) error {
		t.Errorf("the fallback spawned %q although the watchdog served the socket during the wait", path)
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeAlreadyRunning {
		t.Errorf("StartMode = %v, want %v", mode, StartModeAlreadyRunning)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 0 {
		t.Errorf("`launchctl load` invocations = %d, want 0", got)
	}
}

// TestPlatformEnsureRunningKickstartsTheWatchdogAsRoot pins R2's positive half:
// with the privilege to act on a system-domain job, the recovery path acts on
// the watchdog rather than on the LaunchAgent.
//
// The socket is made reachable BY the kickstart, so the assertion is causal:
// a run that skipped the kickstart could not observe a served socket.
func TestPlatformEnsureRunningKickstartsTheWatchdogAsRoot(t *testing.T) {
	socketPath, _ := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
	shrinkWatchdogRespawnWait(t, 5*time.Second)
	stubRootEUID(t)

	served := false
	stubDialReachableWhen(t, func() bool { return served })
	calls := stubLaunchctlWithDeadAgent(t, func(args []string) {
		if args[0] == "kickstart" {
			served = true
		}
	})

	originalStart := startDetachedBossd
	startDetachedBossd = func(path string) error {
		t.Errorf("R4 violated: the root path spawned a detached bossd at %q", path)
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	mode, err := platformEnsureRunning(socketPath)
	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeAlreadyRunning {
		t.Errorf("StartMode = %v, want %v", mode, StartModeAlreadyRunning)
	}
	if got := countLaunchctlVerb(*calls, "kickstart"); got != 1 {
		t.Fatalf("`launchctl kickstart` invocations = %d, want exactly 1", got)
	}
	// The verb alone is not enough — it must name the SYSTEM-domain watchdog,
	// not the gui agent.
	wantTarget := "system/" + WatchdogLabel
	found := false
	for _, call := range *calls {
		if call[0] == "kickstart" {
			for _, arg := range call {
				if arg == wantTarget {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("kickstart calls = %v, want one naming %q", *calls, wantTarget)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 0 {
		t.Errorf("`launchctl load` invocations = %d, want 0", got)
	}
}

// TestPlatformEnsureRunningRefusesToSpawnARootDaemon is R4, and it is the
// assertion that has to be about the STUB's call count rather than about the
// returned error.
//
// Under `sudo` the detached spawn produces a ROOT-owned bossd holding the
// user's socket, app-data directory and singleton lock — a state that user's own
// daemon can never reclaim without manual cleanup. An error return that still
// spawned would satisfy a "did it error" assertion while leaving exactly that
// behind, so the spawn count is what is pinned.
func TestPlatformEnsureRunningRefusesToSpawnARootDaemon(t *testing.T) {
	socketPath, _ := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
	shrinkWatchdogRespawnWait(t, 0)
	stubRootEUID(t)

	stubDialReachableWhen(t, func() bool { return false })
	calls := stubLaunchctlWithDeadAgent(t, nil)

	spawns := 0
	originalStart := startDetachedBossd
	startDetachedBossd = func(string) error {
		spawns++
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	mode, err := platformEnsureRunning(socketPath)
	if spawns != 0 {
		t.Fatalf("startDetachedBossd invocations = %d, want 0 — running as root this spawns a bossd the user can never reclaim", spawns)
	}
	if !errors.Is(err, ErrWatchdogRecoveryFailed) {
		t.Fatalf("error = %v, want one wrapping ErrWatchdogRecoveryFailed", err)
	}
	if mode != StartModeUnknown {
		t.Errorf("StartMode = %v, want %v", mode, StartModeUnknown)
	}
	// The refusal has to be actionable: an operator sent here needs the log to
	// read and the command to run.
	for _, want := range []string{WatchdogInstallCommand, "bossd-watchdog.stderr.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if got := countLaunchctlVerb(*calls, "kickstart"); got != 1 {
		t.Errorf("`launchctl kickstart` invocations = %d, want 1 — the recovery must still target the watchdog before refusing", got)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 0 {
		t.Errorf("`launchctl load` invocations = %d, want 0", got)
	}
}

// TestPlatformEnsureRunningDoesNotWaitForAnInsecureWatchdog is KTD5.
//
// verifyWatchdogPaths failing is a durable HOST fault — a group-writable
// /usr/local, or in this case a plist whose binary is gone — and such a state
// cannot repair itself between two boss commands, so the wait is known-futile
// before it is spent. The budget is deliberately left LARGE here: the elapsed
// time is the assertion, and it would be meaningless against a budget already
// shrunk to zero.
func TestPlatformEnsureRunningDoesNotWaitForAnInsecureWatchdog(t *testing.T) {
	socketPath, layout := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
	// Break the layout the way an operator's host does: the plist survives, the
	// root-owned binary it names does not.
	if err := os.Remove(layout.BinaryPath); err != nil {
		t.Fatalf("remove watchdog binary: %v", err)
	}
	if got := observeUnattendedInstall().State; got != UnattendedInstallInsecure {
		t.Fatalf("install state = %d, want UnattendedInstallInsecure — this test would otherwise prove the Present row", got)
	}

	const budget = 30 * time.Second
	shrinkWatchdogRespawnWait(t, budget)
	warned := captureWatchdogCannotServeWarnings(t)

	served := false
	stubDialReachableWhen(t, func() bool { return served })
	calls := stubLaunchctlWithDeadAgent(t, nil)

	originalStart := startDetachedBossd
	startDetachedBossd = func(string) error {
		served = true
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	start := time.Now()
	mode, err := platformEnsureRunning(socketPath)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if mode != StartModeDetached {
		t.Errorf("StartMode = %v, want %v — the fallback must still be reached", mode, StartModeDetached)
	}
	if elapsed >= budget {
		t.Errorf("platformEnsureRunning took %s against a %s budget — the insecure watchdog was waited for", elapsed, budget)
	}
	if len(*warned) != 1 {
		t.Fatalf("cannot-serve warnings = %d, want exactly 1", len(*warned))
	}
	// The warning must carry the REASON, not just the fact, or the operator has
	// no path from "unsupervised" to a fixed host.
	if (*warned)[0].Err == nil {
		t.Error("the cannot-serve warning carried no reason, so it names no offending path")
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 0 {
		t.Errorf("`launchctl load` invocations = %d, want 0", got)
	}
}

// TestPlatformEnsureRunningWarnsAboutResidueOnTheWatchdogRoute pins the other
// warning on this route: when the configured mode no longer describes the host,
// the operator is told the root job is still there AND given the remedy.
//
// It is a separate warning from the KTD5 one because the two states have
// different fixes — remove the root job versus repair the offending path — and
// the assertion below is that the residue warning fires on exactly the arm
// where its remedy is the true one.
func TestPlatformEnsureRunningWarnsAboutResidueOnTheWatchdogRoute(t *testing.T) {
	socketPath, layout := prepareWatchdogEnsureRunningEnvironment(t, "launch-agent")
	shrinkWatchdogRespawnWait(t, 0)
	warned := captureResidueWarnings(t)

	served := false
	stubDialReachableWhen(t, func() bool { return served })
	stubLaunchctlWithDeadAgent(t, nil)

	originalStart := startDetachedBossd
	startDetachedBossd = func(string) error {
		served = true
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	if _, err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
	}
	if len(*warned) != 1 || (*warned)[0] != layout.PlistPath {
		t.Fatalf("residue warnings = %v, want exactly one naming %s", *warned, layout.PlistPath)
	}
}

// TestPlatformEnsureRunningWatchdogRouteIssuesNoLaunchctlCallWhenSkipped is the
// U2 integration row: with BOSS_DAEMON_SKIP_LAUNCHCTL set the watchdog arm must
// reach launchd not at all and still return a DEFINED StartMode rather than
// falling into an unhandled shape.
func TestPlatformEnsureRunningWatchdogRouteIssuesNoLaunchctlCallWhenSkipped(t *testing.T) {
	socketPath, _ := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	shrinkWatchdogRespawnWait(t, 0)
	stubRootEUID(t)

	served := false
	stubDialReachableWhen(t, func() bool { return served })
	calls := stubLaunchctlWithDeadAgent(t, nil)

	originalStart := startDetachedBossd
	startDetachedBossd = func(string) error {
		served = true
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	mode, err := platformEnsureRunning(socketPath)
	// Root with the kickstart skipped still refuses rather than spawning (R4).
	if !errors.Is(err, ErrWatchdogRecoveryFailed) {
		t.Fatalf("error = %v, want one wrapping ErrWatchdogRecoveryFailed", err)
	}
	if mode != StartModeUnknown {
		t.Errorf("StartMode = %v, want %v", mode, StartModeUnknown)
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl calls = %v, want none under BOSS_DAEMON_SKIP_LAUNCHCTL", *calls)
	}
}

// TestPlatformEnsureRunningTreatsAFailedKickstartAsNonFatal pins the
// best-effort half of the root arm.
//
// The kickstart is a recovery attempt, not a lifecycle command the operator
// invoked, so a launchctl failure must not swallow the refusal that follows it:
// the operator still needs to be told the socket is unserved and that a root
// spawn was refused. Without this the failure would be reported only as
// "kickstart failed", and R4's reason for not spawning would never be stated.
func TestPlatformEnsureRunningTreatsAFailedKickstartAsNonFatal(t *testing.T) {
	socketPath, _ := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
	shrinkWatchdogRespawnWait(t, 0)
	stubRootEUID(t)

	var kickstartErrors []error
	originalWarn := warnWatchdogKickstartFailed
	warnWatchdogKickstartFailed = func(err error) { kickstartErrors = append(kickstartErrors, err) }
	t.Cleanup(func() { warnWatchdogKickstartFailed = originalWarn })

	stubDialReachableWhen(t, func() bool { return false })
	stubLaunchctlWithDeadAgent(t, nil)
	originalRun := runLaunchctl
	runLaunchctl = func(args ...string) ([]byte, error) {
		if args[0] == "kickstart" {
			return []byte("Could not kickstart service"), fakeExitError(t, 5)
		}
		return originalRun(args...)
	}
	t.Cleanup(func() { runLaunchctl = originalRun })

	spawns := 0
	originalStart := startDetachedBossd
	startDetachedBossd = func(string) error {
		spawns++
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = originalStart })

	_, err := platformEnsureRunning(socketPath)
	if len(kickstartErrors) != 1 {
		t.Fatalf("kickstart warnings = %d, want exactly 1", len(kickstartErrors))
	}
	// The failure is reported, then superseded by the refusal — not returned in
	// its place.
	if !errors.Is(err, ErrWatchdogRecoveryFailed) {
		t.Fatalf("error = %v, want one wrapping ErrWatchdogRecoveryFailed", err)
	}
	if spawns != 0 {
		t.Errorf("startDetachedBossd invocations = %d, want 0 even when the kickstart failed", spawns)
	}
}

// TestPlatformSpawnHistoryFollowsTheSupervisionSubstrate is BOS-1204 AC3.
//
// Spawn history is the only probe that can tell a REGISTERED job from a
// RUNNABLE one, and on an unattended host the job launchd actually spawns is
// the root-owned watchdog in the `system` domain. Reading gui/<uid> there is
// not merely uninformative: launchctl exits "could not find service in domain"
// and the doctor line reads `launchd spawn history: unknown` on a machine whose
// spawn history is perfectly readable one domain over.
//
// Both the ARGV and the reported Target are asserted, because AC3 has two
// halves — read the right target, and say which target was read.
func TestPlatformSpawnHistoryFollowsTheSupervisionSubstrate(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origSettings := loadServiceSettings
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		loadServiceSettings = origSettings
	})

	launchAgentTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + Label

	for _, tc := range []struct {
		name       string
		configured string
		wantTarget string
	}{
		{name: "default", configured: "", wantTarget: launchAgentTarget},
		{name: "launch-agent", configured: "launch-agent", wantTarget: launchAgentTarget},
		{name: "unattended", configured: "unattended", wantTarget: "system/" + WatchdogLabel},
		{name: "unrecognised value never routes to the watchdog", configured: "unattnded", wantTarget: launchAgentTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			loadServiceSettings = func() (config.Settings, error) {
				return config.Settings{DaemonSupervisionMode: tc.configured}, nil
			}
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return readSpawnFixture(t, "healthy.txt"), nil
			}

			got, err := platformSpawnHistory()
			if err != nil {
				t.Fatalf("platformSpawnHistory: %v", err)
			}
			wantArgs := []string{"print", tc.wantTarget}
			if len(gotArgs) != len(wantArgs) || gotArgs[0] != wantArgs[0] || gotArgs[1] != wantArgs[1] {
				t.Fatalf("runLaunchctl args = %q, want %q", gotArgs, wantArgs)
			}
			if got.Target != tc.wantTarget {
				t.Fatalf("Target = %q, want %q", got.Target, tc.wantTarget)
			}
		})
	}
}

// TestPlatformSpawnHistorySkipShortCircuitNamesTheSubstrateTarget pins AC9
// against the shape that would satisfy it vacuously: short-circuiting so early
// that the reported target is the LaunchAgent's on a host that has none.
//
// The env var must suppress the launchctl READ, not the substrate resolution —
// otherwise the skip line tells a CI operator the run asked about a target it
// would never have asked about.
func TestPlatformSpawnHistorySkipShortCircuitNamesTheSubstrateTarget(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origSettings := loadServiceSettings
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		loadServiceSettings = origSettings
	})
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{DaemonSupervisionMode: "unattended"}, nil
	}
	runLaunchctl = func(args ...string) ([]byte, error) {
		t.Fatalf("launchctl was invoked with %v under BOSS_DAEMON_SKIP_LAUNCHCTL", args)
		return nil, nil
	}

	got, err := platformSpawnHistory()
	if err != nil {
		t.Fatalf("platformSpawnHistory: %v", err)
	}
	if got.State != SpawnStateUnknown {
		t.Fatalf("State = %q, want %q", got.State, SpawnStateUnknown)
	}
	if want := "system/" + WatchdogLabel; got.Target != want {
		t.Fatalf("Target = %q, want %q", got.Target, want)
	}
}

// launchctlRegisteredNoPIDOutput is what `launchctl list com.bossanova.bossd`
// printed on the host `delta` on 2026-09-08 while the job was REGISTERED and
// had never been spawned: exit 0, and no "PID" key anywhere in the answer.
// `launchctl print` for the same job at the same instant reported
// `state = not running` and `runs = 0` (BOS-1218).
const launchctlRegisteredNoPIDOutput = `{
	"LimitLoadToSessionType" = "Aqua";
	"Label" = "com.bossanova.bossd";
	"OnDemand" = false;
	"LastExitStatus" = 0;
};
`

// launchctlRunningOutput is the same answer for a job launchd actually spawned.
const launchctlRunningOutput = `{
	"LimitLoadToSessionType" = "Aqua";
	"Label" = "com.bossanova.bossd";
	"OnDemand" = false;
	"LastExitStatus" = 0;
	"PID" = 80034;
};
`

// prepareLaunchAgentStatusEnvironment puts both LaunchAgent plists on disk
// under a temp HOME so platformGetStatus and platformMcpGetStatus get past
// their os.Stat guard and reach the launchctl probe the caller stubs.
//
// BOSS_DAEMON_SKIP_LAUNCHCTL is cleared explicitly: it short-circuits both
// probes before any launchctl call, so a stray value inherited from the
// environment would make every assertion below vacuous.
func prepareLaunchAgentStatusEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	for _, resolve := range []func() (string, error){platformServicePath, mcpServicePath} {
		path, err := resolve()
		if err != nil {
			t.Fatalf("resolve service path: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create LaunchAgents dir: %v", err)
		}
		if err := os.WriteFile(path, []byte("<plist/>"), 0o600); err != nil {
			t.Fatalf("write plist: %v", err)
		}
	}
}

// TestPlatformGetStatusRequiresAPIDBeforeReportingRunning is BOS-1218's
// central case, and there was no direct test of this parse loop before it.
//
// `launchctl list <label>` exits 0 for a job launchd has merely REGISTERED, so
// the exit code alone cannot tell a registration from a running process. The
// PID key in the answer is what separates them, and Running must be derived
// from it.
//
// The last two rows are R2a: a zero PID must not stand for two opposite
// observations. "registered, launchd reported no PID" is an ANSWER; a probe
// that could not be run, or whose output carries no recognisable shape, is
// "cannot tell". TestPlatformGetStatusSeparatesAnAbsentPIDFromAnUnreadableOne
// pins that the two Status values actually differ.
func TestPlatformGetStatusRequiresAPIDBeforeReportingRunning(t *testing.T) {
	for _, tc := range []struct {
		name         string
		out          string
		listErr      bool
		wantRunning  bool
		wantPID      int
		wantPIDKnown bool
	}{
		{
			name:         "registered with no PID is not running",
			out:          launchctlRegisteredNoPIDOutput,
			wantRunning:  false,
			wantPID:      0,
			wantPIDKnown: true,
		},
		{
			name:         "a reported PID is what makes it running",
			out:          launchctlRunningOutput,
			wantRunning:  true,
			wantPID:      80034,
			wantPIDKnown: true,
		},
		{
			name:         "tab-separated fallback carries the PID",
			out:          "80034\t0\tcom.bossanova.bossd\n",
			wantRunning:  true,
			wantPID:      80034,
			wantPIDKnown: true,
		},
		{
			name:         "tab-separated fallback with no PID is not running",
			out:          "-\t0\tcom.bossanova.bossd\n",
			wantRunning:  false,
			wantPID:      0,
			wantPIDKnown: true,
		},
		{
			name:         "label not loaded is not running and settles nothing",
			out:          "Could not find service \"com.bossanova.bossd\" in domain for uid: 501",
			listErr:      true,
			wantRunning:  false,
			wantPID:      0,
			wantPIDKnown: false,
		},
		{
			name:         "an unreadable answer settles nothing and is not 'stopped'",
			out:          "launchctl: something entirely unexpected\n",
			wantRunning:  false,
			wantPID:      0,
			wantPIDKnown: false,
		},
		{
			// The rest of the dictionary parses, so `known` was already true
			// by the time this line is read: without the veto the answer would
			// be reported as the SETTLED observation "launchd says this job
			// owns no process", which is precisely the unknown-vs-absent
			// collapse R2a exists to prevent.
			name:         "an unparseable PID value vetoes the whole answer",
			out:          strings.Replace(launchctlRunningOutput, `"PID" = 80034;`, `"PID" = bogus;`, 1),
			wantRunning:  false,
			wantPID:      0,
			wantPIDKnown: false,
		},
		{
			// readSystemdMainPID rejects `v <= 0` on the other substrate; a
			// negative PID is not a process launchd owns and is not an
			// observation that it owns none.
			name:         "a negative PID value settles nothing",
			out:          strings.Replace(launchctlRunningOutput, `"PID" = 80034;`, `"PID" = -1;`, 1),
			wantRunning:  false,
			wantPID:      0,
			wantPIDKnown: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareLaunchAgentStatusEnvironment(t)
			stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
				if args[0] != "list" {
					t.Fatalf("unexpected launchctl verb %q", args[0])
				}
				if tc.listErr {
					return []byte(tc.out), fakeExitError(t, 113)
				}
				return []byte(tc.out), nil
			})

			st, err := platformGetStatus()
			if err != nil {
				t.Fatalf("platformGetStatus: %v", err)
			}
			if !st.Installed {
				t.Fatalf("Installed = false, want true; the plist is on disk")
			}
			if st.Running != tc.wantRunning {
				t.Fatalf("Running = %v, want %v (PID %d)", st.Running, tc.wantRunning, st.PID)
			}
			if st.PID != tc.wantPID {
				t.Fatalf("PID = %d, want %d", st.PID, tc.wantPID)
			}
			if st.PIDKnown != tc.wantPIDKnown {
				t.Fatalf("PIDKnown = %v, want %v", st.PIDKnown, tc.wantPIDKnown)
			}
			if st.Running && st.PID == 0 {
				t.Fatalf("Running with no PID: %+v; R2 says Running implies a PID on launchd", st)
			}
		})
	}
}

// TestPlatformGetStatusSeparatesAnAbsentPIDFromAnUnreadableOne is R2a stated
// as one assertion: the two observations must not collapse to the same value.
func TestPlatformGetStatusSeparatesAnAbsentPIDFromAnUnreadableOne(t *testing.T) {
	read := func(out string, listErr bool) Status {
		t.Helper()
		prepareLaunchAgentStatusEnvironment(t)
		stubRestartLaunchctl(t, func([]string) ([]byte, error) {
			if listErr {
				return []byte(out), fakeExitError(t, 113)
			}
			return []byte(out), nil
		})
		st, err := platformGetStatus()
		if err != nil {
			t.Fatalf("platformGetStatus: %v", err)
		}
		return *st
	}

	reportedAbsent := read(launchctlRegisteredNoPIDOutput, false)
	couldNotTell := read("Could not find service", true)
	if reportedAbsent == couldNotTell {
		t.Fatalf("a registered job that reported no PID is indistinguishable from an unreadable probe: %+v", reportedAbsent)
	}
	if !reportedAbsent.PIDKnown {
		t.Fatalf("PIDKnown = false for an answer that named no PID: %+v", reportedAbsent)
	}
	if couldNotTell.PIDKnown {
		t.Fatalf("PIDKnown = true for a probe that could not be read: %+v", couldNotTell)
	}
}

// TestPlatformMcpGetStatusRequiresAPIDBeforeReportingRunning repeats the two
// decisive rows against McpLabel. The MCP probe carries a byte-identical
// assignment against the same launchctl behaviour, and its consumer
// mcpStillRunningProbe fills the same bootout-verify role as
// bossdStillRunningProbe, so leaving it would have fixed one of two identical
// causes (BOS-1218).
func TestPlatformMcpGetStatusRequiresAPIDBeforeReportingRunning(t *testing.T) {
	mcpOutput := func(body string) string {
		return strings.ReplaceAll(body, Label, McpLabel)
	}
	for _, tc := range []struct {
		name         string
		out          string
		wantRunning  bool
		wantPID      int
		wantPIDKnown bool
	}{
		{
			name:         "registered with no PID is not running",
			out:          mcpOutput(launchctlRegisteredNoPIDOutput),
			wantPIDKnown: true,
		},
		{
			name:         "a reported PID is what makes it running",
			out:          mcpOutput(launchctlRunningOutput),
			wantRunning:  true,
			wantPID:      80034,
			wantPIDKnown: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareLaunchAgentStatusEnvironment(t)
			stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
				if len(args) < 2 || args[1] != McpLabel {
					t.Fatalf("launchctl called with %v, want the MCP label", args)
				}
				return []byte(tc.out), nil
			})

			st, err := platformMcpGetStatus()
			if err != nil {
				t.Fatalf("platformMcpGetStatus: %v", err)
			}
			if st.Running != tc.wantRunning {
				t.Fatalf("Running = %v, want %v (PID %d)", st.Running, tc.wantRunning, st.PID)
			}
			if st.PID != tc.wantPID {
				t.Fatalf("PID = %d, want %d", st.PID, tc.wantPID)
			}
			if st.PIDKnown != tc.wantPIDKnown {
				t.Fatalf("PIDKnown = %v, want %v", st.PIDKnown, tc.wantPIDKnown)
			}
		})
	}
}

// TestStillRunningProbesReportStoppedForARegisteredJobWithNoPID is the other
// half: the ordinary already-stopped case must still verify cleanly, and a
// registered job that owns no process now joins it.
func TestStillRunningProbesReportStoppedForARegisteredJobWithNoPID(t *testing.T) {
	prepareLaunchAgentStatusEnvironment(t)
	stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		return []byte(strings.ReplaceAll(launchctlRegisteredNoPIDOutput, Label, args[1])), nil
	})
	if bossdStillRunningProbe() {
		t.Fatalf("bossdStillRunningProbe = true for a job launchd reported no PID for")
	}
	if mcpStillRunningProbe() {
		t.Fatalf("mcpStillRunningProbe = true for a job launchd reported no PID for")
	}
}

// stubJobDisabledProbe installs a recording runLaunchctlBoundedProbe fake,
// following the same save / reassign / t.Cleanup-restore shape
// stubRestartLaunchctl uses for runLaunchctl.
//
// It stubs the BOUNDED seam and not runLaunchctl, which is the point of that
// seam existing: the lifecycle verbs keep their own unbounded latency, and the
// advisory diagnostic reads are the only thing under a deadline (BOS-1222 R7).
func stubJobDisabledProbe(t *testing.T, respond func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	original := runLaunchctlBoundedProbe
	calls := &[][]string{}
	runLaunchctlBoundedProbe = func(args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return respond(args)
	}
	t.Cleanup(func() { runLaunchctlBoundedProbe = original })
	return calls
}

// TestPlatformJobDisabled covers the launchd wiring around
// parseLaunchdDisabledServices: the domain it asks about, the
// BOSS_DAEMON_SKIP_LAUNCHCTL short-circuit, the deadline, and the two
// launchctl failure shapes. The classification itself is exercised
// platform-agnostically in spawnhistory_test.go, so what is left to prove here
// is that the probe asks launchd the right question and fails closed — never
// "enabled" — when it does not get an answer. BOS-1222.
func TestPlatformJobDisabled(t *testing.T) {
	wantDomain := "gui/" + strconv.Itoa(os.Getuid())

	t.Run("a disabled job is established", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		calls := stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			return []byte("disabled services = {\n\t\"" + Label + "\" => true\n}\n"), nil
		})

		got, err := platformJobDisabled()
		if err != nil {
			t.Fatalf("platformJobDisabled: %v", err)
		}
		if got.State != JobDisabledStateDisabled {
			t.Errorf("State = %q, want %q", got.State, JobDisabledStateDisabled)
		}
		if got.Domain != wantDomain || got.Label != Label {
			t.Errorf("Domain/Label = %q/%q, want %q/%q", got.Domain, got.Label, wantDomain, Label)
		}
		wantArgs := []string{"print-disabled", wantDomain}
		if len(*calls) != 1 || (*calls)[0][0] != wantArgs[0] || (*calls)[0][1] != wantArgs[1] {
			t.Errorf("probe args = %q, want exactly one %q", *calls, wantArgs)
		}
	})

	t.Run("an explicit false override rules the cause out", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			return []byte("disabled services = {\n\t\"" + Label + "\" => false\n}\n"), nil
		})

		got, err := platformJobDisabled()
		if err != nil {
			t.Fatalf("platformJobDisabled: %v", err)
		}
		if got.State != JobDisabledStateEnabled {
			t.Errorf("State = %q, want %q", got.State, JobDisabledStateEnabled)
		}
	})

	t.Run("a label absent from the dump carries no override", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			return []byte("disabled services = {\n\t\"com.example.other\" => true\n}\n"), nil
		})

		got, err := platformJobDisabled()
		if err != nil {
			t.Fatalf("platformJobDisabled: %v", err)
		}
		if got.State != JobDisabledStateEnabled {
			t.Errorf("State = %q, want %q", got.State, JobDisabledStateEnabled)
		}
	})

	t.Run("a malformed dump is unknown, never enabled", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) { return nil, nil })

		got, err := platformJobDisabled()
		if err != nil {
			t.Fatalf("platformJobDisabled: %v", err)
		}
		if got.State != JobDisabledStateUnknown {
			t.Fatalf("State = %q for an empty dump, want %q", got.State, JobDisabledStateUnknown)
		}
		if got.Reason == "" {
			t.Error("an unknown verdict must carry a Reason")
		}
		if got.Domain != wantDomain {
			t.Errorf("Domain = %q, want %q even on the unreadable path", got.Domain, wantDomain)
		}
	})

	t.Run("a launchctl that ran and refused is unknown with a nil error", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			return []byte("Could not find domain for\n"), fakeExitError(t, 113)
		})

		got, err := platformJobDisabled()
		if err != nil {
			t.Fatalf("want a nil error for a launchctl that ran and refused, got %v", err)
		}
		if got.State != JobDisabledStateUnknown {
			t.Errorf("State = %q, want %q", got.State, JobDisabledStateUnknown)
		}
		if !strings.Contains(got.Reason, "Could not find domain") {
			t.Errorf("Reason = %q, want it to carry the launchctl output", got.Reason)
		}
	})

	t.Run("a launchctl that could not be executed returns an error", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			return nil, errors.New(`exec: "launchctl": executable file not found in $PATH`)
		})

		got, err := platformJobDisabled()
		if err == nil {
			t.Fatal("platformJobDisabled() = nil error when launchctl could not be executed, want an error")
		}
		if got.State != JobDisabledStateUnknown {
			t.Errorf("State = %q, want %q even on the error path", got.State, JobDisabledStateUnknown)
		}
		if got.Domain != wantDomain {
			t.Errorf("Domain = %q, want %q even on the error path", got.Domain, wantDomain)
		}
	})

	t.Run("a probe past its deadline degrades to not-checked and names the bound", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			return nil, fmt.Errorf("launchctl print-disabled did not answer within %s: %w",
				launchctlProbeTimeout, context.DeadlineExceeded)
		})

		got, err := platformJobDisabled()
		// A wedged launchd must never hold doctor open, and must never be
		// reported as an execution failure either: it is "we asked and could
		// not tell" (R7).
		if err != nil {
			t.Fatalf("a deadline expiry must be a populated fail-closed value, not an error: %v", err)
		}
		if got.State != JobDisabledStateUnknown {
			t.Errorf("State = %q, want %q", got.State, JobDisabledStateUnknown)
		}
		if !strings.Contains(got.Reason, launchctlProbeTimeout.String()) {
			t.Errorf("Reason = %q, want it to name the %s bound", got.Reason, launchctlProbeTimeout)
		}
	})

	t.Run("skip_launchctl does not shell out", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
			t.Error("runLaunchctlBoundedProbe must not be called when BOSS_DAEMON_SKIP_LAUNCHCTL is set")
			return nil, nil
		})

		got, err := platformJobDisabled()
		if err != nil {
			t.Fatalf("platformJobDisabled: %v", err)
		}
		if got.State != JobDisabledStateUnknown {
			t.Errorf("State = %q, want %q", got.State, JobDisabledStateUnknown)
		}
		if !strings.Contains(got.Reason, "BOSS_DAEMON_SKIP_LAUNCHCTL") {
			t.Errorf("Reason = %q, want it to name BOSS_DAEMON_SKIP_LAUNCHCTL", got.Reason)
		}
		if got.Domain != wantDomain {
			t.Errorf("Domain = %q, want the short-circuit to still name the domain it would have probed", got.Domain)
		}
	})
}

// TestGetJobDisabledDelegates proves the exported entry point is wired to the
// platform probe rather than to a default-valued zero struct — a zero
// JobDisabled has an empty State, which no caller should ever be handed.
func TestGetJobDisabledDelegates(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	stubJobDisabledProbe(t, func(_ []string) ([]byte, error) {
		return []byte("disabled services = {\n\t\"" + Label + "\" => true\n}\n"), nil
	})

	got, err := GetJobDisabled()
	if err != nil {
		t.Fatalf("GetJobDisabled: %v", err)
	}
	if got.State != JobDisabledStateDisabled {
		t.Fatalf("State = %q, want %q", got.State, JobDisabledStateDisabled)
	}
}

// TestRunLaunchctlBoundedProbeWrapsARealDeadlineExpiry drives the PRODUCTION
// runLaunchctlBoundedProbe past a real deadline, which is the one thing every
// other deadline test on this path cannot do: they install a stub that
// hand-fabricates `fmt.Errorf("...: %w", context.DeadlineExceeded)`, so they
// would pass byte-identically against a %v wrap here, or against the ctx.Err()
// check deleted outright — at which point a wedged launchd would fall through
// to the non-*exec.ExitError arm and platformJobDisabled would start returning
// a non-nil error, contradicting the "a deadline expiry is nil-error with an
// unknown verdict" contract (R7) the callers are built on.
//
// The bound is shrunk to a nanosecond rather than to a small millisecond
// count: that is deterministic — the context is already expired by the time
// exec reaches Start — so no launchctl is spawned, the test cannot race a fast
// host, and it costs nothing on a wedged one.
func TestRunLaunchctlBoundedProbeWrapsARealDeadlineExpiry(t *testing.T) {
	original := launchctlProbeTimeout
	launchctlProbeTimeout = time.Nanosecond
	t.Cleanup(func() { launchctlProbeTimeout = original })

	_, err := runLaunchctlBoundedProbe("print-disabled", "gui/501")
	if err == nil {
		t.Fatal("runLaunchctlBoundedProbe past its deadline returned a nil error")
	}
	// The %w wrap is what platformJobDisabled's errors.Is branch keys on.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	// The ctx.Err() check is what turns exec's own error into a sentence that
	// names the timeout; without it the caller cannot name the bound.
	for _, want := range []string{"print-disabled gui/501", "did not answer within", launchctlProbeTimeout.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err.Error(), want)
		}
	}

	// The classifier above it must read that error as "we asked and could not
	// tell", never as an execution failure.
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	got, err := platformJobDisabled()
	if err != nil {
		t.Fatalf("a real deadline expiry must be a populated fail-closed value, not an error: %v", err)
	}
	if got.State != JobDisabledStateUnknown {
		t.Errorf("State = %q, want %q", got.State, JobDisabledStateUnknown)
	}
}
