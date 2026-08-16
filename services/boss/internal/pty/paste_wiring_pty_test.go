//go:build !race

package pty

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
// deterministically (and under -race) by paste_wiring_test.go; the tests here
// exist only to prove the Run() plumbing — that the scanner really sits in the
// stdin path, in the right order — once per paste wiring a command can carry:
// none at all, the uploader (--host), and the image-paste notice (every other
// attach, since BOS-849).

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
//
// configure runs against the PTYCommand after the stdio wiring and BEFORE Run,
// which is the only window in which paste wiring may be installed. It is
// variadic so the uploader call sites read exactly as they did before the
// image-paste notice existed; pass withPTYImagePasteNotice to get the wiring a
// real non---host attach ships.
func startPTYPaste(t *testing.T, id string, upload PasteUpload, configure ...func(*PTYCommand)) *ptyPasteHarness {
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
	for _, apply := range configure {
		apply(pcmd)
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

// withPTYImagePasteNotice installs the non-claiming observation hook, which is
// what internal/views wires on every attach that is NOT --host. Use it for any
// Run()-level test that means to exercise the local configuration users
// actually get.
func withPTYImagePasteNotice(c *PTYCommand) { c.SetImagePasteNotice() }

// TestPTYCommandForwardsPasteUnchangedWithoutUploader pins the UNWIRED command:
// neither an uploader nor the image-paste notice, so pasteClaim is a literal
// nil, the scanner keeps no state and feed is the identity function.
//
// That is a real configuration to protect — it is what a PTYCommand does before
// anything installs paste wiring — but since BOS-849 it is NOT the shape a
// local attach ships: internal/views installs the notice on the else branch, so
// the local wiring runs the scanner and may paint an overlay. The Run()-level
// test for that shape is
// TestPTYCommandForwardsPasteUnchangedWithTheImagePasteNotice below; the two are
// only a complete picture together.
func TestPTYCommandForwardsPasteUnchangedWithoutUploader(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)

	h := startPTYPaste(t, "test-paste-local", nil)

	// Assert the absences against the running command rather than against a
	// counter on a closure that was never installed — a counter nothing can
	// increment reads like coverage while being unfalsifiable. These go red the
	// moment a refactor installs either hook unconditionally.
	if h.pcmd.HasPasteUploader() {
		t.Fatal("an unwired command installed a paste uploader")
	}
	if h.pcmd.HasImagePasteNotice() {
		t.Fatal("an unwired command installed the image-paste notice; this test pins the nil-claim path")
	}

	paste := BracketedPaste(img)
	h.send(paste)

	want := append([]byte("READY"), paste...)
	h.waitForChild(want)

	if got := h.childOutput(); !bytes.Equal(got, want) {
		t.Fatalf("agent received %q, want byte-identical passthrough %q", got, want)
	}
	// No status overlay may appear on the outer terminal with no paste wiring
	// installed at all — deliberately NOT the same claim as "in local mode",
	// which since BOS-849 installs the notice and may paint one. Wait
	// until the echoed paste has actually reached the terminal capture first,
	// so this negative is asserted against a capture that has caught up rather
	// than one that is merely empty.
	h.waitForTerminal(paste)
	if got := h.terminalOutput(); bytes.Contains(got, []byte(ptyMoveLastRow)) {
		t.Fatalf("terminal got a status overlay with no paste wiring at all: %q", got)
	}
}

// TestPTYCommandForwardsPasteUnchangedWithTheImagePasteNotice is the Run()-level
// test for the configuration a real non---host attach ships (BOS-849).
//
// The unit tests prove the pieces — imagePasteNoticeClaim always returns false,
// the scanner conserves the stream at every split offset, internal/views
// installs the notice and no uploader — but nothing above this line proved the
// COMPOSITION: the claim closure firing from inside Run's stdin read loop, with
// writeStatus painting the outer terminal while proc.Attach writes the agent's
// own output to the same file. That is the arrangement users run, so it gets an
// end-to-end assertion.
//
// Byte equality comes first and outranks the overlay: the notice may explain a
// paste it cannot help with, and it may never alter one.
func TestPTYCommandForwardsPasteUnchangedWithTheImagePasteNotice(t *testing.T) {
	// Absolute, well-formed, image-extensioned, and guaranteed absent — the
	// same shape as the reported /tmp/cmux-drop-<uuid>.png, which is exactly
	// the body the hook reacts to.
	missing := filepath.Join(t.TempDir(), "cmux-drop-383cd973.png")

	h := startPTYPaste(t, "test-paste-notice", nil, withPTYImagePasteNotice)

	if !h.pcmd.HasImagePasteNotice() {
		t.Fatal("the harness did not install the image-paste notice")
	}
	// The notice must never bring an uploader with it: with none installed
	// there is nothing a claimed paste could be handed to.
	if h.pcmd.HasPasteUploader() {
		t.Fatal("the notice branch installed a paste uploader")
	}

	paste := BracketedPaste(missing)
	h.send(paste)

	want := append([]byte("READY"), paste...)
	h.waitForChild(want)

	if got := h.childOutput(); !bytes.Equal(got, want) {
		t.Fatalf("agent received %q, want byte-identical passthrough %q", got, want)
	}

	// And the explanation did reach the outer terminal. Asserted on the message
	// text rather than only on ptyMoveLastRow so a stray overlay from some
	// other source could not satisfy it.
	h.waitForTerminal([]byte("boss --host"))
	term := h.terminalOutput()
	if !bytes.Contains(term, []byte("no such image on this machine")) {
		t.Fatalf("terminal never explained the unreachable paste: %q", term)
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

// TestPTYCommandAttachesASerializedTerminalWriter pins the Run() half of the
// mutual-exclusion fix. TestTerminalWritesAreMutuallyExclusive proves the lock
// works when both writers go through terminalWriter; nothing there proves Run
// actually HANDS that writer to the Process. It did not, before BOS-849: Attach
// received c.stdout itself, so the agent-output pump wrote around the lock and
// every overlay could splice into a sequence it was emitting.
//
// Asserted on the writer the Process is holding rather than on rendered bytes,
// because a splice is a scheduling accident: a test that watched the terminal
// would pass on almost every run of the broken tree. Revert Attach to c.stdout
// and this fails immediately and always.
func TestPTYCommandAttachesASerializedTerminalWriter(t *testing.T) {
	h := startPTYPaste(t, "test-serialized-terminal-writer", nil)

	h.proc.mu.Lock()
	w := h.proc.writer
	h.proc.mu.Unlock()

	if _, ok := w.(lockedWriter); !ok {
		t.Fatalf("Process.Attach holds a %T, want lockedWriter: agent output must share the "+
			"terminal lock with the overlay writes, or the two can splice mid-sequence", w)
	}
}
