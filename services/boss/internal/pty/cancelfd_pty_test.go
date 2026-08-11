//go:build !race

package pty

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

// TestRunTeardownCompletesWithHighCancelFd proves the cancel path survives a
// high-numbered descriptor, which is the condition under which it silently did
// not work.
//
// The setup is the whole point. Nothing about this scenario is exotic in
// production — a live boss TUI holds around thirty descriptors above 32, one pty
// master per backgrounded attach plus sockets and pipes, so a new attach's cancel
// pipe lands past 32 routinely. But a test process starts with a handful of
// descriptors, so the pipe always landed low and the broken fd_set math was
// exercised only in its accidentally-correct range. That is precisely why a bug
// that hangs real teardowns was invisible to the existing suite: the harness
// could not reach the range where it lives.
//
// So this test burns descriptors first to push the cancel pipe above the
// boundary, then drives the process-exit teardown — the path that depends on the
// cancel pipe, unlike a Ctrl+X detach which the reader handles itself — and
// requires Run() to actually return. With the 64-bit assumption in fdSet, the
// cancel bit is never set, select never wakes, and Run() blocks forever in
// wg.Wait().
func TestRunTeardownCompletesWithHighCancelFd(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	// Burn descriptors so the next pipe is allocated above the 32-bit word
	// boundary where the old math silently failed.
	//
	// These MUST be descriptors that never become readable, which is why they are
	// pipe read ends nobody writes to rather than the obvious os.DevNull. With the
	// broken math a high descriptor's bit lands in the word belonging to a
	// different, lower descriptor — and /dev/null is ALWAYS readable, so that
	// stray bit made select return immediately and the reader exit by accident.
	// The test then passed against the very bug it exists to catch. Unreadable
	// filler makes the aliased bit inert, so select blocks exactly as it does in
	// production and the hang actually reproduces.
	var burn []*os.File
	for len(burn) < 80 {
		r, w, err := os.Pipe()
		if err != nil {
			break
		}
		burn = append(burn, r, w)
		if int(w.Fd()) > 80 {
			break
		}
	}
	defer func() {
		for _, f := range burn {
			_ = f.Close()
		}
	}()
	if len(burn) == 0 || int(burn[len(burn)-1].Fd()) < 32 {
		t.Skip("could not push descriptors above 32 in this environment")
	}
	t.Logf("descriptors pushed to %d — a new pipe will land in the range the old fd_set math dropped",
		int(burn[len(burn)-1].Fd()))

	// Sanity-check the primitive at the descriptor numbers actually in play, so a
	// pass here cannot be a coincidence of the harness.
	probeFd := int(burn[len(burn)-1].Fd())
	var probeSet syscall.FdSet
	fdSet(&probeSet, probeFd)
	if !fdIsSet(&probeSet, probeFd) {
		t.Fatalf("fd_set cannot represent fd %d — teardown below would hang", probeFd)
	}

	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	oldState, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatalf("make test pty raw: %v", err)
	}
	defer func() {
		_ = term.Restore(int(slave.Fd()), oldState)
		_ = slave.Close()
		_ = master.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, readErr := master.Read(buf); readErr != nil {
				return
			}
		}
	}()

	mgr := NewManager()
	pcmd := NewPTYCommand(mgr, "test-high-cancel-fd", exec.Command("cat"))
	pcmd.inputReady = make(chan struct{})
	pcmd.SetStdin(slave)
	pcmd.SetStdout(slave)
	pcmd.SetStderr(slave)

	runDone := make(chan struct{})
	go func() {
		_ = pcmd.Run()
		close(runDone)
	}()

	select {
	case <-pcmd.inputReady:
	case <-time.After(5 * time.Second):
		t.Fatal("PTYCommand did not start reading input within 5s")
	}

	// Kill the child so Run() leaves via proc.Done() and enters the teardown that
	// must cancel the reader through the pipe.
	p, ok := mgr.Get("test-high-cancel-fd")
	if !ok {
		t.Fatal("process missing before kill")
	}
	if killErr := p.cmd.Process.Kill(); killErr != nil {
		t.Fatalf("kill child: %v", killErr)
	}

	select {
	case <-runDone:
	case <-time.After(20 * time.Second):
		t.Fatal("Run() did not return after the child exited: the cancel pipe never woke the " +
			"stdin reader, so teardown is blocked in wg.Wait() — the descriptor is above the " +
			"range the fd_set helpers can address")
	}
}
