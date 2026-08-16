package pty

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/rs/zerolog"
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

// noticeHarness bundles a PTYCommand carrying the NON-CLAIMING observation hook
// — the wiring every non---host attach gets — with the fake outer terminal it
// paints overlays on and a buffer capturing the debug records it writes.
//
// The logger is injected through the command's own field rather than by
// swapping zerolog's package-global log.Logger. A global swap would race every
// other test in this binary under -race, and the paste path is driven from a
// read-loop goroutine in production, so that race is not hypothetical.
type noticeHarness struct {
	cmd     *PTYCommand
	term    *syncWriter
	logs    *syncWriter
	proc    *fakeInput
	scanner *pasteScanner
}

func newNoticeHarness(t *testing.T) *noticeHarness {
	t.Helper()
	term := &syncWriter{}
	logs := &syncWriter{}
	cmd := &PTYCommand{}
	cmd.SetStdout(term)
	cmd.SetImagePasteNotice()
	cmd.pasteLog = func() zerolog.Logger { return zerolog.New(logs) }
	proc := &fakeInput{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &noticeHarness{
		cmd:     cmd,
		term:    term,
		logs:    logs,
		proc:    proc,
		scanner: newPasteScanner(cmd.pasteClaim(ctx, proc)),
	}
}

// records returns the debug records written so far, one per line.
func (h *noticeHarness) records() []string {
	out := strings.TrimSpace(string(h.logs.bytes()))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// noticeClaim builds just the observation hook's claim closure, for the scanner
// tests in paste_test.go that only care that it never removes a byte.
func noticeClaim(t *testing.T) func(body []byte) bool {
	t.Helper()
	return newNoticeHarness(t).cmd.pasteClaim(context.Background(), &fakeInput{})
}

// TestPasteClaimIsNilWithoutUploaderOrNotice pins the structural half of "an
// unwired command cannot perturb the stdin stream": with neither an uploader nor
// the observation hook there is no claim closure AT ALL, so newPasteScanner
// keeps no state and feed is the identity function. The end-to-end half is
// TestPTYCommandForwardsPasteUnchangedWithoutUploader.
//
// The literal nil is the assertion. A closure that merely answered false would
// conserve the same bytes but would run the whole scanner for every keystroke,
// and this test is what stops that from happening by accident.
func TestPasteClaimIsNilWithoutUploaderOrNotice(t *testing.T) {
	c := &PTYCommand{}
	if c.HasPasteUploader() {
		t.Fatal("a bare PTYCommand reports an uploader")
	}
	if c.HasImagePasteNotice() {
		t.Fatal("a bare PTYCommand reports the image-paste notice; it must be opt-in per attach")
	}
	if claim := c.pasteClaim(context.Background(), &fakeInput{}); claim != nil {
		t.Fatal("pasteClaim returned a non-nil claim with neither an uploader nor the notice installed")
	}

	// ...and the fixture is not vacuous: installing the notice DOES produce a
	// closure, so the nil above is the absence of wiring rather than a broken
	// accessor.
	c.SetImagePasteNotice()
	if !c.HasImagePasteNotice() {
		t.Fatal("SetImagePasteNotice did not install the notice")
	}
	if claim := c.pasteClaim(context.Background(), &fakeInput{}); claim == nil {
		t.Fatal("pasteClaim returned nil with the notice installed")
	}
}

// TestImagePasteNoticeExplainsAMissingImageWithoutClaimingIt is the BOS-849
// behaviour in one test: the exact symptom that was reported — an absolute image
// path that exists on the user's laptop and not on the machine running the TUI —
// still reaches the agent byte for byte, and now leaves both halves of the
// evidence behind.
//
// Byte equality comes first and outranks the rest. The overlay is transient (the
// full-screen agent redraws over the last row on its next frame), which is why
// the debug record is asserted as the durable half.
func TestImagePasteNoticeExplainsAMissingImageWithoutClaimingIt(t *testing.T) {
	// t.TempDir() is removed by the harness at test end but exists now, so the
	// path below is absolute, well-formed, and guaranteed absent — the same
	// shape as the reported /tmp/cmux-drop-<uuid>.png.
	missing := filepath.Join(t.TempDir(), "cmux-drop-383cd973.png")
	h := newNoticeHarness(t)

	paste := BracketedPaste(missing)
	got := h.scanner.feed(paste)
	if !bytes.Equal(got, paste) {
		t.Fatalf("feed returned %q, want the paste forwarded verbatim %q", got, paste)
	}

	records := h.records()
	if len(records) != 1 {
		t.Fatalf("wrote %d debug records, want exactly 1: %q", len(records), records)
	}
	if !strings.Contains(records[0], string(pasteRejectMissing)) {
		t.Errorf("record %q does not name the rejection reason %q", records[0], pasteRejectMissing)
	}
	if !strings.Contains(records[0], missing) {
		t.Errorf("record %q does not name the path %q", records[0], missing)
	}

	// Exactly one overlay, and it is the notice: byte equality against the
	// rendered line proves there is no second, hand-rolled overlay mechanism.
	wantOverlay := pasteStatusLine(imagePasteNoticeMessage(missing))
	if term := h.term.bytes(); !bytes.Equal(term, wantOverlay) {
		t.Fatalf("terminal got %q, want exactly the notice overlay %q", term, wantOverlay)
	}
	if !bytes.Contains(h.term.bytes(), []byte("boss --host")) {
		t.Error("the overlay does not name `boss --host`, which is the only route that works here")
	}

	// Observation only: nothing is injected into the agent's input box, no
	// Enter is withheld, and no upload goroutine is launched.
	if got := h.proc.bytes(); len(got) != 0 {
		t.Fatalf("wrote %q toward the agent, want nothing: the notice never injects", got)
	}
	// Asserted through the filter rather than the hold's counter, because
	// withholding the Enter is the behaviour that would hurt: a hold that is
	// never released leaves the user unable to submit their turn at all.
	enter := []byte{pasteSubmitCR}
	if got := h.cmd.enterHold.filter(enter); !bytes.Equal(got, enter) {
		t.Fatalf("the enter hold swallowed %q; the notice must never call enterHold.begin", enter)
	}
	h.cmd.awaitPasteUploads() // returns immediately unless a goroutine was launched
}

// TestImagePasteNoticeIsSilentWhenItHasNothingToExplain covers every body class
// that is NOT the reported symptom. A false positive here is worse than the bug:
// it paints an overlay over a working pane for an ordinary paste.
//
// The existing-local-image case is the load-bearing one. On a true-local attach
// that is the everyday paste, the notice is installed there too, and it must be
// completely invisible.
func TestImagePasteNoticeIsSilentWhenItHasNothingToExplain(t *testing.T) {
	existing := writeTempImage(t, "shot.png", 32)
	notAnImage := writeTempImage(t, "notes.txt", 16)

	cases := []struct {
		name string
		body string
	}{
		{"existing_local_image", existing},
		{"non_image_path", notAnImage},
		{"prose", "please look at the screenshot I sent"},
		{"prose_mentioning_a_png", "/tmp/shot.png is stale, ignore it"},
		{"relative_image_path", "shot.png"},
		{"missing_non_image_path", filepath.Join(t.TempDir(), "absent.txt")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newNoticeHarness(t)
			paste := BracketedPaste(tc.body)
			if got := h.scanner.feed(paste); !bytes.Equal(got, paste) {
				t.Fatalf("feed returned %q, want unchanged %q", got, paste)
			}
			if recs := h.records(); len(recs) != 0 {
				t.Errorf("wrote %d debug records for %q, want none: %q", len(recs), tc.body, recs)
			}
			if term := h.term.bytes(); len(term) != 0 {
				t.Errorf("painted %q on the terminal for %q, want nothing", term, tc.body)
			}
		})
	}
}

// TestImagePasteNoticeRecordsAnUnreadablePathWithoutSayingItIsMissing pins the
// split between the two post-stat rejections.
//
// os.Stat fails for more reasons than "not here". An image inside a directory
// the user cannot traverse is sitting on THIS machine, so telling them there is
// "no such image on this machine · use boss --host" would be a false diagnosis
// followed by advice that cannot help — a wrong overlay painted over a working
// pane, which is worse than the silence this hook exists to remove. The record
// still happens, because the paste is exactly as undiagnosable without it.
func TestImagePasteNoticeRecordsAnUnreadablePathWithoutSayingItIsMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses a 0000 directory, so os.Stat would succeed and the fixture would be vacuous")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	unreadable := filepath.Join(locked, "shot.png")
	if err := os.WriteFile(unreadable, make([]byte, 8), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Registered before the chmod so it runs even if that fails, and ahead of
	// t.TempDir's own cleanup (which is LIFO) so the removal can descend.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if _, err := os.Stat(unreadable); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Skipf("filesystem does not enforce directory traversal here (stat err = %v); fixture is vacuous", err)
	}

	h := newNoticeHarness(t)
	paste := BracketedPaste(unreadable)
	if got := h.scanner.feed(paste); !bytes.Equal(got, paste) {
		t.Fatalf("feed returned %q, want the paste forwarded verbatim %q", got, paste)
	}

	records := h.records()
	if len(records) != 1 {
		t.Fatalf("wrote %d debug records, want exactly 1: %q", len(records), records)
	}
	if !strings.Contains(records[0], string(pasteRejectUnreadable)) {
		t.Errorf("record %q does not name %q; an unreadable file must not be filed as missing",
			records[0], pasteRejectUnreadable)
	}
	if strings.Contains(records[0], string(pasteRejectMissing)) {
		t.Errorf("record %q claims the file is missing; it is here and merely unreadable", records[0])
	}
	if term := h.term.bytes(); len(term) != 0 {
		t.Errorf("painted %q on the terminal; `no such image on this machine` would be false here", term)
	}
}

// Escape-only spellings of two characters that are invisible in source. A bare
// U+200D is also rejected outright by staticcheck's ST1018, and a bare U+0301
// reorders onto whatever precedes it in an editor, so neither belongs in a
// literal.
const (
	zwjJoiner      = "\u200d" // ZERO WIDTH JOINER - bytes, no display width
	combiningAcute = "\u0301" // COMBINING ACUTE ACCENT - bytes, no display width
)

// TestImagePasteNoticeMessageFitsOneRowAndKeepsTheAdvice pins the overlay's
// COLUMN bound, which is a different property from sanitizePasteMessage's
// 200-BYTE cap and is not implied by it.
//
// The row this is painted on is the terminal's last one. A message wider than
// the terminal wraps there with DECAWM set, which scrolls the agent's whole
// frame up a line and leaves the DECRC restoring a position that now names
// different content — a transient notice moving the pane it is explaining. The
// reported /tmp/cmux-drop-<uuid>.png shape rendered 139 columns before this
// bound existed, far under 200 bytes, so nothing trimmed it.
//
// The second half is the ordering: it is the NAME that gives way, never the
// `boss --host` advice. A tail-truncated byte cap does the opposite — it deletes
// the fixed, actionable clause and keeps the variable one.
func TestImagePasteNoticeMessageFitsOneRowAndKeepsTheAdvice(t *testing.T) {
	cases := []struct {
		name string
		base string
	}{
		{"reported_symptom", "cmux-drop-383cd973-4bab-42c5-a2f5-20497d580fa2.png"},
		{"short", "a.png"},
		{"pathological", strings.Repeat("x", 4096) + ".png"},
		{"multibyte", strings.Repeat("日本語", 300) + ".png"},
		// Narrow but byte-DENSE: these cost bytes without costing cells, so a
		// cell-only bound leaves sanitizePasteMessage's 200-byte tail cut free
		// to fire and take the advice clause with it.
		// Written as escapes rather than literals: a bare U+200D is invisible in
		// source and staticcheck's ST1018 rejects it outright.
		{"zwj_sequences", strings.Repeat("\U0001F468"+zwjJoiner+"\U0001F469"+zwjJoiner+"\U0001F467", 20) + ".png"},
		{"combining_marks", "e" + strings.Repeat(combiningAcute, 300) + ".png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := imagePasteNoticeMessage(filepath.Join("/tmp", tc.base))
			// Assert against the string the overlay ACTUALLY writes, not the
			// composed one: pasteStatusLine renders sanitizePasteMessage(msg),
			// so checking msg alone would miss a cut that only the sanitizer
			// makes.
			rendered := sanitizePasteMessage(msg)

			// Measured in DISPLAY CELLS, deliberately not in runes. Measuring
			// with the same unit the code counts in would make this test
			// circular: a rune-counted bound passes a full-width CJK name that
			// renders at twice the width and wraps anyway.
			for label, s := range map[string]string{"composed": msg, "rendered": rendered} {
				if got := ansi.StringWidth(s); got > maxPasteMessageColumns {
					t.Errorf("%s message is %d display cells, want <= %d: %q",
						label, got, maxPasteMessageColumns, s)
				}
			}
			// The actionable half must survive at EVERY length — it is the only
			// thing on the row that tells the user what to do — and it must
			// survive in the rendered string, which is where a byte-based cut
			// would otherwise reappear for a narrow-but-dense name.
			if !strings.Contains(msg, "boss --host") {
				t.Errorf("composed message %q dropped the boss --host advice", msg)
			}
			if !strings.Contains(rendered, "boss --host") {
				t.Errorf("rendered message %q dropped the boss --host advice", rendered)
			}
		})
	}

	// A name short enough to fit is shown whole — the elision must not fire
	// on the ordinary case and leave every notice looking truncated.
	if msg := imagePasteNoticeMessage("/tmp/a.png"); !strings.Contains(msg, "a.png") {
		t.Errorf("message %q elided a name that fits", msg)
	}
}

// TestImagePasteNoticeNeverClaimsAnything is the guarantee the local mode now
// rests on, asserted directly against the closure rather than through the
// scanner: whatever it is handed, the answer is false.
//
// scanPaste re-emits introducer, body and terminator verbatim for every declined
// body, so "always false" IS stream conservation on this path. It replaced "there
// is no claim closure at all" as the local guarantee (BOS-849), which is why it
// gets its own test instead of being an implied property of the tests above.
func TestImagePasteNoticeNeverClaimsAnything(t *testing.T) {
	existing := writeTempImage(t, "shot.png", 32)
	claim := noticeClaim(t)

	bodies := []string{
		existing,
		filepath.Join(t.TempDir(), "absent.png"),
		"'" + existing + "'",
		"file://" + existing,
		"~/shot.png",
		"just some prose",
		"",
		"   ",
		strings.Repeat("x", 4096),
	}
	for _, body := range bodies {
		if claim([]byte(body)) {
			t.Fatalf("the observation hook claimed %q; it must never swallow a paste", body)
		}
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

// blockingWriter holds every Write open until release is closed, announcing on
// entered that one has begun. It exists to make "can a second write start while
// the first is in flight?" a question with a deterministic answer, rather than
// one a sleep-and-hope race detector might or might not catch on a given run.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.entered <- struct{}{}
	<-b.release
	return len(p), nil
}

// TestTerminalWritesAreMutuallyExclusive pins the fix for the splice this
// package shipped with since the upload path landed: the Process read loop
// writes agent output to the outer terminal from its own goroutine, while an
// overlay is written from the stdin read loop (a paste claim) and from an
// upload goroutine. Sharing the terminal with no lock, an overlay could land in
// the middle of a CSI the agent was emitting, and the terminal would act on the
// splice rather than on either sequence.
//
// The assertion is the NEGATIVE one — that the second write cannot begin — and
// it is what makes this test non-vacuous. Route either writer around
// terminalWriter and the second write starts immediately, so this fails on the
// unserialized tree for the reason it names.
func TestTerminalWritesAreMutuallyExclusive(t *testing.T) {
	bw := &blockingWriter{
		// Buffered so a Write that genuinely begins is never held up by the
		// test's own receive scheduling: an unserialized second write must be
		// able to announce itself, or the negative assertion below would pass
		// for the wrong reason.
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	c := &PTYCommand{}
	c.SetStdout(bw)

	// The agent-output write, in the shape Process.Attach performs it.
	go func() { _, _ = c.terminalWriter().Write([]byte("\x1b[1;1Hagent frame")) }()
	select {
	case <-bw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent-output write never started")
	}

	// The overlay write, on the path a paste takes, while the first is still
	// mid-flight and holding the terminal.
	go func() { c.writeStatus(pasteStatusLine("[boss] no such image on this machine: shot.png")) }()

	select {
	case <-bw.entered:
		t.Fatal("an overlay write began while agent output was mid-write: the two can splice mid-sequence")
	case <-time.After(100 * time.Millisecond):
	}

	// Serialized, not deadlocked: the overlay must still land once the agent
	// write completes. A lock that never released would pass the assertion
	// above and strand the notice, which is the opposite failure.
	close(bw.release)
	select {
	case <-bw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the overlay write never ran after the agent write released the terminal")
	}
}
