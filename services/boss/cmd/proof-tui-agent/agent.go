package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/recurser/boss/internal/tuidriver"
)

// tui is the minimal surface the serve loop depends on. *tuidriver.Driver
// satisfies it directly; protocol tests pass a fake.
type tui interface {
	Screen() string
	SendString(s string) error
	PasteString(s string) error
	WaitForText(timeout time.Duration, text string) error
}

// errQuit is returned by serve when a "quit" op is processed so the caller can
// tear down and exit 0.
var errQuit = errors.New("quit")

// maxLineBytes caps a single NDJSON request line. A line over the cap yields an
// error response rather than an unbounded buffer or hang.
const maxLineBytes = 1 << 20 // 1 MiB

// settleConfig controls the stability auto-settle and carries the fixed
// terminal dimensions reported by observe.
type settleConfig struct {
	poll      time.Duration
	stableFor time.Duration
	hardCap   time.Duration
	cols      int
	rows      int
}

// request is one NDJSON input line.
type request struct {
	ID        int64    `json:"id"`
	Op        string   `json:"op"`
	Keys      []string `json:"keys"`
	Text      string   `json:"text"`
	TimeoutMs int64    `json:"timeoutMs"`
}

// settle polls t.Screen() until it is unchanged for st.stableFor, or st.hardCap
// elapses, and returns the final screen.
func settle(t tui, st settleConfig) string {
	deadline := time.Now().Add(st.hardCap)
	last := t.Screen()
	stableSince := time.Now()
	for {
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(st.poll)
		cur := t.Screen()
		if cur != last {
			last = cur
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= st.stableFor {
			return last
		}
	}
}

// writeJSON marshals v to a single line and flushes.
func writeJSON(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

// serve reads NDJSON requests from in and writes one NDJSON response per request
// to out. It returns nil on input EOF (parent death / closed stdin) and errQuit
// after acking a quit op. It never crashes or hangs on malformed input.
func serve(t tui, in io.Reader, out io.Writer, st settleConfig) error {
	bw := bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		quit, err := handleLine(t, bw, line, st)
		if err != nil {
			return err
		}
		if quit {
			return errQuit
		}
	}
	if err := scanner.Err(); err != nil {
		// A line over the cap (bufio.ErrTooLong) or read error: emit an error
		// response and stop. We cannot recover the request id past this point.
		_ = writeJSON(bw, errorResponse(0, err.Error()))
		return nil
	}
	return nil
}

// handleLine processes one request line. It returns quit=true after a quit op.
func handleLine(t tui, bw *bufio.Writer, line []byte, st settleConfig) (quit bool, err error) {
	var req request
	if uerr := json.Unmarshal(line, &req); uerr != nil {
		// id may still be parseable; best-effort recover it.
		return false, writeJSON(bw, errorResponse(parseID(line), uerr.Error()))
	}

	switch req.Op {
	case "observe":
		screen := settle(t, st)
		return false, writeJSON(bw, map[string]any{
			"id":     req.ID,
			"screen": screen,
			"cols":   st.cols,
			"rows":   st.rows,
		})
	case "key":
		for _, k := range req.Keys {
			b, kerr := tuidriver.KeyBytes(k)
			if kerr != nil {
				return false, writeJSON(bw, errorResponse(req.ID, kerr.Error()))
			}
			if serr := t.SendString(string(b)); serr != nil {
				return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
			}
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, st)))
	case "type":
		if serr := t.PasteString(req.Text); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, st)))
	case "enter":
		if serr := t.SendString("\r"); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, st)))
	case "esc":
		if serr := t.SendString("\x1b"); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, st)))
	case "wait":
		timeout := time.Duration(req.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		if werr := t.WaitForText(timeout, req.Text); werr != nil {
			return false, writeJSON(bw, map[string]any{
				"id":     req.ID,
				"ok":     false,
				"error":  "timeout",
				"screen": t.Screen(),
			})
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, st)))
	case "quit":
		return true, writeJSON(bw, map[string]any{"id": req.ID, "ok": true})
	default:
		return false, writeJSON(bw, errorResponse(req.ID, fmt.Sprintf("unknown op %q", req.Op)))
	}
}

func okScreen(id int64, screen string) map[string]any {
	return map[string]any{"id": id, "ok": true, "screen": screen}
}

func errorResponse(id int64, msg string) map[string]any {
	return map[string]any{"id": id, "ok": false, "error": msg}
}

// parseID best-effort extracts the "id" field from a malformed line so error
// responses can still echo it. Returns 0 if not recoverable.
func parseID(line []byte) int64 {
	var partial struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &partial); err != nil {
		return 0
	}
	return partial.ID
}
