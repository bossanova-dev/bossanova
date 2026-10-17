//go:build !race

package pty

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

// TestPTYCommandKeepsTerminalRepliesFromAttachedChild drives a real attach: a
// PTY pair stands in for the user's terminal, and the attached child records
// every byte it reads. Bubble Tea asks the terminal for DECRPM mode reports
// (CSI ? 2026 $ p, CSI ? 2027 $ p) and the kitty keyboard flags (CSI ? u); a
// reply that lands on stdin while a chat is attached must never reach the
// child, because tmux turns it into keystrokes and the agent's composer then
// shows "2026;2$y". The user's own keystrokes around the replies, including a
// kitty-encoded key that shares the CSI ... u shape, must still arrive intact.
//
// Shares the sibling PTY tests' constraints: real creackpty pair and
// //go:build !race, because the real-OS-PTY + select(2) scheduling starves
// under the race runner.
func TestPTYCommandKeepsTerminalRepliesFromAttachedChild(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty not available")
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
	var screen bytes.Buffer
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				mu.Lock()
				screen.Write(buf[:n])
				mu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()

	recorded := filepath.Join(t.TempDir(), "child-stdin")
	const id = "test-reply-leak"
	mgr := NewManager()
	child := exec.Command("/bin/sh", "-c", `stty raw -echo; printf READY; exec cat > "$1"`, "sh", recorded)
	pcmd := NewPTYCommand(mgr, id, child)
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
	defer func() {
		if p, ok := mgr.Get(id); ok {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Logf("Run did not return within 15s of child kill; closing PTY anyway")
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
	waitFor(t, 10*time.Second, "child to switch its tty to raw mode", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Contains(screen.Bytes(), []byte("READY"))
	})

	const typed = "a\x1b[?2026;2$y\x1b[?2027;0$yb\x1b[?0uc\x1b[97;5ud\x1b[?62;22;52cZ"
	const want = "abc\x1b[97;5udZ"
	if _, err := master.Write([]byte(typed)); err != nil {
		t.Fatalf("write terminal input: %v", err)
	}

	var got []byte
	waitFor(t, 10*time.Second, "child to receive the end marker", func() bool {
		got, _ = os.ReadFile(recorded)
		return bytes.HasSuffix(got, []byte("Z"))
	})
	if string(got) != want {
		t.Fatalf("attached child read %q, want %q (terminal replies must not reach it)", got, want)
	}

	if _, err := master.Write([]byte{detachByte}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("PTYCommand did not return within 30s of Ctrl+]")
	}
	if runErr != nil || !pcmd.Detached {
		t.Fatalf("Ctrl+] after the replies: Run err=%v Detached=%v, want nil/true", runErr, pcmd.Detached)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
