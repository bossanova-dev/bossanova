package main

import (
	"bytes"
)

// detectAuthFailure reports whether the supplied byte slice (typically a
// log tail produced by agentruntime.Runner's PostExit hook) contains one of
// the auth-failure markers codex emits when its OpenAI credentials are
// missing or expired.
//
// Lane 0 spike captures showed two stable markers:
//
//   - `401 Unauthorized` — appears in stderr ("HTTP error: 401
//     Unauthorized, url: wss://api.openai.com/v1/responses")
//   - `Missing bearer or basic authentication` — appears in the
//     `turn.failed` JSONL event written to stdout
//
// Either marker is enough to trigger ErrAuthRequired; we do not require
// both because codex emits them independently depending on the auth
// transport (REST vs WebSocket).
func detectAuthFailure(data []byte) bool {
	for _, marker := range authFailureMarkers {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	return false
}

// authFailureMarkers enumerates the auth-failure substrings detectAuthFailure
// matches. Kept as []byte literals because PostExit receives []byte and
// callers should never have to allocate a string copy just to do membership
// checks.
var authFailureMarkers = [][]byte{
	[]byte("401 Unauthorized"),
	[]byte("Missing bearer or basic authentication"),
}

// ErrAuthRequired is the typed sentinel surfaced via Runner.ExitError when
// a codex run fails because auth is missing or expired. The daemon
// (Lane D) inspects this with errors.Is to distinguish a recoverable
// "user needs to run `codex login`" failure from any other crash.
var ErrAuthRequired authErr = "codex auth required: run `codex login`"

// authErr is a typed string error so daemon callers can do errors.As /
// errors.Is on ErrAuthRequired without depending on a global pointer
// identity.
type authErr string

func (e authErr) Error() string { return string(e) }
