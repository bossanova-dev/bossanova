package plugin_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"github.com/recurser/bossalib/config"
	sharedplugin "github.com/recurser/bossalib/plugin"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
)

// This file is the re-exec intermediate parent the orphan proof needs.
//
// Killing the host-side go-plugin client orphans nothing: the plugin
// subprocess's parent is still the `go test` process, which stays alive, so
// os.Getppid never changes, the watchdog correctly never fires, and a test
// written that way hangs to its deadline while proving nothing. Producing a
// genuine orphan requires killing the process that really is the plugin's
// parent, so the test re-execs its own binary (os.Args[0] plus a sentinel env
// var — the standard helper-process pattern) into a mode that launches the
// plugin, reports its PID, and blocks. The test then SIGKILLs that
// intermediate.
const (
	// orphanSpawnerEnv switches a re-exec of the test binary into spawner mode.
	orphanSpawnerEnv = "BOSS_TEST_ORPHAN_SPAWNER"
	// orphanPluginBinEnv hands the spawner the plugin binary to launch.
	orphanPluginBinEnv = "BOSS_TEST_ORPHAN_PLUGIN_BIN"
	// orphanStampEnv is "1" when the spawner should stamp its own identity the
	// way launchPlugin does, and "0" for the fail-open control.
	orphanStampEnv = "BOSS_TEST_ORPHAN_STAMP"

	// orphanPIDMarker prefixes the line the spawner writes to stdout once the
	// plugin has handshaked. The line carries the plugin's PID and whether its
	// watchdog actually armed: "ORPHAN_PLUGIN_PID=<pid> ARMED=<0|1>".
	orphanPIDMarker = "ORPHAN_PLUGIN_PID="

	// orphanArmedField labels the armed flag on that line. Arming is invisible
	// from outside the plugin process, so without it every control here passes
	// identically against a build whose stamp was shadowed and whose watchdog
	// therefore never armed -- including the graceful-kill control, which
	// client.Kill reaps by SIGKILL regardless.
	orphanArmedField = "ARMED="

	// orphanArmWindow bounds how long the spawner waits for the plugin to
	// report arming. The plugin arms before it serves, so the marker is
	// already written by the time the handshake returns; this only covers
	// go-plugin's asynchronous stderr pump.
	orphanArmWindow = 10 * time.Second

	// orphanSpawnerMaxLife bounds the spawner's own life so a test that dies
	// before its cleanup runs cannot leak the very thing this ticket is about.
	orphanSpawnerMaxLife = 2 * time.Minute
)

// TestHelperOrphanSpawner is not a test. It is the re-exec entry point: under
// the sentinel env var it launches a real plugin subprocess through go-plugin
// (so the plugin is genuinely this process's child), prints the plugin's PID,
// and blocks until it is killed or orphanSpawnerMaxLife elapses. Its name
// deliberately does not match the TestOrphan prefix the acceptance criteria
// run, so it never appears as a skipped case there.
func TestHelperOrphanSpawner(t *testing.T) {
	if os.Getenv(orphanSpawnerEnv) == "" {
		t.Skip("not the re-exec orphan spawner")
	}

	binPath := os.Getenv(orphanPluginBinEnv)
	if binPath == "" {
		fmt.Fprintln(os.Stderr, "spawner: no plugin binary supplied")
		os.Exit(2)
	}

	// #nosec G204 -- test harness re-exec: binPath is a plugin binary this same
	// test built or resolved from runfiles, never attacker-controlled.
	cmd := exec.Command(binPath)
	cfg := config.PluginConfig{Name: "stub-runner"}
	if os.Getenv(orphanStampEnv) == "1" {
		// Exactly what launchPlugin does, including clearing the variable from
		// this process's own environment so go-plugin's second os.Environ()
		// append cannot shadow the stamp.
		cmd.Env = pluginpkg.PluginSubprocessEnv(cfg, os.Getpid())
	} else {
		// The fail-open control: no parent identity reaches the plugin at all.
		_ = os.Unsetenv(sharedplugin.ParentPIDEnvVar)
		cmd.Env = os.Environ()
	}

	watcher := &armedWatcher{}
	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: pluginpkg.NewHandshake("orphan-test-cookie"),
		Plugins: goplugin.PluginSet{
			sharedplugin.PluginTypeAgentRunner: pluginpkg.NewAgentRunnerGRPCPlugin(hostService),
		},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		// go-plugin forwards the plugin's stderr here line by line, which is
		// the only channel through which the plugin's own view of whether it
		// armed reaches this process.
		Stderr: watcher,
	})

	if _, err := client.Client(); err != nil {
		fmt.Fprintf(os.Stderr, "spawner: handshake failed: %v\n", err)
		client.Kill()
		os.Exit(3)
	}

	rc := client.ReattachConfig()
	if rc == nil || rc.Pid <= 0 {
		fmt.Fprintln(os.Stderr, "spawner: no plugin pid after handshake")
		client.Kill()
		os.Exit(4)
	}

	armed := "0"
	if watcher.waitArmed(orphanArmWindow) {
		armed = "1"
	}
	fmt.Printf("%s%d %s%s\n", orphanPIDMarker, rc.Pid, orphanArmedField, armed)

	// Block. The test SIGKILLs this process to orphan the plugin; the deadline
	// is only a backstop against a test that died before its cleanup ran.
	time.Sleep(orphanSpawnerMaxLife)
	client.Kill()
	os.Exit(5)
}

// orphanSpawner is a running intermediate parent plus the PID of the real
// plugin subprocess it owns.
type orphanSpawner struct {
	cmd       *exec.Cmd
	pluginPID int
	// armed is the plugin's own report of whether its parent-death watchdog
	// armed, read off the plugin's stderr rather than assumed from the inputs.
	armed bool
}

// armedWatcher scrapes a plugin subprocess's stderr for the watchdog's armed
// marker. It is an io.Writer because that is the shape go-plugin's
// ClientConfig.Stderr takes; it never forwards, since the plugin's log lines
// are noise for these tests.
type armedWatcher struct {
	mu    sync.Mutex
	armed bool
}

var _ io.Writer = (*armedWatcher)(nil)

func (w *armedWatcher) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(sharedplugin.ParentWatchArmedMsg)) {
		w.mu.Lock()
		w.armed = true
		w.mu.Unlock()
	}
	return len(p), nil
}

// waitArmed polls until the marker shows up or the budget expires. A false
// answer is meaningful, not merely "not yet": the plugin logs the marker
// before it serves, so a handshaked plugin that has not reported arming
// within the budget did not arm.
func (w *armedWatcher) waitArmed(budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		w.mu.Lock()
		armed := w.armed
		w.mu.Unlock()
		if armed || time.Now().After(deadline) {
			return armed
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startOrphanSpawner re-execs this test binary in spawner mode, waits for the
// plugin PID line, and registers cleanup that kills both processes on every
// path — including the paths where the test itself fails.
func startOrphanSpawner(t *testing.T, binPath string, stampIdentity bool) *orphanSpawner {
	t.Helper()

	stamp := "0"
	if stampIdentity {
		stamp = "1"
	}

	// #nosec G204 -- re-exec of this very test binary (os.Args[0]).
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperOrphanSpawner", "-test.timeout=0")
	cmd.Env = append(os.Environ(),
		orphanSpawnerEnv+"=1",
		orphanPluginBinEnv+"="+binPath,
		orphanStampEnv+"="+stamp,
		// Bazel passes its --test_filter through TESTBRIDGE_TEST_ONLY, which the
		// Go test main turns into -test.run and would override ours. The other
		// two are rules_go's per-test report paths: the spawner is SIGKILLed by
		// design, and a premature-exit sentinel it left behind would be read as
		// the *parent* test crashing.
		"TESTBRIDGE_TEST_ONLY=",
		"XML_OUTPUT_FILE=",
		"TEST_PREMATURE_EXIT_FILE=",
	)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("spawner stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start spawner: %v", err)
	}

	sp := &orphanSpawner{cmd: cmd}
	t.Cleanup(func() {
		sp.kill(t)
		if sp.pluginPID > 0 {
			killPID(sp.pluginPID)
		}
	})

	type result struct {
		pid   int
		armed bool
		err   error
	}
	found := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, orphanPIDMarker) {
				continue
			}
			pidField, armedField, ok := strings.Cut(strings.TrimPrefix(line, orphanPIDMarker), " ")
			if !ok || !strings.HasPrefix(armedField, orphanArmedField) {
				found <- result{err: fmt.Errorf("spawner reported a malformed marker line %q", line)}
				return
			}
			pid, convErr := strconv.Atoi(pidField)
			found <- result{
				pid:   pid,
				armed: strings.TrimPrefix(armedField, orphanArmedField) == "1",
				err:   convErr,
			}
			return
		}
		found <- result{err: fmt.Errorf("spawner exited without reporting a plugin pid")}
	}()

	select {
	case r := <-found:
		if r.err != nil {
			t.Fatalf("spawner: %v", r.err)
		}
		if r.pid <= 0 {
			t.Fatalf("spawner reported non-positive plugin pid %d", r.pid)
		}
		sp.pluginPID = r.pid
		sp.armed = r.armed
	case <-time.After(90 * time.Second):
		t.Fatal("spawner did not report a plugin pid within 90s")
	}

	if !processAlive(sp.pluginPID) {
		t.Fatalf("plugin pid %d is not alive immediately after handshake", sp.pluginPID)
	}

	// The premise every caller below rests on, checked rather than assumed. A
	// stamped plugin that did not arm would make the survival control pass for
	// the wrong reason, and an unstamped plugin that did arm would make the
	// fail-open control pass while the compatibility case was broken.
	if sp.armed != stampIdentity {
		t.Fatalf("plugin pid %d reported armed=%v with stamped identity=%v; "+
			"the stamp did not reach the plugin's arm-time guard as intended",
			sp.pluginPID, sp.armed, stampIdentity)
	}
	return sp
}

// kill SIGKILLs the intermediate parent and reaps it, so it does not linger as
// a zombie of the test process. Killing the parent — not the host-side
// go-plugin client — is what actually orphans the plugin.
func (s *orphanSpawner) kill(t *testing.T) {
	t.Helper()
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	_, _ = s.cmd.Process.Wait()
	s.cmd.Process = nil
}

// processAlive reports whether pid names a live process. Signal 0 performs the
// existence and permission checks without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// killPID is best-effort cleanup for a process this test spawned indirectly.
func killPID(pid int) {
	if pid <= 0 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
}

// waitProcessGone polls until pid disappears, returning false if it is still
// there when the budget expires. Polling rather than sleeping a fixed interval
// keeps the passing case fast and the failing case informative.
func waitProcessGone(pid int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}
