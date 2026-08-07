package pty

import (
	"context"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/safego"
)

// PasteUpload copies a local file to the machine the agent runs on and returns
// the path the agent can open it at.
//
// The capability is injected rather than imported: the actual transport (an ssh
// copy) lives in internal/remotehost, and this package must stay free of it.
//
// Implementations MUST honour ctx. Run cancels it during teardown, and that
// cancellation is the only thing that stops an in-flight copy when the user
// detaches — it is what lets the teardown drain wait without a timeout.
type PasteUpload func(ctx context.Context, localPath string) (remotePath string, err error)

// SetPasteUploader installs the upload capability. Nil (the default, and the
// only value in local mode) means the paste path is not inspected at all: Run
// builds its pasteScanner with a nil claim, newPasteScanner keeps no state, and
// pasteScanner.feed is the identity function. Local (non --host) mode is
// therefore byte-for-byte unchanged BY CONSTRUCTION — there is no branch that
// has to agree, because there is no machinery in the stdin path to disagree.
func (c *PTYCommand) SetPasteUploader(upload PasteUpload) {
	c.pasteUpload = upload
}

// HasPasteUploader reports whether an uploader was installed.
//
// It exists for one assertion, made from the package that does the installing
// (internal/views): that a LOCAL attach never calls SetPasteUploader at all. The
// un-installed nil is what makes local mode byte-identical, and a test that only
// checked "nothing crashed" would pass against an unconditional install — so the
// absence has to be observable from outside this package.
func (c *PTYCommand) HasPasteUploader() bool {
	return c.pasteUpload != nil
}

// pasteInputWriter is the only thing the upload completion path needs from a
// *Process. Narrowing it here keeps the orchestration testable without a real
// PTY child and select(2) scheduling.
type pasteInputWriter interface {
	WriteInput(data []byte) error
}

// pasteClaim builds the pasteScanner claim closure for this attach, or returns
// nil when no uploader is installed. Returning a literal nil (rather than a
// closure that answers false) is deliberate: see SetPasteUploader.
func (c *PTYCommand) pasteClaim(ctx context.Context, proc pasteInputWriter) func(body []byte) bool {
	if c.pasteUpload == nil {
		return nil
	}
	upload := c.pasteUpload
	return func(body []byte) bool {
		localPath, ok := imagePastePath(body)
		if !ok {
			return false
		}

		// Written synchronously, on the read-loop goroutine, BEFORE the upload
		// is launched. The paste has just been swallowed; without immediate
		// feedback a slow upload is indistinguishable from boss having eaten
		// the user's keystrokes.
		c.writeStatus(pasteStatusLine(pasteUploadingMessage(localPath)))

		// Also synchronous and also before the launch: from here until
		// finishPasteUpload releases it, an Enter is withheld rather than
		// forwarded, so the turn cannot be sent without the path. Registering it
		// after the goroutine started would leave the very window it closes.
		c.enterHold.begin()

		// Asynchronous so the keystroke path never stalls behind an ssh copy.
		c.trackPasteUpload(safego.Go(pasteUploadLogger(), func() {
			c.finishPasteUpload(ctx, proc, upload, localPath)
		}))
		return true
	}
}

// pasteUploadLogger is the logger safego reports a recovered upload panic
// through. It is the process-global one, NOT a Nop: boss installs
// bossalog.SetupFileOnly (services/boss/cmd/main.go), so the global logger
// writes to a file rather than the terminal, and a write from here therefore
// cannot corrupt the display a full-screen agent owns — internal/views logs
// through the same logger from the live detach path for the same reason. A Nop
// here would make an upload panic leave no trace anywhere, which is the one
// failure this file has no other way to explain.
func pasteUploadLogger() zerolog.Logger { return log.Logger }

// finishPasteUpload performs the upload and injects its result into the agent's
// input box.
func (c *PTYCommand) finishPasteUpload(
	ctx context.Context,
	proc pasteInputWriter,
	upload PasteUpload,
	localPath string,
) {
	// Registered BEFORE the upload runs, so a panic anywhere below — recovered
	// by safego, and therefore invisible here — still clears the in-flight
	// indicator. Otherwise "[boss] uploading foo.png…" stays painted with no
	// path and no error: the silent no-op that is indistinguishable from boss
	// having eaten the user's paste, which is the outcome the whole
	// fail-visibly design exists to prevent.
	//
	// The ctx guard is the same one the body applies: once teardown has begun
	// the terminal is being restored and nothing may be written to it.
	defer func() {
		if ctx.Err() != nil {
			return
		}
		c.writeStatus(pasteStatusLine(""))
	}()

	// Paired with the enterHold.begin in pasteClaim, and deferred for the same
	// reason the status clear above is: a panic below is recovered by safego and
	// is invisible here, but it must not leave the user's Enter withheld
	// forever. Registered AFTER the status defer so LIFO runs it FIRST — the
	// replayed key should reach the agent before the in-flight indicator is
	// cleared, so the composer is never briefly idle-looking with a submit still
	// in flight.
	//
	// A replay is skipped once teardown has begun: the composer is going away,
	// so there is nothing left for the key to submit. discard is not needed here
	// because release has already consumed the held flag.
	defer func() {
		if replay := c.enterHold.release(); replay && ctx.Err() == nil {
			_ = proc.WriteInput([]byte{pasteSubmitCR})
		}
	}()

	remotePath, err := upload(ctx, localPath)

	// Teardown has begun: the terminal is being restored to cooked mode and the
	// process output is being detached, so nothing may be written to either
	// from here.
	if ctx.Err() != nil {
		return
	}

	if err != nil {
		// No path is injected on failure — an unusable path in the input box is
		// worse than none, because the agent would try to read it.
		_ = proc.WriteInput(BracketedPaste(pasteSeparator + pasteFailureMessage(err)))
		return
	}

	// Separators on BOTH ends, and never a newline. A newline would submit the
	// turn before the prompt that gives the image meaning has been written. The
	// trailing space lets the user keep typing after the path — and the LEADING
	// one matters for the same reason in reverse: this injection is
	// asynchronous, so it lands wherever the composer cursor is when the upload
	// finishes, which is at the end of whatever the user typed while waiting.
	// Without it, "what is wrong in" + the path arrives glued into one word.
	_ = proc.WriteInput(BracketedPaste(pasteSeparator + injectedRemotePath(remotePath) + pasteSeparator))
}

// pasteSeparator keeps injected text a token of its own in the composer.
const pasteSeparator = " "

// injectedRemotePath renders the remote path for the agent's composer.
//
// A space is backslash-escaped, which is exactly the form a terminal produces
// when the user drags a spaced filename in locally — the form decodePastedPath
// on this very seam already decodes — so the agent meets the token it meets
// every day instead of a bare path that stops at the first space.
//
// The escaping is needed because a space CAN reach here. remotehost keeps the
// basename it chooses space-free, but the directory prefix is the remote's
// `${XDG_CACHE_HOME:-$HOME/.cache}`: arbitrary user-set env over an arbitrary
// passwd field, which remotehost.validateRemoteDir deliberately permits a space
// in and has a named test admitting one. So this is a case that occurs, not one
// that cannot — and when it does, an unescaped path fails AC 1 silently, with no
// error and nothing to distinguish it from a broken feature.
//
// The escape is unambiguous because a backslash can never reach here: both
// remotehost sanitisers and validateRemoteDir reject one outright.
func injectedRemotePath(remotePath string) string {
	return strings.ReplaceAll(remotePath, " ", `\ `)
}

// trackPasteUpload records a launched upload goroutine so teardown can join it.
// safego.Go's done channel is consumed rather than discarded — no
// fire-and-forget.
func (c *PTYCommand) trackPasteUpload(done <-chan struct{}) {
	c.uploadMu.Lock()
	defer c.uploadMu.Unlock()
	c.uploadDone = append(c.uploadDone, done)
}

// awaitPasteUploads blocks until every launched upload goroutine has exited.
//
// There is deliberately no timeout. Run cancels the upload context immediately
// before calling this, and PasteUpload implementations run their transport
// under that context, so every in-flight upload is already unwinding by the
// time we start waiting.
func (c *PTYCommand) awaitPasteUploads() {
	c.uploadMu.Lock()
	pending := c.uploadDone
	c.uploadDone = nil
	c.uploadMu.Unlock()

	for _, done := range pending {
		<-done
	}
}

// writeStatus puts a rendered status line on the outer terminal. Best-effort:
// write errors are ignored, matching the other terminal writes in this package.
func (c *PTYCommand) writeStatus(line []byte) {
	if c.stdout == nil {
		return
	}
	_, _ = c.stdout.Write(line)
}

// Overlay framing: save cursor (DECSC), jump to the last row, erase it, write,
// restore cursor (DECRC).
const (
	ptySaveCursor    = "\x1b7"
	ptyMoveLastRow   = "\x1b[999;1H"
	ptyEraseLine     = "\x1b[2K"
	ptyRestoreCursor = "\x1b8"
)

// pasteStatusLine renders a transient one-line overlay on the outer terminal.
// An empty message clears the line.
//
// Transient by design: the wrapped full-screen agent redraws over the last row
// on its next frame, so the overlay is a flash, not a widget. It is the only
// feedback channel available here — tea.Exec has handed the terminal to this
// command, so bubbletea is not rendering and cannot show progress.
//
// The message is sanitized with the same helper the injected failure text uses,
// so a remote error string can never drive the user's terminal.
func pasteStatusLine(msg string) []byte {
	clean := sanitizePasteMessage(msg)
	out := make([]byte, 0,
		len(ptySaveCursor)+len(ptyMoveLastRow)+len(ptyEraseLine)+len(clean)+len(ptyRestoreCursor))
	out = append(out, ptySaveCursor...)
	out = append(out, ptyMoveLastRow...)
	out = append(out, ptyEraseLine...)
	out = append(out, clean...)
	out = append(out, ptyRestoreCursor...)
	return out
}

// pasteUploadingMessage is the in-flight indicator text. Only the base name is
// shown: the full path is often wider than the terminal, and the user just
// dragged the file in, so they know where it came from.
func pasteUploadingMessage(localPath string) string {
	return "[boss] uploading " + filepath.Base(localPath) + "…"
}

// pasteFailureMessage is the text injected into the agent's input box when an
// upload fails. The "[boss]" prefix makes it unmistakably boss's own words
// rather than something the user typed or the agent produced.
func pasteFailureMessage(err error) string {
	return "[boss] image upload failed: " + sanitizePasteMessage(err.Error()) + " "
}

// maxPasteMessageBytes bounds a sanitized message. An ssh failure can be
// kilobytes of banner and stderr; the input box is a single line the user is
// about to type into.
const maxPasteMessageBytes = 200

// pasteMessageEllipsis marks a truncated message.
const pasteMessageEllipsis = "…"

// sanitizePasteMessage makes an arbitrary string — in practice an error that
// originated on a REMOTE machine — safe to write to the terminal and to inject
// into the agent's input box.
//
// Every control character is dropped — C0 (< 0x20) and DEL, and also C1
// (U+0080–U+009F), which a terminal in an 8-bit locale reads as controls in
// their own right: U+009B *is* CSI there, so a remote error carrying one could
// drive the terminal with no ESC anywhere in the string. Runs of dropped
// characters plus ordinary whitespace collapse to a single space. This is the
// security-relevant part: an unescaped \x1b (or a raw \x9b) reaching the outer
// terminal or the agent's input handler would be read as an escape sequence,
// letting a remote error string move the cursor, clear the screen, or forge
// input. Bounded length keeps a multi-kilobyte error from flooding the input box.
//
// The scan is rune-wise, not byte-wise, for two reasons. Dropping the raw bytes
// 0x80–0x9F would corrupt ordinary text, because those values are also UTF-8
// continuation bytes — "…" is E2 80 A6, and a byte filter would eat its 0x80.
// And ranging a Go string decodes as it goes, so invalid UTF-8 surfaces as
// RuneError and is dropped with everything else, which means the result is
// always valid UTF-8 and the rune-boundary truncation below is well defined.
//
// The printable remainder of a stripped sequence survives as literal text — a
// "\x1b[2J" becomes "[2J". That is deliberate: without the introducer those
// bytes are inert, and dropping the control character is the whole of the
// guarantee. Trying to also excise well-formed parameter runs would mean parsing
// attacker input to decide what to delete, which is more surface, not less.
func sanitizePasteMessage(msg string) string {
	var b strings.Builder
	b.Grow(len(msg))
	pendingSpace := false
	for _, r := range msg {
		if isDroppedMessageRune(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteRune(r)
	}

	out := b.String()
	if len(out) <= maxPasteMessageBytes {
		return out
	}
	// Back off to a rune boundary so truncation cannot emit half a multi-byte
	// character (which some terminals render as a replacement glyph and others
	// as garbage).
	cut := maxPasteMessageBytes
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + pasteMessageEllipsis
}

// isDroppedMessageRune reports whether r must not reach the terminal or the
// agent's input box: C0 controls, DEL, C1 controls, and the replacement rune
// that ranging a string yields for invalid UTF-8. A plain space is dropped too,
// but only so runs collapse — the caller re-emits one.
func isDroppedMessageRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == utf8.RuneError:
		return true
	case r == ' ':
		return true
	default:
		return false
	}
}
