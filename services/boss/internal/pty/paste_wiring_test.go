package pty

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeInput records everything the upload path writes toward the agent process.
// It stands in for *Process so the orchestration can be exercised without a
// real PTY child and select(2) scheduling — the Run() plumbing itself is proven
// separately in paste_wiring_pty_test.go.
type fakeInput struct {
	mu      sync.Mutex
	written []byte
}

func (f *fakeInput) WriteInput(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, data...)
	return nil
}

func (f *fakeInput) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written...)
}

// syncWriter is a concurrency-safe stand-in for the outer terminal. The upload
// goroutine and the read loop both write to it, so an unsynchronized
// bytes.Buffer would be a data race under -race.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// uploaderSpy counts calls and records the local path it was asked to upload.
type uploaderSpy struct {
	mu     sync.Mutex
	calls  int
	paths  []string
	upload func(ctx context.Context, localPath string) (string, error)
}

func (u *uploaderSpy) fn(ctx context.Context, localPath string) (string, error) {
	u.mu.Lock()
	u.calls++
	u.paths = append(u.paths, localPath)
	fn := u.upload
	u.mu.Unlock()
	if fn == nil {
		return "", errors.New("no upload configured")
	}
	return fn(ctx, localPath)
}

func (u *uploaderSpy) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *uploaderSpy) lastPath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.paths) == 0 {
		return ""
	}
	return u.paths[len(u.paths)-1]
}

// pasteHarness bundles a PTYCommand with a fake terminal and fake process, and
// the scanner Run would have built for it.
type pasteHarness struct {
	cmd     *PTYCommand
	term    *syncWriter
	proc    *fakeInput
	scanner *pasteScanner
	cancel  context.CancelFunc
}

func newPasteHarness(t *testing.T, upload PasteUpload) *pasteHarness {
	t.Helper()
	term := &syncWriter{}
	cmd := &PTYCommand{}
	cmd.SetStdout(term)
	cmd.SetPasteUploader(upload)
	proc := &fakeInput{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &pasteHarness{
		cmd:     cmd,
		term:    term,
		proc:    proc,
		scanner: newPasteScanner(cmd.pasteClaim(ctx, proc)),
		cancel:  cancel,
	}
}

// drain joins every launched upload goroutine, with a bound so a hung drain
// fails the test instead of hanging the package.
func (h *pasteHarness) drain(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.cmd.awaitPasteUploads()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("awaitPasteUploads did not complete within 10s")
	}
}

// TestPasteClaimIsNilWithoutUploader pins the structural half of "local mode is
// unchanged": with no uploader there is no claim closure at all, so
// newPasteScanner keeps no state and feed is the identity function. The
// end-to-end half is TestPTYCommandForwardsPasteUnchangedWithoutUploader.
func TestPasteClaimIsNilWithoutUploader(t *testing.T) {
	c := &PTYCommand{}
	if claim := c.pasteClaim(context.Background(), &fakeInput{}); claim != nil {
		t.Fatal("pasteClaim returned a non-nil claim with no uploader installed")
	}
}

// TestPasteClaimUploadsOnceAndSwallowsPaste proves the interception: an
// installed uploader is consulted exactly once, with the local path the paste
// named, and nothing from the original paste is forwarded to the agent.
func TestPasteClaimUploadsOnceAndSwallowsPaste(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	const remote = "/remote/uploads/9f2c/dropped.png"

	spy := &uploaderSpy{upload: func(context.Context, string) (string, error) {
		return remote, nil
	}}
	h := newPasteHarness(t, spy.fn)

	paste := BracketedPaste(img)
	if out := h.scanner.feed(paste); len(out) != 0 {
		t.Fatalf("feed forwarded %q, want the paste fully swallowed", out)
	}
	h.drain(t)

	if got := spy.callCount(); got != 1 {
		t.Fatalf("upload called %d times, want exactly 1", got)
	}
	if got := spy.lastPath(); got != img {
		t.Fatalf("upload local path = %q, want %q", got, img)
	}
	if got := h.proc.bytes(); bytes.Contains(got, []byte(img)) {
		t.Fatalf("forwarded bytes %q contain the original local path", got)
	}
}

// TestPasteUploadInjectsRemotePathWithNoNewline asserts the exact injection
// shape on success. The absence of a trailing newline is the acceptance
// criterion: a newline would auto-submit the turn. The separators on BOTH ends
// are load-bearing too — the injection is asynchronous, so it lands after
// whatever the user typed while waiting, and without a leading one the path
// arrives glued to their last word.
func TestPasteUploadInjectsRemotePathWithNoNewline(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	const remote = "/remote/uploads/9f2c/dropped.png"

	h := newPasteHarness(t, func(context.Context, string) (string, error) {
		return remote, nil
	})
	h.scanner.feed(BracketedPaste(img))
	h.drain(t)

	got := h.proc.bytes()
	want := BracketedPaste(" " + remote + " ")
	if !bytes.Equal(got, want) {
		t.Fatalf("injected %q, want %q", got, want)
	}
	body := bytes.TrimSuffix(bytes.TrimPrefix(got, []byte("\x1b[200~")), []byte("\x1b[201~"))
	if !bytes.HasPrefix(body, []byte(" ")) {
		t.Errorf("injected %q has no leading separator: an async injection lands after "+
			"whatever the user typed while waiting and would glue the path to their last word", got)
	}
	if !bytes.HasSuffix(body, []byte(" ")) {
		t.Errorf("injected %q has no trailing separator", got)
	}
	if !bytes.HasPrefix(got, []byte("\x1b[200~")) {
		t.Errorf("injected %q, want bracketed-paste introducer prefix", got)
	}
	if !bytes.HasSuffix(got, []byte("\x1b[201~")) {
		t.Errorf("injected %q, want bracketed-paste terminator suffix", got)
	}
	if !bytes.Contains(got, []byte(remote)) {
		t.Errorf("injected %q, want it to contain the remote path %q", got, remote)
	}
	if bytes.Contains(got, []byte(img)) {
		t.Errorf("injected %q, want the LOCAL path %q absent", got, img)
	}
	// The whole point of BracketedPaste: no newline anywhere, so the turn is
	// not submitted.
	if bytes.ContainsAny(got, "\n\r") {
		t.Errorf("injected %q contains a newline, want none", got)
	}
}

// TestPasteUploadEscapesASpaceInTheRemotePath is the AC-1 guard for a remote
// $HOME that contains a space.
//
// remotehost keeps the basename IT chooses space-free, but the directory prefix
// is the remote's ${XDG_CACHE_HOME:-$HOME/.cache} — arbitrary user-set env over
// an arbitrary passwd field — and remotehost.validateRemoteDir deliberately
// permits a space there (TestEnsureUploadDirAcceptsARemoteHomeWithASpace pins
// it). Injected bare, such a path is a token that ends at the first space, so
// the agent reads "/Users/Some" and the paste fails silently: no error, no
// status text, nothing to distinguish it from a broken feature. Escaping it is
// exactly what the terminal does for a local drag, and what decodePastedPath on
// this same seam already decodes.
func TestPasteUploadEscapesASpaceInTheRemotePath(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	const remote = "/Users/Some User/.cache/boss/uploads/sess/9f2c/shot.png"

	h := newPasteHarness(t, func(context.Context, string) (string, error) {
		return remote, nil
	})
	h.scanner.feed(BracketedPaste(img))
	h.drain(t)

	got := h.proc.bytes()
	want := BracketedPaste(` /Users/Some\ User/.cache/boss/uploads/sess/9f2c/shot.png `)
	if !bytes.Equal(got, want) {
		t.Fatalf("injected %q, want %q", got, want)
	}
	// The falsifiable half: no space may survive unescaped inside the path, or
	// the agent's token stops there.
	body := bytes.TrimSuffix(bytes.TrimPrefix(got, []byte("\x1b[200~")), []byte("\x1b[201~"))
	inner := bytes.TrimSuffix(bytes.TrimPrefix(body, []byte(" ")), []byte(" "))
	for i, b := range inner {
		if b == ' ' && (i == 0 || inner[i-1] != '\\') {
			t.Fatalf("injected %q leaves an unescaped space at %d: the agent would read a "+
				"truncated path", got, i)
		}
	}
}

// TestPasteUploadFailureInjectsMessageAndNoPath asserts that a failed upload
// tells the user what happened and injects no path at all — neither the remote
// one that was never created nor the local one the agent cannot open.
func TestPasteUploadFailureInjectsMessageAndNoPath(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	const remote = "/remote/uploads/9f2c/dropped.png"

	h := newPasteHarness(t, func(context.Context, string) (string, error) {
		return remote, errors.New("scp: permission denied")
	})
	h.scanner.feed(BracketedPaste(img))
	h.drain(t)

	got := h.proc.bytes()
	if !bytes.Contains(got, []byte("[boss]")) {
		t.Errorf("injected %q, want the [boss] prefix", got)
	}
	if !bytes.Contains(got, []byte("permission denied")) {
		t.Errorf("injected %q, want it to carry the failure reason", got)
	}
	if bytes.Contains(got, []byte(remote)) {
		t.Errorf("injected %q, want the remote path %q absent on failure", got, remote)
	}
	if bytes.Contains(got, []byte(img)) {
		t.Errorf("injected %q, want the local path %q absent on failure", got, img)
	}
}

// TestSanitizePasteMessage covers the hygiene helper directly.
func TestSanitizePasteMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "connection refused", "connection refused"},
		// The ESC bytes go; the printable residue stays as inert text.
		{"escape byte stripped", "boom \x1b[2J\x1b[H gone", "boom [2J [H gone"},
		{"bare escape stripped", "boom \x1b gone", "boom gone"},
		{"newlines collapse", "line one\nline two\r\n\tline three", "line one line two line three"},
		{"runs collapse", "a   \x00\x01  b", "a b"},
		{"del stripped", "a\x7fb", "a b"},
		{"leading and trailing dropped", "\n  hello  \n", "hello"},
		{"empty", "", ""},
		{"only control", "\x1b\x00\n", ""},
		// C1 controls: a terminal in an 8-bit locale reads U+009B as CSI in its
		// own right, so a remote error can drive the terminal with no ESC in it.
		{"c1 csi stripped", "boom \u009b2J gone", "boom 2J gone"},
		{"c1 range stripped", "a\u0080\u008f\u009fb", "a b"},
		// ...but the filter is rune-wise, so multi-byte text whose UTF-8
		// encoding CONTAINS 0x80-0x9f bytes survives intact. A byte-wise C1
		// filter would eat the 0x80 out of "…" (E2 80 A6) and mangle this.
		{"multibyte survives", "done… déjà vu", "done… déjà vu"},
		{"invalid utf8 dropped", "a\xffb", "a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizePasteMessage(tc.in); got != tc.want {
				t.Errorf("sanitizePasteMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizePasteMessageTruncatesOnRuneBoundary makes sure the bound cannot
// split a multi-byte rune.
func TestSanitizePasteMessageTruncatesOnRuneBoundary(t *testing.T) {
	got := sanitizePasteMessage(strings.Repeat("é", 400))
	if len(got) > maxPasteMessageBytes+len(pasteMessageEllipsis) {
		t.Fatalf("sanitized length %d exceeds the bound", len(got))
	}
	if !strings.HasSuffix(got, pasteMessageEllipsis) {
		t.Fatalf("sanitized %q, want the truncation ellipsis", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("sanitized %q is not valid UTF-8", got)
	}
}

// TestPasteUploadFailureMessageIsControlFree is the security assertion: an
// error string that originated on a REMOTE machine must not be able to carry an
// escape sequence into the agent's input box, and must not be able to flood it.
func TestPasteUploadFailureMessageIsControlFree(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	hostile := "\x1b[2Jwiped\nthe screen " + strings.Repeat("A", 5<<10)

	h := newPasteHarness(t, func(context.Context, string) (string, error) {
		return "", errors.New(hostile)
	})
	h.scanner.feed(BracketedPaste(img))
	h.drain(t)

	got := h.proc.bytes()
	// Strip exactly the framing this payload is allowed to contain.
	body := bytes.TrimPrefix(got, []byte("\x1b[200~"))
	body = bytes.TrimSuffix(body, []byte("\x1b[201~"))
	for i, b := range body {
		if b < 0x20 || b == 0x7f {
			t.Fatalf("injected body byte %d = %#x is a control character: %q", i, b, body)
		}
	}
	// [boss] prefix + reason bound + ellipsis + trailing space, well under the
	// width of any input box.
	if len(got) > 320 {
		t.Fatalf("injected payload is %d bytes, want a bounded message", len(got))
	}
	if !bytes.Contains(got, []byte("[2Jwiped the screen")) {
		t.Errorf("injected %q, want the sanitized reason text", got)
	}
}

// TestPasteStatusLine asserts the exact overlay bytes, including that an empty
// message clears the line.
func TestPasteStatusLine(t *testing.T) {
	got := pasteStatusLine("uploading")
	want := []byte("\x1b7\x1b[999;1H\x1b[2Kuploading\x1b8")
	if !bytes.Equal(got, want) {
		t.Errorf("pasteStatusLine = %q, want %q", got, want)
	}

	clear := pasteStatusLine("")
	wantClear := []byte("\x1b7\x1b[999;1H\x1b[2K\x1b8")
	if !bytes.Equal(clear, wantClear) {
		t.Errorf("pasteStatusLine(\"\") = %q, want %q", clear, wantClear)
	}

	// The message goes through the same hygiene helper as the injected text:
	// the escape byte is gone, so the residue cannot drive the terminal.
	hostile := pasteStatusLine("bad \x1b[2J thing")
	if !bytes.Equal(hostile, []byte("\x1b7\x1b[999;1H\x1b[2Kbad [2J thing\x1b8")) {
		t.Errorf("pasteStatusLine did not sanitize its message: %q", hostile)
	}
}

// TestPasteUploadShowsAndClearsStatusLine proves the in-flight indicator
// reaches the outer terminal BEFORE the upload returns (the whole reason it
// exists) and is cleared once it does.
func TestPasteUploadShowsAndClearsStatusLine(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	started := make(chan struct{})
	release := make(chan struct{})

	h := newPasteHarness(t, func(context.Context, string) (string, error) {
		close(started)
		<-release
		return "/remote/uploads/dropped.png", nil
	})

	h.scanner.feed(BracketedPaste(img))
	<-started

	inFlight := pasteStatusLine(pasteUploadingMessage(img))
	cleared := pasteStatusLine("")
	if got := h.term.bytes(); !bytes.Contains(got, inFlight) {
		t.Fatalf("terminal got %q, want the in-flight indicator %q", got, inFlight)
	}
	if got := h.term.bytes(); bytes.Contains(got, cleared) {
		t.Fatalf("terminal got %q, want the status line NOT yet cleared", got)
	}

	close(release)
	h.drain(t)

	if got := h.term.bytes(); !bytes.Contains(got, cleared) {
		t.Fatalf("terminal got %q, want the clear sequence %q after completion", got, cleared)
	}
}

// TestPasteUploadCancelledOnTeardown is the leak / post-teardown guard: an
// upload still in flight when the attach tears down sees a cancelled context,
// the drain completes without a timeout, and nothing further is written to the
// process or the terminal.
func TestPasteUploadCancelledOnTeardown(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)
	started := make(chan struct{})
	sawCancel := make(chan struct{})

	h := newPasteHarness(t, func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		close(sawCancel)
		return "/remote/uploads/dropped.png", nil
	})

	h.scanner.feed(BracketedPaste(img))
	<-started
	termBefore := h.term.bytes()

	// What Run's teardown defer does: cancel, then drain.
	h.cancel()
	select {
	case <-sawCancel:
	case <-time.After(10 * time.Second):
		t.Fatal("upload context was not cancelled on teardown")
	}
	h.drain(t)

	if got := h.proc.bytes(); len(got) != 0 {
		t.Fatalf("wrote %q to the process after teardown, want nothing", got)
	}
	if got := h.term.bytes(); !bytes.Equal(got, termBefore) {
		t.Fatalf("wrote %q to the terminal after teardown, want nothing beyond %q", got, termBefore)
	}
}

// TestPasteScannerForwardsNonImagePasteWithUploaderInstalled keeps the
// fail-open contract honest in host mode: a paste that is not an image path is
// forwarded byte for byte and never reaches the uploader.
func TestPasteScannerForwardsNonImagePasteWithUploaderInstalled(t *testing.T) {
	spy := &uploaderSpy{}
	h := newPasteHarness(t, spy.fn)

	paste := BracketedPaste("just some prose about /tmp/shot.png being stale")
	got := h.scanner.feed(paste)
	if !bytes.Equal(got, paste) {
		t.Fatalf("feed returned %q, want the paste forwarded unchanged %q", got, paste)
	}
	h.drain(t)
	if n := spy.callCount(); n != 0 {
		t.Fatalf("uploader consulted %d times for a non-image paste, want 0", n)
	}
}
