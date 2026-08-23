//go:build darwin

package daemon

import (
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

	if err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
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

	if err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
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
	if err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
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
	if err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("a staging failure turned a working start into a hard failure: %v", err)
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

	if err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("fallback re-resolved and re-staged instead of reusing the refresh it had just done: %v", err)
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
	if err := platformEnsureRunning(socketPath); err != nil {
		t.Fatalf("platformEnsureRunning: %v", err)
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
