//go:build !race

package pty

import (
	"bytes"
	"os/exec"
	"sync"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

// TestPTYCommandReassertsMouseModeOnReattach is the end-to-end guard for the
// unscrollable-pane bug, and the counterpart to
// TestPTYCommandEmitsMouseResetOnDetach: that test proves the teardown turns
// mouse reporting OFF on the real terminal, this one proves the NEXT attach
// turns it back on.
//
// Why it has to drive a real detach-then-reattach rather than assert on the flag
// (TestModesClobberedRoundTrip already does that): the bug was never in either
// half on its own. The reset was correct and deliberate, the process reuse was
// correct and deliberate, and the defect lived only in the seam between them —
// a reused `tmux attach` never re-runs its terminal init, so with nothing
// re-asserting, mouse reporting stayed off for the whole life of that attach.
// A unit test of the flag would pass with the wiring in Run() deleted, which is
// exactly the failure this must catch.
//
// Shares the sibling's constraints: real creackpty pair, //go:build !race, and
// the same generous deadlines, because the real-OS-PTY + select(2) scheduling
// starves under the race runner.
func TestPTYCommandReassertsMouseModeOnReattach(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	oldState, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatalf("make test pty raw: %v", err)
	}

	var mu sync.Mutex
	var captured bytes.Buffer
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				mu.Lock()
				captured.Write(buf[:n])
				mu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()

	const id = "test-reassert-reattach"
	mgr := NewManager()

	// runAttach starts one attach against the shared manager and returns a
	// channel closed when its Run returns. cmd is nil for a reattach, which is
	// what makes GetOrStart hand back the SAME live child.
	runAttach := func(cmd *exec.Cmd) chan struct{} {
		pcmd := NewPTYCommand(mgr, id, cmd)
		pcmd.inputReady = make(chan struct{})
		pcmd.SetStdin(slave)
		pcmd.SetStdout(slave)
		pcmd.SetStderr(slave)
		done := make(chan struct{})
		go func() {
			_ = pcmd.Run()
			close(done)
		}()
		select {
		case <-pcmd.inputReady:
		case <-time.After(5 * time.Second):
			t.Error("PTYCommand did not start reading input within 5s")
		}
		return done
	}

	detach := func(done chan struct{}) {
		t.Helper()
		// modifyOtherKeys=2 Ctrl+X — the form that arrives in practice.
		if _, werr := master.Write([]byte("\x1b[27;5;120~")); werr != nil {
			t.Fatalf("write detach bytes: %v", werr)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("PTYCommand did not return within 30s of detach byte")
		}
	}

	firstDone := runAttach(exec.Command("cat"))

	defer func() {
		if p, ok := mgr.Get(id); ok {
			_ = p.cmd.Process.Kill()
		}
		_ = term.Restore(int(slave.Fd()), oldState)
		_ = slave.Close()
		_ = master.Close()
		<-readerDone
	}()

	detach(firstDone)

	// The child must still be alive — the whole bug depends on it outliving the
	// attach. If it exited, the next attach would spawn a fresh one that runs its
	// own terminal init, and this test would be asserting nothing.
	if _, alive := mgr.Get(id); !alive {
		t.Fatal("child did not outlive the first attach; the reuse path is not being exercised")
	}

	// Drop everything captured so far so the assertion below cannot be satisfied
	// by bytes from the first attach.
	mu.Lock()
	captured.Reset()
	mu.Unlock()

	secondDone := runAttach(nil)

	enable := []byte("\x1b[?1006h")
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		found := bytes.Contains(captured.Bytes(), enable)
		mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := captured.String()
			mu.Unlock()
			t.Fatalf("re-attach did not re-assert mouse reporting: sentinel %q absent; captured %q", enable, got)
		case <-time.After(10 * time.Millisecond):
		}
	}

	detach(secondDone)
}
