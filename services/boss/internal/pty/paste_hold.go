package pty

import "sync"

// Submit and cancel keys, as the terminal delivers them in raw mode.
const (
	// pasteSubmitCR is Enter. Terminals in raw mode send CR, not LF, so CR is
	// the byte that submits an agent turn.
	pasteSubmitCR = '\r'
	// pasteSubmitLF is accepted alongside CR because a terminal configured for
	// it, or an automated driver writing to the same fd, can send it instead.
	// Holding only CR would leave that path racing exactly as before.
	pasteSubmitLF = '\n'
	// pasteCancelETX is Ctrl+C. It abandons whatever is in the composer, which
	// is why it discards a held submit rather than releasing it.
	pasteCancelETX = 0x03
)

// pasteEnterHold withholds the user's Enter while a paste upload is still in
// flight, and replays it once the remote path has been injected.
//
// The race it closes: pasteClaim swallows the image paste, launches the upload
// asynchronously and returns immediately, so nothing in the keystroke path is
// waiting on the copy. A user who presses Enter before it lands sends a turn
// with the image removed and no path substituted — the agent is asked about an
// image it was never given — and the path then arrives in the NEXT composer.
// The status line makes a slow upload visible but enforces nothing.
//
// Holding a key the user actually pressed is a real cost, taken deliberately:
// a turn that silently loses its image is a worse surprise than an Enter that
// lands a moment late. The wait is bounded rather than open-ended — the
// upload's own Run is capped by WaitDelay — and both cancel keys below release
// the composer immediately, so the user is never stuck behind it.
//
// Concurrency: filter runs on the single stdin read loop, release on each
// upload goroutine. Every field is guarded.
type pasteEnterHold struct {
	mu sync.Mutex
	// pending counts uploads that have been launched but not yet finished.
	pending int
	// held records that a submit key was withheld and still owes a replay.
	held bool
}

// begin records a launched upload. It is called on the read-loop goroutine
// BEFORE the upload goroutine starts, so a submit key in the very next chunk
// already sees a non-zero pending count — the window this whole type exists to
// close would otherwise reopen at its own edge.
func (h *pasteEnterHold) begin() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending++
}

// filter returns the bytes to forward to the agent, withholding a submit key
// while an upload is in flight.
//
// A cancel key (Ctrl+C) is forwarded AND drops any held submit: the user has
// abandoned the composer, so replaying their earlier Enter later would submit a
// turn they just cancelled. Everything else passes through untouched, so the
// user can keep typing while the upload runs — which is the point of the
// asynchronous upload in the first place.
//
// With no upload pending this is the identity function, so the ordinary
// keystroke path is unchanged.
func (h *pasteEnterHold) filter(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == 0 && !h.held {
		return data
	}

	out := make([]byte, 0, len(data))
	for _, b := range data {
		switch {
		case b == pasteCancelETX:
			// Cancel supersedes a held submit, and is itself forwarded so the
			// agent sees the interrupt at once.
			h.held = false
			out = append(out, b)
		case (b == pasteSubmitCR || b == pasteSubmitLF) && h.pending > 0:
			// Withheld, not dropped: release replays it once the last upload
			// has injected its path.
			h.held = true
		default:
			out = append(out, b)
		}
	}
	return out
}

// release records that one upload has finished and reports whether the caller
// must now replay a withheld submit key.
//
// It returns true only when the LAST in-flight upload completes. Two images
// pasted in quick succession produce one composer with two paths, and the
// single Enter the user pressed belongs to that whole composer — replaying it
// when the first of them landed would submit a turn still missing the second.
//
// replay is deliberately consumed here (held is cleared as it is reported), so
// a second caller racing on the same completion cannot submit twice.
func (h *pasteEnterHold) release() (replay bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending > 0 {
		h.pending--
	}
	if h.pending > 0 || !h.held {
		return false
	}
	h.held = false
	return true
}

// discard drops a held submit without replaying it, and is what teardown calls.
// A detach means the composer is going away, so the Enter has nothing left to
// submit; replaying it into a process being torn down would be a keystroke the
// user never gets to see the result of.
func (h *pasteEnterHold) discard() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = false
}
