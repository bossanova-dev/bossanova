package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestResolveBossdPath(t *testing.T) {
	// Create a temp directory with a bossd binary.
	tmpDir := t.TempDir()
	bossdPath := filepath.Join(tmpDir, "bossd")
	if err := os.WriteFile(bossdPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create temp bossd: %v", err)
	}

	// Temporarily add tmpDir to PATH.
	origPath := os.Getenv("PATH")
	defer func() { _ = os.Setenv("PATH", origPath) }()
	if err := os.Setenv("PATH", tmpDir+":"+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	// ResolveBossdPath should find it in PATH.
	found, err := ResolveBossdPath()
	if err != nil {
		t.Fatalf("ResolveBossdPath: %v", err)
	}

	// The path should be absolute and point to our temp bossd.
	if !filepath.IsAbs(found) {
		t.Errorf("expected absolute path, got %s", found)
	}
	if filepath.Base(found) != "bossd" {
		t.Errorf("expected bossd, got %s", filepath.Base(found))
	}
}

func TestIsSocketReachable(t *testing.T) {
	// Test with a non-existent socket.
	if isSocketReachable("/tmp/nonexistent-socket-12345.sock") {
		t.Error("expected socket to be unreachable")
	}
}

// TestIsSocketReachable_LiveSocket verifies that isSocketReachable returns
// true for a real, accepting Unix socket. This exercises the dial-timeout
// arithmetic on daemon.go:110 (`500*time.Millisecond`).
//
// Catches daemon.go:110 ARITHMETIC_BASE: if the `*` is swapped to `-`, the
// expression becomes `500-time.Millisecond`, a *negative* duration. A
// negative timeout makes net.DialTimeout fail immediately (deadline already
// expired) even against a live, accepting socket — so isSocketReachable
// would wrongly return false. The real 500ms timeout connects successfully
// and returns true, so this assertion fails under the mutant and passes
// against the real code.
func TestIsSocketReachable_LiveSocket(t *testing.T) {
	// Use a short socket path: macOS limits sun_path to ~104 bytes, and
	// t.TempDir() paths can be long.
	sockPath := filepath.Join("/tmp", "bs-"+strconv.Itoa(os.Getpid())+".sock")
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove stale socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sockPath) })

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Accept and immediately close connections so dials succeed.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if !isSocketReachable(sockPath) {
		t.Errorf("expected live accepting socket %q to be reachable", sockPath)
	}
}

func TestWaitForSocket(t *testing.T) {
	// Test timeout with non-existent socket.
	start := time.Now()
	if waitForSocket("/tmp/nonexistent-socket-12345.sock", 100*time.Millisecond) {
		t.Error("expected waitForSocket to timeout")
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("expected timeout >= 100ms, got %v", elapsed)
	}
}

func TestWaitForSocketReturnsTrueWhenSocketBecomesReachable(t *testing.T) {
	// Short socket path: macOS limits sun_path to ~104 bytes.
	sockPath := filepath.Join("/tmp", "bs-wait-ok-"+strconv.Itoa(os.Getpid())+".sock")
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove stale socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sockPath) })

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if !waitForSocket(sockPath, 2*time.Second) {
		t.Fatalf("expected waitForSocket to observe reachable socket %q", sockPath)
	}
}

func TestLifecycleWaitTimeoutsCoverDaemonStartupAndShutdown(t *testing.T) {
	// bossd's graceful shutdown removes the socket only after draining the cron
	// scheduler (bounded ~10s) and shutting down the gRPC server (~5s). The
	// shutdown wait must comfortably cover that worst case so a slow drain does
	// not produce a false "timed out waiting for socket to stop" error. Keep
	// this coupled to bossd's shutdown budget (services/bossd/cmd/main.go): if
	// those timeouts grow, this floor should grow with them.
	const bossdShutdownBudget = 15 * time.Second
	if LifecycleShutdownTimeout < bossdShutdownBudget {
		t.Fatalf("LifecycleShutdownTimeout = %v, want >= %v to cover bossd graceful shutdown", LifecycleShutdownTimeout, bossdShutdownBudget)
	}

	// Startup can involve plugin launch and initial work before the socket
	// accepts connections; give it at least as long as shutdown.
	if LifecycleStartupTimeout < LifecycleShutdownTimeout {
		t.Fatalf("LifecycleStartupTimeout = %v, want >= LifecycleShutdownTimeout = %v", LifecycleStartupTimeout, LifecycleShutdownTimeout)
	}

	// The poll interval must be small relative to the timeouts so waits resolve
	// promptly once the socket flips state, and it must be positive.
	if LifecyclePollInterval <= 0 {
		t.Fatalf("LifecyclePollInterval = %v, want > 0", LifecyclePollInterval)
	}
	if LifecyclePollInterval*10 > LifecycleShutdownTimeout {
		t.Fatalf("LifecyclePollInterval = %v is too coarse relative to LifecycleShutdownTimeout = %v", LifecyclePollInterval, LifecycleShutdownTimeout)
	}
}

func TestRestartSkipsServiceManagerWhenEnvSet(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	if err := Restart(); err != nil {
		t.Fatalf("Restart with skip env: %v", err)
	}
}

// TestStopSkipsServiceManagerWhenEnvSet mirrors the Restart test: when the
// skip-launchctl env is set, Stop must be a no-op rather than shelling out
// to launchctl/systemctl. Without the env handling, this test would hit the
// real service manager and either fail or worse, stop a real daemon.
func TestStopSkipsServiceManagerWhenEnvSet(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	if err := Stop(); err != nil {
		t.Fatalf("Stop with skip env: %v", err)
	}
}

// TestSkipLaunchctl pins the env gate that lets tests exercise file-writing
// behaviour without touching the host's service manager. skipLaunchctl must
// report true exactly when BOSS_DAEMON_SKIP_LAUNCHCTL is non-empty.
//
// Catches daemon.go:40 CONDITIONALS_NEGATION (`!= ""` → `== ""`): the mutant
// inverts the gate, so every launchctl/systemctl call site would flip. The
// indirect Restart/Stop tests don't catch it because the platform stubs
// swallow "not loaded" errors and still return nil under the mutant; only a
// direct assertion on the boolean distinguishes the two.
func TestSkipLaunchctl(t *testing.T) {
	t.Run("non-empty value reports true", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		if !skipLaunchctl() {
			t.Error("skipLaunchctl() = false, want true when env is set")
		}
	})
	t.Run("empty value reports false", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		if skipLaunchctl() {
			t.Error("skipLaunchctl() = true, want false when env is empty")
		}
	})
}

// TestResolveMcpPath_NotFound verifies that ResolveMcpPath returns a non-nil
// error when neither "boss-mcp" nor "mcp" is present next to the test binary
// or on PATH.
func TestResolveMcpPath_NotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ResolveMcpPath()
	if err == nil {
		t.Fatal("ResolveMcpPath() returned nil error when no binary is found")
	}
}

// TestResolveMcpPath_FindsBossMcpOnPath verifies that ResolveMcpPath returns
// the "boss-mcp" binary when it is present on PATH.
func TestResolveMcpPath_FindsBossMcpOnPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "boss-mcp"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create boss-mcp: %v", err)
	}
	t.Setenv("PATH", dir)
	got, err := ResolveMcpPath()
	if err != nil {
		t.Fatalf("ResolveMcpPath: %v", err)
	}
	if filepath.Base(got) != "boss-mcp" {
		t.Fatalf("ResolveMcpPath() = %q, want path with base \"boss-mcp\"", got)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("ResolveMcpPath() = %q, want absolute path", got)
	}
}

// TestResolveBossdPath_PrefersExecutableDir verifies that ResolveBossdPath
// returns the bossd binary that lives next to the running executable,
// even when a different bossd is also on PATH.
//
// Catches:
//   - daemon.go:79 CONDITIONALS_NEGATION (`err == nil` → `err != nil`):
//     mutated code would skip the executable-dir branch entirely.
//   - daemon.go:82 CONDITIONALS_NEGATION (`err == nil` → `err != nil`):
//     mutated code would not return the candidate even when it exists.
func TestResolveBossdPath_PrefersExecutableDir(t *testing.T) {
	// Determine the directory where the test binary lives.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable not available: %v", err)
	}
	exeDir := filepath.Dir(exe)
	candidate := filepath.Join(exeDir, "bossd")

	// Skip if a real bossd already lives next to the test binary — we don't
	// want to clobber it. Otherwise create a fake one and clean up after.
	if _, err := os.Stat(candidate); err == nil {
		t.Skipf("bossd already exists at %s; skipping to avoid clobber", candidate)
	}
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create executable-dir bossd: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(candidate) })

	// Put a *different* bossd on PATH at a separate location to ensure the
	// function chooses the executable-dir one.
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "bossd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create PATH bossd: %v", err)
	}
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	if err := os.Setenv("PATH", pathDir+":"+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	got, err := ResolveBossdPath()
	if err != nil {
		t.Fatalf("ResolveBossdPath: %v", err)
	}
	if got != candidate {
		t.Errorf("ResolveBossdPath = %q, want %q (executable-dir bossd should win over PATH)", got, candidate)
	}
}
