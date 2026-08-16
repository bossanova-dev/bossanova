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

	originalExecutablePath := executablePath
	executablePath = func() (string, error) {
		return filepath.Join(filepath.Dir(sourcePath), "boss"), nil
	}
	t.Cleanup(func() { executablePath = originalExecutablePath })

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

// countBootstrapCalls counts the recorded `launchctl bootstrap` invocations,
// which is what pins platformRestart's retry as bounded.
func countBootstrapCalls(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if len(call) > 0 && call[0] == "bootstrap" {
			count++
		}
	}
	return count
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
	originalExecutablePath := executablePath
	executablePath = func() (string, error) {
		return filepath.Join(filepath.Dir(sourcePath), "boss"), nil
	}
	t.Cleanup(func() { executablePath = originalExecutablePath })

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
