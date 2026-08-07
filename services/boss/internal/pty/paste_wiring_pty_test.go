//go:build !race

package pty

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

// Build tag: !race, for the same reason as command_detach_pty_test.go — these
// are real-OS-PTY + select(2) integration tests whose timing starves under the
// race runner on loaded CI boxes. The interception logic, the injected byte
// shapes, the hygiene helper, and the teardown cancel/drain are all covered
// deterministically (and under -race) by paste_wiring_test.go; these two tests
// exist only to prove the Run() plumbing — that the scanner really sits in the
// stdin path, in the right order.

// ptyPasteHarness drives PTYCommand.Run against a real PTY pair, with a child
// that echoes its stdin back byte for byte so the test can assert exactly what
// reached the agent process.
type ptyPasteHarness struct {
	t       *testing.T
	master  *os.File
	proc    *Process
	pcmd    *PTYCommand
	runDone chan struct{}
	termMu  *sync.Mutex
	termBuf *bytes.Buffer
}

// startPTYPaste boots the harness and returns it once the child is echoing.
//
// The child is `stty raw -echo; printf READY; exec cat`: raw+noecho takes the
// line discipline out of the picture (no canonical buffering, no ^[ echo
// mangling), so `cat` reflects the exact bytes PTYCommand wrote to the PTY, and
// READY marks the point after which that is true.
func startPTYPaste(t *testing.T, id string, upload PasteUpload) *ptyPasteHarness {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	// Raw before Run starts, matching the sibling PTY tests: canonical mode on
	// a loaded runner can hold our bytes indefinitely.
	oldState, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatalf("make test pty raw: %v", err)
	}

	// Drain the master continuously so slave-side writes never block on a full
	// PTY buffer, and so the status-line overlay is captured as it arrives.
	var termMu sync.Mutex
	var termBuf bytes.Buffer
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				termMu.Lock()
				termBuf.Write(buf[:n])
				termMu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()

	mgr := NewManager()
	cmd := exec.Command("sh", "-c", "stty raw -echo; printf READY; exec cat")
	pcmd := NewPTYCommand(mgr, id, cmd)
	pcmd.inputReady = make(chan struct{})
	pcmd.SetStdin(slave)
	pcmd.SetStdout(slave)
	pcmd.SetStderr(slave)
	if upload != nil {
		pcmd.SetPasteUploader(upload)
	}

	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = pcmd.Run()
		close(runDone)
	}()

	t.Cleanup(func() {
		if p, ok := mgr.Get(id); ok {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-runDone:
			// A test that ends without detaching gets here via the kill above,
			// so a "signal: killed" error is the expected shape; only the
			// detach test asserts on how Run returned.
			_ = runErr
		case <-time.After(15 * time.Second):
			t.Logf("Run did not return within 15s — closing PTY anyway")
		}
		_ = term.Restore(int(slave.Fd()), oldState)
		_ = slave.Close()
		_ = master.Close()
		<-readerDone
	})

	select {
	case <-pcmd.inputReady:
	case <-time.After(5 * time.Second):
		t.Fatal("PTYCommand did not start reading input within 5s")
	}

	proc, ok := mgr.Get(id)
	if !ok {
		t.Fatal("manager has no process for the attach")
	}

	h := &ptyPasteHarness{
		t:       t,
		master:  master,
		proc:    proc,
		pcmd:    pcmd,
		runDone: runDone,
		termMu:  &termMu,
		termBuf: &termBuf,
	}
	h.waitForChild([]byte("READY"))
	return h
}

// waitForChild polls the child's echoed output until it contains want.
func (h *ptyPasteHarness) waitForChild(want []byte) {
	h.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if bytes.Contains(h.proc.RecentOutput(8192), want) {
			return
		}
		select {
		case <-deadline:
			h.t.Fatalf("child never echoed %q; got %q", want, h.proc.RecentOutput(8192))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForTerminal polls the outer-terminal capture until it contains want. The
// capture arrives through a second PTY hop and a drain goroutine, so it can lag
// the child echo by a scheduling quantum or more on a loaded runner.
func (h *ptyPasteHarness) waitForTerminal(want []byte) {
	h.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if bytes.Contains(h.terminalOutput(), want) {
			return
		}
		select {
		case <-deadline:
			h.t.Fatalf("terminal never showed %q; got %q", want, h.terminalOutput())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (h *ptyPasteHarness) childOutput() []byte { return h.proc.RecentOutput(8192) }

func (h *ptyPasteHarness) terminalOutput() []byte {
	h.termMu.Lock()
	defer h.termMu.Unlock()
	return append([]byte(nil), h.termBuf.Bytes()...)
}

func (h *ptyPasteHarness) send(b []byte) {
	h.t.Helper()
	if _, err := h.master.Write(b); err != nil {
		h.t.Fatalf("write to pty master: %v", err)
	}
}

// TestPTYCommandForwardsPasteUnchangedWithoutUploader is the load-bearing
// local-mode test. With no uploader installed, a bracketed paste of a path that
// WOULD match imagePastePath must reach the agent byte for byte, no status
// overlay may appear, and no uploader may be installed at all.
func TestPTYCommandForwardsPasteUnchangedWithoutUploader(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)

	h := startPTYPaste(t, "test-paste-local", nil)

	// The absence of an uploader is the whole local guarantee, so assert it
	// against the running command rather than against a counter on a closure
	// that was never installed — a counter nothing can increment reads like
	// coverage while being unfalsifiable. This one goes red the moment a
	// refactor installs an uploader unconditionally.
	if h.pcmd.HasPasteUploader() {
		t.Fatal("a local attach installed a paste uploader; local mode must install none at all")
	}

	paste := BracketedPaste(img)
	h.send(paste)

	want := append([]byte("READY"), paste...)
	h.waitForChild(want)

	if got := h.childOutput(); !bytes.Equal(got, want) {
		t.Fatalf("agent received %q, want byte-identical passthrough %q", got, want)
	}
	// No status overlay may appear on the outer terminal in local mode. Wait
	// until the echoed paste has actually reached the terminal capture first,
	// so this negative is asserted against a capture that has caught up rather
	// than one that is merely empty.
	h.waitForTerminal(paste)
	if got := h.terminalOutput(); bytes.Contains(got, []byte(ptyMoveLastRow)) {
		t.Fatalf("terminal got a status overlay in local mode: %q", got)
	}
}

// TestPTYCommandUploadsPastedImage proves the Run() wiring in host mode: the
// paste is swallowed on the stdin path, the uploader runs once, and only the
// remote path reaches the agent.
func TestPTYCommandUploadsPastedImage(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	const remote = "/remote/uploads/9f2c/dropped.png"

	var calls int32
	h := startPTYPaste(t, "test-paste-host", func(context.Context, string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return remote, nil
	})

	h.send(BracketedPaste(img))

	want := append([]byte("READY"), BracketedPaste(" "+remote+" ")...)
	h.waitForChild(want)

	got := h.childOutput()
	if !bytes.Equal(got, want) {
		t.Fatalf("agent received %q, want %q", got, want)
	}
	if bytes.Contains(got, []byte(img)) {
		t.Fatalf("agent received the local path in %q", got)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("uploader called %d times, want exactly 1", n)
	}
	h.waitForTerminal(pasteStatusLine(pasteUploadingMessage(img)))
	h.waitForTerminal(pasteStatusLine(""))
}

// TestPTYCommandDetachStillFiresWithUploaderInstalled keeps the detach contract
// green in host mode: a detach key that arrives outside a paste is forwarded by
// the scanner and still detaches.
func TestPTYCommandDetachStillFiresWithUploaderInstalled(t *testing.T) {
	h := startPTYPaste(t, "test-paste-detach", func(context.Context, string) (string, error) {
		return "/remote/never.png", nil
	})

	// Ordinary typing passes straight through the scanner.
	h.send([]byte("hello"))
	h.waitForChild([]byte("READYhello"))

	// modifyOtherKeys=2 Ctrl+X — the form that arrives in practice.
	h.send([]byte("\x1b[27;5;120~"))

	select {
	case <-h.runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("PTYCommand did not detach within 30s with an uploader installed")
	}
	if !h.pcmd.Detached {
		t.Fatal("expected Detached=true after the detach key")
	}
	if h.pcmd.ProcessExited {
		t.Fatal("expected ProcessExited=false after a detach")
	}
	if got := h.childOutput(); bytes.Contains(got, []byte("\x1b[27;5;120~")) {
		t.Fatalf("detach sequence leaked to the agent: %q", got)
	}
}

// TestPTYCommandDetachFiresFromInsideAnUnterminatedPaste is the sibling that
// matters more: the scanner forwards nothing while it buffers a paste body, so
// a paste whose terminator never arrives could hold the detach key — the user's
// only guaranteed exit from an attached pane — until 8 KiB had accumulated.
// Proven through Run() rather than the scanner alone, because "the key escapes
// feed" is only useful if it reaches the detach scan on the same chunk.
func TestPTYCommandDetachFiresFromInsideAnUnterminatedPaste(t *testing.T) {
	h := startPTYPaste(t, "test-paste-detach-mid", func(context.Context, string) (string, error) {
		return "/remote/never.png", nil
	})

	// A paste that has begun and, as far as the scanner can tell, never ends.
	h.send([]byte("\x1b[200~/Users/someone/half-a-pa"))
	// Give the read loop a chance to buffer it before the key arrives, so the
	// key is genuinely delivered mid-paste rather than in the same chunk.
	time.Sleep(200 * time.Millisecond)
	h.send([]byte("\x18")) // Ctrl+X

	select {
	case <-h.runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Ctrl+X inside an unterminated paste never detached: " +
			"the paste buffer swallowed the user's only way out")
	}
	if !h.pcmd.Detached {
		t.Fatal("expected Detached=true after a Ctrl+X delivered inside a paste")
	}
}
