package pty

import (
	"os/exec"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

// TestPTYCommandDetectsDetach runs PTYCommand under a fake stdin/stdout PTY
// pair and verifies that each detach key encoding (raw Ctrl-X, kitty CSI u,
// xterm modifyOtherKeys=2) causes the command to return with Detached=true.
// Claude Code enables modifyOtherKeys=2 on attach, so the encoded forms are
// what arrive in practice — the raw 0x18 byte rarely shows up on a real
// terminal once an inner TUI is running.
func TestPTYCommandDetectsDetach(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	cases := []struct {
		name  string
		bytes []byte
	}{
		{"raw_ctrl_x", []byte{0x18}},
		{"raw_ctrl_rbracket", []byte{0x1d}},
		{"kitty_ctrl_x", []byte("\x1b[120;5u")},
		{"kitty_ctrl_rbracket", []byte("\x1b[93;5u")},
		{"modifyOtherKeys_ctrl_x", []byte("\x1b[27;5;120~")},
		{"modifyOtherKeys_ctrl_rbracket", []byte("\x1b[27;5;93~")},
	}

	for _, tc := range cases {
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

			// 30s deadline — was 10s, but raw_ctrl_x flaked on Linux
			// under -race with multiple test binaries in flight. The
			// generous bound is for failure clarity, not real waits:
			// the success path returns in ~1ms once the byte arrives.
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
}
