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
	// SettleMs/HardCapMs are optional per-op settle overrides (milliseconds).
	// 0 or absent ⇒ use the process-default settleConfig. Shared with the
	// BOS-217 daemon op, which reuses resolveSettle.
	SettleMs  int64 `json:"settleMs"`
	HardCapMs int64 `json:"hardCapMs"`
}

// Per-op settle-override clamp bounds (milliseconds). Positive request values
// are clamped into these ranges; non-positive values fall back to the base
// config. Shared by every settling op and by the BOS-217 daemon op.
const (
	minSettleMs  = 20
	maxSettleMs  = 5_000
	minHardCapMs = 100
	maxHardCapMs = 30_000
)

// resolveSettle returns base with stableFor/hardCap overridden from req's
// optional per-op values. poll/cols/rows are preserved. Non-positive override
// values keep the base value; positive values are clamped into the [min,max]
// bounds. The resolved hardCap is never below stableFor, so settle can always
// reach stability at least once.
func resolveSettle(base settleConfig, req request) settleConfig {
	out := base
	if req.SettleMs > 0 {
		out.stableFor = clampMs(req.SettleMs, minSettleMs, maxSettleMs)
	}
	if req.HardCapMs > 0 {
		out.hardCap = clampMs(req.HardCapMs, minHardCapMs, maxHardCapMs)
	}
	if out.hardCap < out.stableFor {
		out.hardCap = out.stableFor
	}
	return out
}

// clampMs clamps v (milliseconds) into [lo, hi] and returns a Duration.
func clampMs(v, lo, hi int64) time.Duration {
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return time.Duration(v) * time.Millisecond
}

// settle polls t.Screen() until the NORMALIZED screen is unchanged for
// st.stableFor (spinner animation ignored — see normalizeAnimation), or
// st.hardCap elapses. It returns the RAW final screen; only the stability
// comparison is normalized.
func settle(t tui, st settleConfig) string {
	deadline := time.Now().Add(st.hardCap)
	lastRaw := t.Screen()
	lastNorm := normalizeAnimation(lastRaw)
	stableSince := time.Now()
	for {
		if time.Now().After(deadline) {
			return lastRaw
		}
		time.Sleep(st.poll)
		curRaw := t.Screen()
		curNorm := normalizeAnimation(curRaw)
		if curNorm != lastNorm {
			lastRaw = curRaw
			lastNorm = curNorm
			stableSince = time.Now()
			continue
		}
		lastRaw = curRaw // keep the freshest raw frame while stable
		if time.Since(stableSince) >= st.stableFor {
			return lastRaw
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
		screen := settle(t, resolveSettle(st, req))
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
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "type":
		if serr := t.PasteString(req.Text); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "enter":
		if serr := t.SendString("\r"); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "esc":
		if serr := t.SendString("\x1b"); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
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
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
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
