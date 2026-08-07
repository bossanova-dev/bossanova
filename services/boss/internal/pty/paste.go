package pty

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// pasteIntroducer and pasteTerminator bracket a paste (DEC mode 2004). The
// terminal emits them around pasted text so applications can tell a paste from
// typing; Claude and Codex both use them to fill the input box without
// submitting.
var (
	pasteIntroducer = []byte("\x1b[200~")
	pasteTerminator = []byte("\x1b[201~")
)

// BracketedPaste wraps text in the bracketed-paste introducer/terminator so it
// lands in the agent's input box WITHOUT auto-submitting the turn.
//
// There is deliberately no trailing newline: a newline inside (or after) the
// paste is what the agent reads as "send", and submitting here would fire the
// turn before the user has typed the prompt that gives the pasted text meaning.
// This is the single construction site for the sequence — build paste payloads
// with it rather than concatenating the escapes by hand.
func BracketedPaste(text string) []byte {
	out := make([]byte, 0, len(pasteIntroducer)+len(text)+len(pasteTerminator))
	out = append(out, pasteIntroducer...)
	out = append(out, text...)
	out = append(out, pasteTerminator...)
	return out
}

// maxPendingPasteBytes caps the paste body buffered while waiting for the
// terminator. A dropped-file path is a few hundred bytes at the very most, so
// 8192 is generous for the shape we intercept while still bounding growth when
// the user pastes a whole file's worth of text. Same fail-open policy as
// maxPendingFilterBytes: on overflow everything held is forwarded unchanged and
// scanning returns to passthrough, so a big paste is forwarded, never swallowed
// and never held indefinitely. Tradeoff: a paste larger than the cap is never
// offered for interception — acceptable, because nothing we intercept is
// anywhere near that size, and forwarding is always the safe answer.
const maxPendingPasteBytes = 8192

// maxPasteImageBytes caps the image size we are willing to intercept. The
// transport that uploads the file re-checks this; the check exists here so an
// over-cap image paste passes through as ordinary text (the user still sees the
// local path, and can react) rather than being swallowed and then failing to
// upload.
const maxPasteImageBytes = 10 << 20 // 10 MiB

// imagePasteExtensions are the extensions (lowercased) we treat as an image
// drag. Deliberately short: anything not listed passes through as text.
var imagePasteExtensions = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".webp": true,
	".bmp":  true,
}

type pasteScanState int

const (
	// pasteStateIdle scans for the introducer; everything else is forwarded.
	pasteStateIdle pasteScanState = iota
	// pasteStatePaste buffers the body until the terminator arrives.
	pasteStatePaste
	// pasteStateFlush forwards an over-cap paste as it arrives, tracking the
	// terminator only so idle scanning resumes at the right byte.
	pasteStateFlush
)

// pasteScanner buffers a bracketed paste so a completed body can be offered to
// an interceptor before it reaches the agent.
//
// It sits in the path of every keystroke the user sends, so the governing rule
// is that anything ambiguous passes through byte for byte: the concatenation of
// every feed return equals the concatenation of every input, minus exactly the
// pastes that were claimed.
//
// Not concurrency-safe: it is driven from a single stdin read loop and holds
// carry-over state between chunks.
type pasteScanner struct {
	claim func(body []byte) bool

	state pasteScanState
	// held is a proper prefix of the introducer seen at the end of a chunk,
	// reconsidered (in its original position) on the next feed.
	held []byte
	// body accumulates the paste body while in pasteStatePaste.
	body []byte
	// flushMatch counts terminator bytes matched so far in pasteStateFlush.
	flushMatch int
}

// newPasteScanner returns a scanner that offers each completed paste body to
// claim, which reports whether it consumed the paste. A nil claim means no
// interception at all: feed becomes the identity function and no state is kept.
//
// The body handed to claim is a private copy, safe to retain.
func newPasteScanner(claim func(body []byte) bool) *pasteScanner {
	return &pasteScanner{claim: claim}
}

// feed returns the bytes that must be forwarded to the agent for this chunk.
func (s *pasteScanner) feed(data []byte) []byte {
	// No interceptor, no machinery. This is what makes "local (non --host)
	// mode is byte-for-byte unchanged" true by construction rather than by
	// every branch below happening to agree.
	if s.claim == nil {
		return data
	}
	if len(data) == 0 && len(s.held) == 0 {
		return data
	}

	buf := data
	if len(s.held) > 0 {
		joined := make([]byte, 0, len(s.held)+len(data))
		joined = append(joined, s.held...)
		joined = append(joined, data...)
		buf = joined
		s.held = s.held[:0]
	}

	out := make([]byte, 0, len(buf))
	for len(buf) > 0 {
		switch s.state {
		case pasteStateIdle:
			buf = s.scanIdle(buf, &out)
		case pasteStatePaste:
			buf = s.scanPaste(buf, &out)
		case pasteStateFlush:
			buf = s.scanFlush(buf, &out)
		}
	}
	return out
}

// scanIdle forwards bytes until an introducer is found, returning the
// unconsumed remainder.
func (s *pasteScanner) scanIdle(buf []byte, out *[]byte) []byte {
	if idx := bytes.Index(buf, pasteIntroducer); idx >= 0 {
		*out = append(*out, buf[:idx]...)
		s.state = pasteStatePaste
		s.body = s.body[:0]
		return buf[idx+len(pasteIntroducer):]
	}
	// No complete introducer in this chunk. Hold back exactly the trailing
	// bytes that could still become one; everything before them is real input
	// and must not wait on the next read.
	keep := partialSuffixLen(buf, pasteIntroducer)
	*out = append(*out, buf[:len(buf)-keep]...)
	if keep > 0 {
		s.held = append(s.held[:0], buf[len(buf)-keep:]...)
	}
	return nil
}

// scanPaste buffers the paste body, offering it to claim once the terminator
// arrives, and returns the unconsumed remainder of the chunk.
func (s *pasteScanner) scanPaste(buf []byte, out *[]byte) []byte {
	// Ctrl+X / Ctrl+] is the user's guaranteed way out of an attached pane, and
	// buffering a paste body must never be able to swallow it. Without this,
	// a paste whose terminator never arrives — a terminal that aborts mid-paste,
	// or a terminator eaten by the query-reply strip that runs just upstream —
	// holds every subsequent keystroke, INCLUDING the detach key, until 8 KiB
	// have accumulated. From the user's seat that is a hung terminal with the
	// one escape hatch disabled, which is a worse failure than any paste this
	// scanner exists to improve.
	//
	// Flushing verbatim and returning to passthrough puts the key in front of
	// Run's detach scan on this very chunk — exactly what that scan saw before
	// this scanner existed, so a detach byte inside a paste behaves as it always
	// has. It costs only the interception of a paste whose body contains a
	// control byte no image path we would claim can carry.
	//
	// Run drops the whole chunk it detaches on (it has since before this
	// scanner existed), so the flushed body does not reach the agent either.
	// That is the right trade: it can only happen when a paste was already
	// aborted mid-flight AND the user asked to leave, and delivering a
	// half-paste into a pane they are walking away from buys nothing.
	if containsDetachSequence(buf) {
		*out = append(*out, pasteIntroducer...)
		*out = append(*out, s.body...)
		*out = append(*out, buf...)
		s.body = s.body[:0]
		s.state = pasteStateIdle
		return nil
	}

	if len(s.body)+len(buf) > maxPendingPasteBytes {
		// Fail open: forward everything held so far unchanged and stop
		// buffering. The rest of this paste streams through in
		// pasteStateFlush.
		//
		// The test is the PROJECTED total, not this chunk alone, so a single
		// huge write cuts over as readily as a slow dribble. Which exact byte
		// it cuts over on still depends on how the terminal chunked the read —
		// a body that lands within a few hundred bytes of the cap can be
		// intercepted or not depending on chunking. That is harmless: the
		// stream is conserved either way, and nothing this scanner intercepts
		// is within three orders of magnitude of the cap.
		*out = append(*out, pasteIntroducer...)
		*out = append(*out, s.body...)
		s.flushMatch = partialSuffixLen(s.body, pasteTerminator)
		s.body = s.body[:0]
		s.state = pasteStateFlush
		return buf
	}

	prev := len(s.body)
	s.body = append(s.body, buf...)

	// A terminator can straddle the chunk boundary, so rescan from far enough
	// back to catch one that started in the previous chunk.
	from := prev - (len(pasteTerminator) - 1)
	if from < 0 {
		from = 0
	}
	idx := bytes.Index(s.body[from:], pasteTerminator)
	if idx < 0 {
		return nil
	}
	idx += from

	body := s.body[:idx]
	rest := append([]byte(nil), s.body[idx+len(pasteTerminator):]...)
	if !s.claimBody(body) {
		*out = append(*out, pasteIntroducer...)
		*out = append(*out, body...)
		*out = append(*out, pasteTerminator...)
	}
	s.body = s.body[:0]
	s.state = pasteStateIdle
	return rest
}

// claimBody offers a private copy of the body to the interceptor.
func (s *pasteScanner) claimBody(body []byte) bool {
	return s.claim(append([]byte(nil), body...))
}

// scanFlush forwards an over-cap paste verbatim, returning to idle after the
// terminator so a later paste can still be intercepted.
func (s *pasteScanner) scanFlush(buf []byte, out *[]byte) []byte {
	for i := 0; i < len(buf); i++ {
		if buf[i] == pasteTerminator[s.flushMatch] {
			s.flushMatch++
			if s.flushMatch == len(pasteTerminator) {
				*out = append(*out, buf[:i+1]...)
				s.flushMatch = 0
				s.state = pasteStateIdle
				return buf[i+1:]
			}
			continue
		}
		// Restart the match. ESC appears only at index 0 of the terminator, so
		// a mismatch can only ever restart a match at this byte.
		if buf[i] == pasteTerminator[0] {
			s.flushMatch = 1
		} else {
			s.flushMatch = 0
		}
	}
	*out = append(*out, buf...)
	return nil
}

// partialSuffixLen returns the length of the longest proper prefix of pat that
// buf ends with, or 0 if there is none.
func partialSuffixLen(buf, pat []byte) int {
	maxLen := len(pat) - 1
	if maxLen > len(buf) {
		maxLen = len(buf)
	}
	for k := maxLen; k > 0; k-- {
		if bytes.HasSuffix(buf, pat[:k]) {
			return k
		}
	}
	return 0
}

// imagePastePath returns the local filesystem path a pasted body names, and
// whether the body is a paste boss should intercept.
//
// Every branch defaults to "not intercepted": anything unrecognised, ambiguous,
// or unverifiable returns ("", false) and therefore passes through to the agent
// as the plain text the user pasted.
//
// The os.Stat below runs SYNCHRONOUSLY on the stdin read-loop goroutine, and
// that is a considered trade rather than an oversight. The decision it makes is
// claim-or-passthrough, and the acceptance criteria require a path that does not
// exist locally to pass through UNTOUCHED — so the existence check cannot be
// deferred to the upload goroutine the way the ssh copy is, because by then the
// paste has already been swallowed. The residual risk is a dragged path on a
// hung network mount blocking the loop; it is accepted because the same
// goroutine already blocks on proc.WriteInput, because the trigger requires
// dragging a file off a wedged mount, and because bounding it would mean a
// goroutine and a deadline inside the most safety-critical predicate here. Do
// not "fix" this by claiming first and statting later — that trades a rare stall
// for a routine swallowed paste.
func imagePastePath(body []byte) (string, bool) {
	// Leading/trailing whitespace is what a drag adds around the path; a
	// newline or carriage return anywhere inside means this is prose or a
	// multi-line paste, not a dropped file.
	text := strings.Trim(string(body), asciiWhitespace)
	if text == "" || strings.ContainsAny(text, "\n\r") {
		return "", false
	}

	path, ok := decodePastedPath(text)
	if !ok {
		return "", false
	}
	path, ok = absolutePastedPath(path)
	if !ok {
		return "", false
	}
	if !imagePasteExtensions[strings.ToLower(filepath.Ext(path))] {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if !info.Mode().IsRegular() {
		return "", false
	}
	if info.Size() > maxPasteImageBytes {
		return "", false
	}
	return path, true
}

const asciiWhitespace = " \t\n\r\v\f"

// decodePastedPath recognises the forms a terminal produces when a file is
// dragged in — a bare path, backslash-escaped spaces, a fully single- or
// double-quoted path, or a file:// URL — and nothing else. A body that is only
// partially quoted, or that carries an escape we do not model, is rejected
// rather than guessed at.
func decodePastedPath(text string) (string, bool) {
	switch {
	case strings.HasPrefix(text, "'"):
		return unquoteSinglePastedPath(text)
	case strings.HasPrefix(text, `"`):
		return unquoteDoublePastedPath(text)
	case strings.HasPrefix(text, "file://"):
		return decodePastedFileURL(text)
	default:
		return unescapeBarePastedPath(text)
	}
}

// unescapeBarePastedPath handles the unquoted form, where a terminal escapes
// spaces (and other shell metacharacters) with a backslash. Any unescaped
// whitespace means the body is more than one token, so it is not a path.
func unescapeBarePastedPath(text string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\\':
			if i+1 >= len(text) {
				return "", false // trailing backslash: unparseable
			}
			i++
			b.WriteByte(text[i])
		case strings.IndexByte(asciiWhitespace, c) >= 0:
			return "", false // a second token
		case c == '\'' || c == '"':
			return "", false // partially quoted: do not guess
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// unquoteSinglePastedPath handles a fully single-quoted path, including the
// POSIX idiom terminals use to embed a single quote: close the quote, emit an
// escaped quote, reopen.
func unquoteSinglePastedPath(text string) (string, bool) {
	var b strings.Builder
	for i := 1; i < len(text); {
		if text[i] != '\'' {
			b.WriteByte(text[i])
			i++
			continue
		}
		if i == len(text)-1 {
			return b.String(), true
		}
		if strings.HasPrefix(text[i:], `'\''`) {
			b.WriteByte('\'')
			i += 4
			continue
		}
		return "", false // the quote closed early: trailing junk
	}
	return "", false // unterminated
}

// unquoteDoublePastedPath handles a fully double-quoted path with \" and \\
// escapes. Any other escape is rejected rather than interpreted.
func unquoteDoublePastedPath(text string) (string, bool) {
	var b strings.Builder
	for i := 1; i < len(text); {
		switch text[i] {
		case '\\':
			if i+1 >= len(text) {
				return "", false
			}
			next := text[i+1]
			if next != '"' && next != '\\' {
				return "", false
			}
			b.WriteByte(next)
			i += 2
		case '"':
			if i == len(text)-1 {
				return b.String(), true
			}
			return "", false // the quote closed early: trailing junk
		default:
			b.WriteByte(text[i])
			i++
		}
	}
	return "", false // unterminated
}

// decodePastedFileURL handles the file:///path and file://localhost/path forms
// terminals emit. It only accepts a body that percent-decodes cleanly — if it
// does not, the body is not a file URL we understand and passes through.
func decodePastedFileURL(text string) (string, bool) {
	if strings.ContainsAny(text, asciiWhitespace) || strings.ContainsAny(text, "?#") {
		return "", false
	}
	rest := strings.TrimPrefix(text, "file://")
	rest = strings.TrimPrefix(rest, "localhost")
	if !strings.HasPrefix(rest, "/") {
		return "", false // a remote host, or a form we do not model
	}
	path, err := url.PathUnescape(rest)
	if err != nil {
		return "", false
	}
	return path, true
}

// absolutePastedPath expands a leading ~/ and requires the result to be
// absolute. A bare relative path is far too easily ordinary prose ("notes.png
// is stale"), so it is never intercepted.
func absolutePastedPath(path string) (string, bool) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return "", false
	}
	return filepath.Clean(path), true
}
