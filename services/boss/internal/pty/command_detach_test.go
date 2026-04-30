package pty

import (
	"os"
	"os/exec"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
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
			defer master.Close() //nolint:errcheck // best-effort cleanup in test
			defer slave.Close()  //nolint:errcheck // best-effort cleanup in test

			origStdin, origStdout := os.Stdin, os.Stdout
			os.Stdin = slave
			os.Stdout = slave
			defer func() {
				os.Stdin = origStdin
				os.Stdout = origStdout
			}()

			mgr := NewManager()
			cmd := exec.Command("cat")
			pcmd := NewPTYCommand(mgr, "test-detach-"+tc.name, cmd)
			pcmd.SetStdin(slave)
			pcmd.SetStdout(slave)
			pcmd.SetStderr(slave)

			done := make(chan error, 1)
			go func() {
				done <- pcmd.Run()
			}()

			time.Sleep(200 * time.Millisecond)
			if _, err := master.Write(tc.bytes); err != nil {
				t.Fatalf("write detach bytes: %v", err)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run returned error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("PTYCommand did not return within 3s of %q", tc.bytes)
			}

			if !pcmd.Detached {
				t.Fatalf("expected Detached=true after %q", tc.bytes)
			}
			if pcmd.ProcessExited {
				t.Fatal("expected ProcessExited=false (process should still be running after detach)")
			}

			if p, ok := mgr.Get("test-detach-" + tc.name); ok {
				_ = p.cmd.Process.Kill()
				<-p.Done()
			}
		})
	}
}
