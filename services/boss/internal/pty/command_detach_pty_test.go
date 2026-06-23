//go:build !race

package pty

import (
	"os/exec"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

// TestPTYCommandDetectsDetach runs PTYCommand under a fake stdin/stdout PTY
// pair and verifies that a terminal-encoded detach key causes the command to
// return with Detached=true. The parser variants are covered exhaustively by
// TestContainsDetachSequence; this test only needs to prove the Run() plumbing.
// Claude Code enables modifyOtherKeys=2 on attach, so the encoded forms are
// what arrive in practice — the raw 0x18 byte rarely shows up on a real
// terminal once an inner TUI is running.
//
// Build tag: !race. This is a real-OS-PTY + select(2) integration smoke test
// whose success path depends on the kernel waking the stdin-read goroutine and
// the OS scheduler running it promptly. Under -race on loaded CI runners (the
// race detector's scheduling overhead plus dozens of parallel test binaries on
// a 2-core box), that goroutine can be starved for tens of seconds — long
// enough that even a 30s deadline times out, taking staging releases red. Eight
// prior "stabilize" attempts (PRs #133, #225, #254, #447, #592, …) could not
// make the wall-clock assertions survive that starvation. The detach-detection
// logic itself is fully covered deterministically by TestContainsDetachSequence
// (which runs under -race), and the Manager's concurrency is race-tested by
// TestManagerConcurrentGetGetOrStartCleanup, so excluding this timing-sensitive
// plumbing check from -race loses no race-detection coverage while ending the
// flake. It still runs in normal (non-race) builds and local `go test`.
func TestPTYCommandDetectsDetach(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	tc := struct {
		name  string
		bytes []byte
	}{
		name:  "modifyOtherKeys_ctrl_x",
		bytes: []byte("\x1b[27;5;120~"),
	}

	t.Run(tc.name, func(t *testing.T) {
		master, slave, err := creackpty.Open()
		if err != nil {
			t.Fatalf("open pty: %v", err)
		}
		// Put the fake terminal in raw mode before Run starts. If a
		// loaded race job writes Ctrl-X before Run reaches MakeRaw,
		// canonical mode can hold the byte indefinitely.
		oldState, err := term.MakeRaw(int(slave.Fd()))
		if err != nil {
			t.Fatalf("make test pty raw: %v", err)
		}

		mgr := NewManager()
		cmd := exec.Command("cat")
		pcmd := NewPTYCommand(mgr, "test-detach-"+tc.name, cmd)
		pcmd.inputReady = make(chan struct{})
		pcmd.SetStdin(slave)
		pcmd.SetStdout(slave)
		pcmd.SetStderr(slave)

		// runDone closes when Run() returns. A close-once channel
		// (rather than a one-shot send) lets both the success path
		// and the cleanup defer await termination without the
		// cleanup blocking on an empty buffered channel after the
		// success path has already drained it.
		runDone := make(chan struct{})
		var runErr error
		go func() {
			runErr = pcmd.Run()
			close(runDone)
		}()

		// Single cleanup defer so we control ordering: kill cat →
		// wait for Run() to fully unwind (which joins goroutine
		// 20's read of slave.Fd) → close PTY. Closing the PTY
		// before that join would race the still-live reader on
		// loaded -race CI runners.
		defer func() {
			if p, ok := mgr.Get("test-detach-" + tc.name); ok {
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
		}()

		select {
		case <-pcmd.inputReady:
		case <-time.After(5 * time.Second):
			t.Fatal("PTYCommand did not start reading input within 5s")
		}
		if _, err := master.Write(tc.bytes); err != nil {
			t.Fatalf("write detach bytes: %v", err)
		}

		// 30s deadline — generous bound for failure clarity, not real
		// waits: the success path returns in ~1ms once the byte arrives.
		select {
		case <-runDone:
			if runErr != nil {
				t.Fatalf("Run returned error: %v", runErr)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("PTYCommand did not return within 30s of %q", tc.bytes)
		}

		if !pcmd.Detached {
			t.Fatalf("expected Detached=true after %q", tc.bytes)
		}
		if pcmd.ProcessExited {
			t.Fatal("expected ProcessExited=false (process should still be running after detach)")
		}
	})
}
