//go:build darwin

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Environment for the probe's two child halves. The parent re-executes this
// test binary once per half, each as a different unprivileged uid.
const (
	reusePortMeasureEnv = "BOSS_MEASURE_REUSEPORT"
	reusePortRoleEnv    = "BOSS_REUSEPORT_PROBE_ROLE"
	reusePortPortEnv    = "BOSS_REUSEPORT_PROBE_PORT"

	reusePortRoleFirst  = "first"
	reusePortRoleSecond = "second"

	reusePortLinePort  = "REUSEPORT_PROBE_PORT="
	reusePortLineBound = "REUSEPORT_PROBE_SECOND_BIND="
)

// TestDarwinReusePortCrossUserCoBind measures whether a socket owned by one
// user can co-bind 127.0.0.1:<port> that another user's socket holds, both
// going through reusableSocketControl (SO_REUSEADDR + SO_REUSEPORT). It logs
// the macOS version and the result, and fails when the result contradicts
// darwinReusePortAllowsCrossUserCoBind.
//
// It needs root to drop each half to a different uid, so it skips unless run
// as root with BOSS_MEASURE_REUSEPORT=1 (see the constant's comment for the
// exact command). Skipped in CI by design.
func TestDarwinReusePortCrossUserCoBind(t *testing.T) {
	switch os.Getenv(reusePortRoleEnv) {
	case reusePortRoleFirst:
		reusePortProbeFirst(t)
		return
	case reusePortRoleSecond:
		reusePortProbeSecond(t)
		return
	}
	if os.Getenv(reusePortMeasureEnv) != "1" || os.Geteuid() != 0 {
		t.Skipf("measurement probe: run as root with %s=1 (sudo %s=1 go test -run TestDarwinReusePortCrossUserCoBind ./internal/server/)",
			reusePortMeasureEnv, reusePortMeasureEnv)
	}

	firstUser := lookupProbeUser(t, "nobody")
	secondUser := lookupProbeUser(t, "daemon")
	if firstUser.Uid == secondUser.Uid {
		t.Fatalf("probe users share uid %d; need two different uids", firstUser.Uid)
	}
	binary := probeBinaryCopy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First half: bind and listen, report the port, hold it until stdin closes.
	first := exec.CommandContext(ctx, binary, "-test.run=^TestDarwinReusePortCrossUserCoBind$")
	first.Dir = filepath.Dir(binary)
	first.Env = append(os.Environ(), reusePortRoleEnv+"="+reusePortRoleFirst)
	first.SysProcAttr = &syscall.SysProcAttr{Credential: firstUser}
	first.Stderr = os.Stderr
	hold, err := first.StdinPipe()
	if err != nil {
		t.Fatalf("first half stdin: %v", err)
	}
	firstOut, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("first half stdout: %v", err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("start first half as uid %d: %v", firstUser.Uid, err)
	}
	defer func() {
		_ = hold.Close()
		_ = first.Wait()
	}()
	port := readProbeLine(t, firstOut, reusePortLinePort)

	// Second half: a different uid tries the same port through the same control.
	second := exec.CommandContext(ctx, binary, "-test.run=^TestDarwinReusePortCrossUserCoBind$")
	second.Dir = filepath.Dir(binary)
	second.Env = append(os.Environ(), reusePortRoleEnv+"="+reusePortRoleSecond, reusePortPortEnv+"="+port)
	second.SysProcAttr = &syscall.SysProcAttr{Credential: secondUser}
	second.Stderr = os.Stderr
	secondOut, err := second.StdoutPipe()
	if err != nil {
		t.Fatalf("second half stdout: %v", err)
	}
	if err := second.Start(); err != nil {
		t.Fatalf("start second half as uid %d: %v", secondUser.Uid, err)
	}
	result := readProbeLine(t, secondOut, reusePortLineBound)
	_ = second.Wait()

	allowed := result == "ok"
	version, _ := exec.Command("sw_vers", "-productVersion").Output()
	t.Logf("macOS %s, %s: cross-user SO_REUSEPORT co-bind of 127.0.0.1:%s (uid %d then uid %d): second bind %s",
		strings.TrimSpace(string(version)), time.Now().UTC().Format("2006-01-02"), port,
		firstUser.Uid, secondUser.Uid, result)
	if allowed != darwinReusePortAllowsCrossUserCoBind {
		t.Fatalf("measured cross-user co-bind allowed=%t, but darwinReusePortAllowsCrossUserCoBind=%t; apply the decision rule in proxy_listen_control_darwin.go",
			allowed, darwinReusePortAllowsCrossUserCoBind)
	}
}

// reusePortProbeFirst is the first child half.
func reusePortProbeFirst(t *testing.T) {
	lc := net.ListenConfig{Control: reusableSocketControl}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first half bind: %v", err)
	}
	defer func() { _ = ln.Close() }()
	fmt.Printf("%s%d\n", reusePortLinePort, ln.Addr().(*net.TCPAddr).Port)
	// Hold the listener until the parent closes stdin.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// reusePortProbeSecond is the second child half.
func reusePortProbeSecond(t *testing.T) {
	port := os.Getenv(reusePortPortEnv)
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("second half: bad port %q", port)
	}
	lc := net.ListenConfig{Control: reusableSocketControl}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Printf("%srefused (%v)\n", reusePortLineBound, err)
		return
	}
	_ = ln.Close()
	fmt.Printf("%sok\n", reusePortLineBound)
}

func lookupProbeUser(t *testing.T, name string) *syscall.Credential {
	t.Helper()
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatalf("look up probe user %q: %v", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		t.Fatalf("probe user %q uid %q: %v", name, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		t.Fatalf("probe user %q gid %q: %v", name, u.Gid, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
}

// probeBinaryCopy copies the running test binary somewhere the unprivileged
// probe users can execute it: under sudo, go test builds it inside root's
// private temp directory.
func probeBinaryCopy(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	dir, err := os.MkdirTemp("", "boss-reuseport-probe-")
	if err != nil {
		t.Fatalf("probe dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod probe dir: %v", err)
	}
	dst := filepath.Join(dir, "probe.test")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("write probe binary: %v", err)
	}
	return dst
}

// readProbeLine returns the value of the first stdout line carrying prefix.
func readProbeLine(t *testing.T, r io.Reader, prefix string) string {
	t.Helper()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// What the child prints after this line is a few bytes of test
		// summary, well under a pipe buffer, so it never blocks unread.
		if v, ok := strings.CutPrefix(sc.Text(), prefix); ok {
			return v
		}
	}
	t.Fatalf("probe child exited without a %q line (scan err %v)", prefix, sc.Err())
	return ""
}
