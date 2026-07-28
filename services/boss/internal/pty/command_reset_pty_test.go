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

// TestPTYCommandEmitsMouseResetOnDetach drives a real-PTY detach and asserts
// that PTYCommand.Run()'s teardown writes the xterm mouse-tracking disable
// sequence to its stdout on the wire (BOS-499). It mirrors the harness in
// command_detach_pty_test.go — real creackpty pair, //go:build !race, and the
// same 15s/30s deadlines — because it shares the same real-OS-PTY + select(2)
// scheduling sensitivity that starves under the race runner. The deterministic
// byte-emission guard is TestWriteTerminalModeReset (which runs under -race);
// this test proves the teardown actually puts the reset on the terminal.
func TestPTYCommandEmitsMouseResetOnDetach(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}

	// Put the fake terminal in raw mode before Run starts, matching the
	// sibling detach test's rationale (avoid canonical mode swallowing the
	// detach byte on a loaded runner).
	oldState, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatalf("make test pty raw: %v", err)
	}

	// Continuously drain the master side into a synchronized buffer so the
	// slave-side teardown writes never block on a full PTY buffer, and so the
	// reset bytes are captured as they arrive.
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

	mgr := NewManager()
	cmd := exec.Command("cat")
	pcmd := NewPTYCommand(mgr, "test-reset-detach", cmd)
	pcmd.inputReady = make(chan struct{})
	pcmd.SetStdin(slave)
	pcmd.SetStdout(slave)
	pcmd.SetStderr(slave)

	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = pcmd.Run()
		close(runDone)
	}()

	// Ordered cleanup: kill cat → join Run() (which joins the stdin reader on
	// slave.Fd) → close PTY (unblocks the master drain goroutine).
	defer func() {
		if p, ok := mgr.Get("test-reset-detach"); ok {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Logf("Run did not return within 15s of cat kill — closing PTY anyway")
		}
		_ = term.Restore(int(slave.Fd()), oldState)
		_ = slave.Close()
		_ = master.Close()
		<-readerDone
	}()

	select {
	case <-pcmd.inputReady:
	case <-time.After(5 * time.Second):
		t.Fatal("PTYCommand did not start reading input within 5s")
	}

	// modifyOtherKeys=2 Ctrl+X — the form that arrives in practice.
	if _, err := master.Write([]byte("\x1b[27;5;120~")); err != nil {
		t.Fatalf("write detach bytes: %v", err)
	}

	select {
	case <-runDone:
		if runErr != nil {
			t.Fatalf("Run returned error: %v", runErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("PTYCommand did not return within 30s of detach byte")
	}
	if !pcmd.Detached {
		t.Fatal("expected Detached=true after detach byte")
	}

	// The teardown write happens in a defer that runs before Run() returns, but
	// the bytes still have to propagate through the PTY to the drain goroutine.
	// Poll the captured buffer for the SGR mouse-reset sentinel (?1006l).
	sentinel := []byte("\x1b[?1006l")
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		found := bytes.Contains(captured.Bytes(), sentinel)
		mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := captured.String()
			mu.Unlock()
			t.Fatalf("mouse-reset sentinel %q not written on teardown; captured %q", sentinel, got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
