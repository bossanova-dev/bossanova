package pty

import "strings"

// maxPendingFilterBytes caps the carry-over buffer used when a terminal
// query reply is split across reads. DA replies are typically <30 bytes
// and XTVERSION replies are short version strings, so 256 is generous
// while still bounding pathological growth on malformed input.
const maxPendingFilterBytes = 256

// stripTerminalQueryReplies removes terminal capability-query responses
// from a stdin chunk. These patterns are filtered:
//
//   - DA1 reply:        \x1b[?<digits/semicolons>c
//   - kitty flags:      \x1b[?<digits>u
//   - DECRPM reply:     \x1b[?<digits/semicolons>$y
//   - DA2 reply:        \x1b[><digits/semicolons>c
//   - XTWINOPS reply:   \x1b[<digits/semicolons>t
//   - DCS reply (incl.  \x1bP><something>|<text>\x1b\\
//     XTVERSION):
//
// These shapes are emitted by the outer terminal in response to capability
// probes that tmux's client startup sends on every attach. The replies land
// on stdin during the brief window between tmux's attach handshake and the
// inner pane becoming the input consumer, leaking into Claude's input box
// as visible garbage like "?62;22;52c". Real keystrokes never start with
// "ESC [ ?" or "ESC [ >" or "ESC P" followed by these specific shapes, so
// stripping them is safe for kitty CSI-u, xterm modifyOtherKeys, and
// bracketed-paste sequences.
//
// pending carries the tail of a previous chunk that ended mid-sequence;
// callers must thread it through across reads. If the chunk ends with an
// incomplete candidate, those bytes are returned in newPending instead of
// being forwarded. If a carried pending candidate is still incomplete after
// one continuation read, it is forwarded fail-open and flushedStalePending is
// true.
func stripTerminalQueryReplies(data, pending []byte) (filtered, newPending []byte, flushedStalePending bool) {
	if len(data) == 0 && len(pending) == 0 {
		return nil, nil, false
	}

	buf := make([]byte, 0, len(pending)+len(data))
	buf = append(buf, pending...)
	buf = append(buf, data...)

	out := make([]byte, 0, len(buf))
	i := 0
	for i < len(buf) {
		if buf[i] != 0x1b {
			out = append(out, buf[i])
			i++
			continue
		}

		end, matched, complete := matchQueryReply(buf[i:])
		if !complete {
			tail := buf[i:]
			// Fail open: a candidate carried in pending is still incomplete
			// after one continuation read, so forward it rather than holding
			// it across further reads and starving real keystrokes (BOS-55).
			// Tradeoff: a legitimate reply split across three or more reads
			// leaks its fragment instead of being stripped — see
			// docs/plans/2026-06-23-tui-pty-input-freeze.md "Risks".
			if len(pending) > 0 && len(data) > 0 && i < len(pending) {
				out = append(out, tail...)
				return out, nil, true
			}
			if len(tail) > maxPendingFilterBytes {
				// Held more than we should — flush as-is rather than
				// holding indefinitely on malformed input.
				out = append(out, tail...)
				return out, nil, false
			}
			held := make([]byte, len(tail))
			copy(held, tail)
			return out, held, false
		}
		if matched {
			i += end
			continue
		}
		out = append(out, buf[i])
		i++
	}
	return out, nil, false
}

// matchQueryReply inspects a buffer that begins with ESC and decides whether
// it starts one of the target reply shapes. Returns:
//
//   - end:      bytes consumed by the matched sequence (only valid when
//     complete && matched).
//   - matched:  true if the prefix is one of our targets.
//   - complete: false if the buffer was truncated mid-sequence and the
//     caller should hold from ESC onwards for the next read.
//
// When complete && !matched, the caller emits ESC and resumes scanning at
// the next byte — so e.g. arrow keys (\x1b[A) and bracketed-paste
// (\x1b[200~) pass through naturally.
func matchQueryReply(buf []byte) (end int, matched, complete bool) {
	if len(buf) < 2 {
		return 0, false, false
	}
	switch buf[1] {
	case '[':
		if len(buf) < 3 {
			return 0, false, false
		}
		// Reply shapes that start with ESC [:
		//   ESC [ ? <digits;...> c      DA1 reply
		//   ESC [ ? <digits;...> u      kitty keyboard flags reply
		//   ESC [ ? <digits;...> $ y    DECRPM (mode report) reply
		//   ESC [ > <digits;...> c      DA2 reply
		//   ESC [   <digits;...> t      XTWINOPS reply (window/text-area size)
		// The kitty and DECRPM replies answer Bubble Tea's own probes (CSI ? u,
		// CSI ? 2026 $ p, CSI ? 2027 $ p). tmux never sends either query, so
		// no attached child is waiting for them; forwarded, tmux turns them
		// into keystrokes and the agent's composer shows "2026;2$y".
		// Anything else (arrow keys ESC[A, kitty CSI-u ESC[120;5u, which has
		// no '?', modifyOtherKeys ESC[27;5;120~, bracketed-paste ESC[200~)
		// falls through to passthrough.
		var startParams int
		var finals string
		decrpm := false
		switch buf[2] {
		case '?':
			startParams, finals, decrpm = 3, "cu", true
		case '>':
			startParams, finals = 3, "c"
		default:
			if buf[2] < '0' || buf[2] > '9' {
				return 0, false, true
			}
			startParams, finals = 2, "t"
		}
		for j := startParams; j < len(buf); j++ {
			b := buf[j]
			switch {
			case strings.IndexByte(finals, b) >= 0:
				return j + 1, true, true
			case decrpm && b == '$':
				if j+1 == len(buf) {
					return 0, false, false
				}
				if buf[j+1] == 'y' {
					return j + 2, true, true
				}
				return 0, false, true
			case (b < '0' || b > '9') && b != ';':
				return 0, false, true
			}
		}
		return 0, false, false

	case 'P':
		// DCS. Accept any "ESC P ... ESC \\" — XTVERSION replies use
		// "ESC P > | <text> ESC \\" but tmux/iTerm/Ghostty also emit
		// other DCS replies (e.g. XTGETTCAP) that we don't want
		// landing in the input box either. Limiting to the '>|' prefix
		// would miss those.
		for j := 2; j < len(buf)-1; j++ {
			if buf[j] == 0x1b && buf[j+1] == '\\' {
				return j + 2, true, true
			}
		}
		return 0, false, false

	default:
		return 0, false, true
	}
}
