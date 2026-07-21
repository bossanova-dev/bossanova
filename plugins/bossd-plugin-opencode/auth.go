package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// errorEvent is the tolerant projection of an opencode `--format json` error
// event: a JSONL line carrying a nested `error` object with a `statusCode` and
// `message`. opencode has NO single canonical auth-error string (it is
// provider-dependent), so both the auth detector and the usage classifier key
// off these two stable fields rather than a fixed message.
type errorEvent struct {
	StatusCode int
	Message    string
}

// scanErrorEvents parses a stdout/log tail into the error events it carries.
// The scan is line-scoped and failure-tolerant: non-`{` lines, malformed JSON,
// and lines without a nested `error` object are skipped so one garbled line
// never aborts detection. A line qualifies as an error event iff it unmarshals
// with a non-nil top-level `error` object; the event `type` string is NOT
// required to equal "error", so a future opencode release that files the same
// error object under a different type still classifies. A successful run emits
// no `error` object, so this returns nil for clean tails.
//
// Two tail shapes reach this scanner and both are handled per line:
//
//   - Raw opencode JSONL — the early-output/SessionIDFromOutput path is teed off
//     stdout before wrapping, so its lines are the agent's own events.
//   - agentruntime lineWriter envelopes — the PostExit hook is fed the per-session
//     log tail, where every line is NDJSON `{"ts":"...","text":"<raw line>"}` with
//     the real opencode event carried as a string in `text`. Without unwrapping,
//     the top level has no `error` object, so real 401/403/429 events would be
//     silently missed. Each line is first parsed as-is; if that yields no error
//     event and the line is an envelope, the `text` payload is unwrapped and
//     retried. Parsing raw first means a genuine error event that happens to
//     carry a top-level `text` field is never lost to a spurious unwrap.
func scanErrorEvents(data []byte) []errorEvent {
	var out []errorEvent
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if ev, ok := parseErrorEvent(line); ok {
			out = append(out, ev)
			continue
		}
		if inner := unwrapLogLine(line); inner != nil {
			if ev, ok := parseErrorEvent(bytes.TrimSpace(inner)); ok {
				out = append(out, ev)
			}
		}
	}
	return out
}

// parseErrorEvent unmarshals a single tail line into an errorEvent, reporting
// ok=false for non-`{` lines, malformed JSON, and lines carrying no top-level
// `error` object.
func parseErrorEvent(line []byte) (errorEvent, bool) {
	if !bytes.HasPrefix(line, []byte("{")) {
		return errorEvent{}, false
	}
	var entry struct {
		Error *struct {
			StatusCode int    `json:"statusCode"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &entry); err != nil || entry.Error == nil {
		return errorEvent{}, false
	}
	return errorEvent{StatusCode: entry.Error.StatusCode, Message: entry.Error.Message}, true
}

// unwrapLogLine returns the `text` payload of an agentruntime lineWriter NDJSON
// envelope (`{"ts":"...","text":"<raw line>"}`), or nil when line is not such an
// envelope. A `*string` distinguishes a present-but-empty `text` (an envelope
// wrapping a blank line) from an absent `text` key (a raw opencode event), so
// raw stdout lines return nil and are scanned as-is by the caller.
func unwrapLogLine(line []byte) []byte {
	var env struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(line, &env); err != nil || env.Text == nil {
		return nil
	}
	return []byte(*env.Text)
}

// detectAuthFailure reports whether the supplied tail (a stdout/log tail passed
// by agentruntime.Runner's PostExit hook) carries an opencode auth failure. An
// error event is an auth failure when its statusCode is 401 or 403 OR its
// message contains one of the case-tolerant auth markers. Either signal alone is
// sufficient because opencode surfaces auth failures differently depending on the
// provider transport.
func detectAuthFailure(data []byte) bool {
	for _, ev := range scanErrorEvents(data) {
		if ev.StatusCode == 401 || ev.StatusCode == 403 {
			return true
		}
		if hasAuthMarker(ev.Message) {
			return true
		}
	}
	return false
}

// hasAuthMarker reports whether msg contains one of the auth-failure substrings,
// matched case-insensitively (opencode message casing is provider-dependent).
func hasAuthMarker(msg string) bool {
	lower := strings.ToLower(msg)
	for _, marker := range authMessageMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// authMessageMarkers enumerates the lowercase auth-failure substrings
// detectAuthFailure matches against an error event's message. Kept lowercase so
// hasAuthMarker can do a single strings.ToLower on the candidate.
var authMessageMarkers = []string{
	"unauthorized",
	"invalid api key",
	"invalid_token",
	"token refresh failed",
}

// ErrAuthRequired is the typed sentinel surfaced via Runner.ExitError when an
// opencode run fails because auth is missing or expired. The daemon inspects it
// with errors.Is to distinguish a recoverable "user needs to run `opencode auth
// login`" failure from any other crash.
var ErrAuthRequired authErr = "opencode auth required: run `opencode auth login`"

// authErr is a typed string error so daemon callers can do errors.As / errors.Is
// on ErrAuthRequired without depending on a global pointer identity.
type authErr string

func (e authErr) Error() string { return string(e) }
