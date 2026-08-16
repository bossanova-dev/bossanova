package pty

import (
	"context"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
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

// SetPasteUploader installs the upload capability. It is called on --host
// attaches and only there; a nil pasteUpload is what "no transfer" means, and
// nothing below ever swallows a paste without one.
//
// It used to be true that a non-uploader attach had NO paste machinery at all —
// nil claim, no scanner state, feed as the identity function — and that this
// made local mode byte-identical by construction. SetImagePasteNotice narrows
// that (BOS-849): a non---host attach now installs a claim closure so a paste
// boss cannot help with can say so. The identity-function path is still real and
// still what you get when NEITHER an uploader nor the notice is installed
// (pasteClaim returns a literal nil), but conservation on the notice path rests
// instead on that closure ALWAYS returning false — see imagePasteNoticeClaim,
// which is why its return value is a constant and not a decision.
func (c *PTYCommand) SetPasteUploader(upload PasteUpload) {
	c.pasteUpload = upload
}

// SetImagePasteNotice installs the non-claiming observation hook.
//
// It exists because the alternative was silence. Under a plain ssh login the
// boss TUI runs on the REMOTE box, so no uploader is installed, and a screenshot
// pasted from the user's laptop arrives as an absolute path to a file that only
// exists on the other side of the connection. Before this hook nothing inspected
// that paste at all: the agent received a path it could not open, boss said
// nothing, and the outcome was indistinguishable from a broken feature — which
// is precisely how BOS-849 was reported.
//
// The hook cannot fix it. There is no reverse channel from a remote boss back to
// the client's filesystem, so the honest response is to forward the bytes
// unchanged and explain, pointing at `boss --host`, which puts the TUI on the
// machine the file is actually on. Everything this installs is therefore
// observation only: no upload, no goroutine, no enterHold, no WriteInput, and
// above all no claim.
//
// An uploader WINS. pasteClaim tests pasteUpload first, so a command carrying
// both gets the upload claim and this hook is never consulted — while
// HasImagePasteNotice would still answer true. That is deliberate (an uploader
// can actually move the file, so its message is the better one) but it means
// this flag records what was asked for, not what runs; internal/views installs
// exactly one of the two and asserts both halves, which is what keeps the
// distinction from mattering.
//
// Install it on every attach that is NOT --host, including a true-local one.
// The predicate is "an absolute image path that does not resolve on THIS
// machine", which is false for every ordinary local paste, so a local attach
// that pastes a real file sees nothing at all — while a local attach that pastes
// a genuinely mistyped path gets the same honest message, worded so it is true
// in both cases.
func (c *PTYCommand) SetImagePasteNotice() {
	c.imagePasteNotice = true
}

// HasImagePasteNotice reports whether the observation hook was installed.
//
// It is the twin of HasPasteUploader and exists for the same reason: the
// installing package (internal/views) has to be able to assert the wiring from
// outside this one. HasPasteUploader makes an ABSENCE observable; this makes the
// PRESENCE observable on the branch where the absence is asserted, so the
// non---host branch of that gate cannot silently become a no-op.
func (c *PTYCommand) HasImagePasteNotice() bool {
	return c.imagePasteNotice
}

// HasPasteUploader reports whether an uploader was installed.
//
// It exists for one assertion, made from the package that does the installing
// (internal/views): that a LOCAL attach never calls SetPasteUploader at all. A
// test that only checked "nothing crashed" would pass against an unconditional
// install — so the absence has to be observable from outside this package.
//
// Since BOS-849 that un-installed nil is no longer by itself what makes local
// mode byte-identical: such an attach installs the observation hook instead, and
// conservation there rests on imagePasteNoticeClaim always returning false. What
// the absence still guarantees is the half only an uploader can break — with no
// uploader there is nothing to hand a claimed paste to, so no paste can be
// swallowed. Read it together with HasImagePasteNotice, which is why
// internal/views asserts both on the same branch.
func (c *PTYCommand) HasPasteUploader() bool {
	return c.pasteUpload != nil
}

// pasteInputWriter is the only thing the upload completion path needs from a
// *Process. Narrowing it here keeps the orchestration testable without a real
// PTY child and select(2) scheduling.
type pasteInputWriter interface {
	WriteInput(data []byte) error
}

// pasteClaim builds the pasteScanner claim closure for this attach: the upload
// claim when an uploader is installed, the non-claiming notice when only the
// observation hook is, and a literal nil when neither is.
//
// That literal nil (rather than a closure that answers false) is still
// deliberate and still the cheapest guarantee available: with it the scanner
// keeps no state and feed is the identity function, so a command with no paste
// wiring at all cannot perturb the stdin stream even in principle. See
// SetPasteUploader.
func (c *PTYCommand) pasteClaim(ctx context.Context, proc pasteInputWriter) func(body []byte) bool {
	if c.pasteUpload == nil {
		if c.imagePasteNotice {
			return c.imagePasteNoticeClaim()
		}
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

// pasteLogger returns the logger this command's paste path records through:
// the injected one if a test installed it, otherwise the process-global,
// file-only logger above.
//
// Injection is a field rather than a package-level swap because a test that
// reassigned zerolog's global log.Logger would race every other test in the
// binary under -race — and the paste path is exercised from a read-loop
// goroutine, so that race is not theoretical.
func (c *PTYCommand) pasteLogger() zerolog.Logger {
	if c.pasteLog != nil {
		return c.pasteLog()
	}
	return pasteUploadLogger()
}

// imagePasteNoticeClaim is the observation hook's claim closure. It ALWAYS
// returns false.
//
// That constant is the whole safety argument. scanPaste re-emits the
// introducer, the body, and the terminator verbatim for any body the claim
// declines, so a closure that cannot answer true cannot remove a byte from the
// stream — stream conservation on this path is a property of the return
// statement, not of the branches above it. Do not make this return value
// conditional; if a future change needs to claim, it needs an uploader, not
// this hook.
//
// The classification is taken from ONE classifyImagePaste call, which is one
// os.Stat, on the stdin read-loop goroutine. Asking twice — once for the claim
// decision and once for the reason — would double the syscall cost of every
// pasted path on the keystroke path, which is exactly the trade that function's
// comment refuses.
//
// Only the two POST-STAT rejections say anything at all. A claimable local
// image (ok) is the feature working; prose, a non-image path, and a directory
// are the overwhelming majority of pastes and are none of boss's business.
//
// The two that survive are then treated differently on purpose:
//
//   - pasteRejectMissing — the file is not here. Unambiguous, unhelpable, and
//     the reported symptom, so it earns both the record and the overlay.
//   - pasteRejectUnreadable — some other stat failure, so the file may be right
//     here and merely unreadable. Recorded, but NOT shown: "no such image on
//     this machine · use boss --host" would be a false diagnosis followed by
//     advice that would not help, and a wrong overlay over a working pane is
//     worse than the silence this hook exists to remove.
func (c *PTYCommand) imagePasteNoticeClaim() func(body []byte) bool {
	return func(body []byte) bool {
		path, reason, ok := classifyImagePaste(body)
		if ok || (reason != pasteRejectMissing && reason != pasteRejectUnreadable) {
			return false
		}

		// The durable half. zerolog, not slog: boss installs
		// bossalog.SetupFileOnly (services/boss/cmd/main.go), so the global
		// zerolog logger writes ONLY to the rotated boss.log and a record from
		// here cannot corrupt the display a full-screen agent owns. Nothing in
		// boss calls slog.SetDefault, so slog would instead write to
		// os.Stderr — straight into the attached pane — and its default Info
		// level would drop a Debug on the floor before it got there. This is
		// the same reasoning pasteUploadLogger above documents, and the same
		// logger internal/views uses from the live detach path.
		//
		// Bound to a local first because zerolog's level methods take a
		// pointer receiver and a call result is not addressable.
		logger := c.pasteLogger()
		logger.Debug().
			Str("reason", string(reason)).
			Str("path", path).
			Msg("paste: image path not claimed")

		// The visible half — and it is DELIBERATELY the weaker half here, more
		// so than on the upload path. There the paste is claimed, so nothing
		// reaches the agent and the "uploading…" overlay stands until
		// finishPasteUpload clears it. Here the body is forwarded a moment
		// later by the very feed call this closure runs inside, so the agent
		// receives the paste, re-renders its composer, and repaints the last
		// row this is painted on — the redraw that erases the notice is
		// CAUSALLY TRIGGERED by the same paste, milliseconds away. Treat the
		// flash as a nudge for a user already looking at the bottom row, never
		// as the record; the Debug line above is the durable evidence, and
		// docs/help/troubleshooting.md tells the user to go and read it.
		//
		// Not "fixed" with a delayed repaint: the only way to outlast that
		// redraw is to paint again on a timer from another goroutine. That
		// write would be serialized against the agent's output like every other
		// one here — terminalWriter makes splicing impossible — but ordering is
		// not the objection. It would land intact at an arbitrary later moment,
		// on top of whatever frame the agent owns by then, with nothing left to
		// erase it. A brief flash plus a durable log beats a stray overlay
		// stranded over unrelated content.
		//
		// Same overlay mechanism the upload path uses, so there is exactly one
		// of these in the package, and pasteStatusLine sanitizes the message.
		//
		// Guarded, per the reason split above: only a file that is genuinely
		// not here gets a message claiming it is not here.
		if reason == pasteRejectMissing {
			c.writeStatus(pasteStatusLine(imagePasteNoticeMessage(path)))
		}
		return false
	}
}

// imagePasteNoticeMessage is the one-line overlay for an image path that does
// not resolve on this machine.
//
// The wording has to be true in two situations, because this hook is installed
// on EVERY non---host attach. On the remote side of a plain ssh login the file
// really is on another machine; on a true-local attach the user simply pasted a
// path that does not exist. "No such image on this machine" is accurate in both,
// and naming `boss --host` is useful in both — it is the supported route for the
// remote case and inert advice in the local one, where the real answer (the file
// is not there) is already the first half of the sentence.
//
// Only the base name is shown, for the same reason pasteUploadingMessage shows
// only the base name: a dropped path is routinely wider than the terminal, and
// the row this is painted on is one line.
//
// And the base name is ELIDED to fit, which sanitizePasteMessage's cap cannot do
// for us: that cap is 200 BYTES, and the row this lands on is measured in
// COLUMNS. The reported /tmp/cmux-drop-<uuid>.png shape rendered 139 columns
// with the original wording — nowhere near 200 bytes, so nothing trimmed it, and
// on an 80-column terminal the write at the last row wraps with DECAWM set and
// SCROLLS THE AGENT'S WHOLE FRAME up a line, after which the DECRC restores a
// saved position that now names different content. Painting a transient notice
// must never move the pane it is explaining.
//
// The advice is kept SHORT and the name is what gives way, so the half that
// tells the user what to do survives every length: a tail-truncated 200-byte cap
// deletes the fixed, actionable clause and keeps the variable one, which is
// exactly backwards. The full explanation lives in the docs and the boss.log
// record; this row only has to name the file and point somewhere.
func imagePasteNoticeMessage(localPath string) string {
	const prefix = "[boss] no such image on this machine: "
	const suffix = " · use boss --host"
	// TWO budgets, because two different caps can cut this line and they do not
	// imply each other. The row is measured in cells, so that is what bounds
	// wrapping — but sanitizePasteMessage below still tail-truncates at
	// maxPasteMessageBytes, and a narrow name can be byte-dense: twenty ZWJ
	// family emoji are 79 cells and 258 BYTES, so a cell-only budget lets the
	// sanitizer fire and take the advice clause with it. Bounding both is what
	// makes "the name gives way, never the advice" true rather than nearly true.
	cells := maxPasteMessageColumns - ansi.StringWidth(prefix) - ansi.StringWidth(suffix)
	bytesLeft := maxPasteMessageBytes - len(prefix) - len(suffix)
	return prefix + elidePasteName(filepath.Base(localPath), cells, bytesLeft) + suffix
}

// maxPasteMessageColumns bounds a status-line overlay in display columns.
//
// 80 is the conservative floor rather than the real terminal width: this runs on
// the stdin read-loop goroutine, which holds no size, and a notice that fits the
// narrowest terminal anyone still uses cannot wrap on a wider one. Asking the
// PTY for its size here would be more precise and would also put a syscall and a
// resize race on the keystroke path to save a few columns.
//
// Width is measured in DISPLAY CELLS, via the same ansi helpers internal/views
// already uses — never in runes, and never in bytes. A rune count is only right
// for the ASCII paths a terminal drag usually produces: a full-width CJK name
// occupies two cells per rune, so a 24-rune budget spent on one renders 48 cells
// and wraps exactly as the byte cap did. Counting runes here would also make the
// test that pins this bound circular, since it would measure with the same wrong
// unit the code did.
const maxPasteMessageColumns = 80

// elidePasteName shortens name to at most cells display cells AND at most
// byteBudget bytes, marking either cut with an ellipsis. A non-positive budget
// yields "" — the caller's fixed text already fills the row, and the name is the
// part that may be dropped.
//
// ansi.Truncate does the cell-aware cut: it never splits a wide rune down the
// middle, it returns the string untouched when it already fits, and it subtracts
// the ellipsis's own width itself. The byte pass then runs on the result,
// because a string can be narrow and still enormous — combining marks and ZWJ
// sequences cost bytes without costing cells — and it is the only thing standing
// between such a name and sanitizePasteMessage's tail cut.
func elidePasteName(name string, cells, byteBudget int) string {
	if cells <= 0 || byteBudget <= 0 {
		return ""
	}
	name = ansi.Truncate(name, cells, pasteMessageEllipsis)
	if len(name) <= byteBudget {
		return name
	}
	// Back off to a rune boundary so the cut cannot emit half a multi-byte
	// character, exactly as sanitizePasteMessage does. Cutting bytes can only
	// narrow the result, so the cell bound above still holds afterwards.
	cut := byteBudget - len(pasteMessageEllipsis)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + pasteMessageEllipsis
}

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
//
// It goes through terminalWriter, not stdout, and the whole overlay is ONE
// Write for that reason. This runs on the stdin read loop (a paste claim) and
// on an upload goroutine (the in-flight indicator and its clear), while the
// Process read loop is pumping agent output to the same terminal; unserialized,
// the frame this emits could land inside a sequence the agent was midway
// through, and the terminal would act on the splice. pasteStatusLine already
// renders save-cursor / move / erase / text / restore-cursor into a single
// buffer, so holding the lock once per overlay is enough to keep the five
// pieces together — do not decompose this into several writes.
func (c *PTYCommand) writeStatus(line []byte) {
	if c.stdout == nil {
		return
	}
	_, _ = c.terminalWriter().Write(line)
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
