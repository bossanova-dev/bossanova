package main

import "testing"

// TestDetectAuthFailureMatchesSpikeMarker verifies the two real-world
// markers captured by the Lane 0 spike trip the detector. The first is a
// stderr line from the WebSocket handshake; the second is a turn.failed
// JSONL event on stdout. Either alone is sufficient.
func TestDetectAuthFailureMatchesSpikeMarker(t *testing.T) {
	stderr := []byte(`2026-05-08T07:49:20.474615Z ERROR codex_api::endpoint::responses_websocket: failed to connect to websocket: HTTP error: 401 Unauthorized, url: wss://api.openai.com/v1/responses`)
	if !detectAuthFailure(stderr) {
		t.Error("expected detection on websocket 401 stderr line")
	}

	stdout := []byte(`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses"}}`)
	if !detectAuthFailure(stdout) {
		t.Error("expected detection on stdout turn.failed event")
	}
}

// TestDetectAuthFailureIgnoresOtherErrors verifies the detector does not
// trip on unrelated network or rate-limit failures. The PostExit hook
// must only upgrade the exit error when the markers are exact — any
// false positive would mask real bugs by re-labelling them as auth
// failures.
func TestDetectAuthFailureIgnoresOtherErrors(t *testing.T) {
	for _, c := range [][]byte{
		[]byte("connection timed out"),
		[]byte("rate limited: 429 Too Many Requests"),
		[]byte("network unreachable"),
	} {
		if detectAuthFailure(c) {
			t.Errorf("false positive on %q", c)
		}
	}
}

// TestErrAuthRequiredIsTyped verifies ErrAuthRequired implements the
// error interface and carries a meaningful message — daemon-side
// callers (Lane D) do `errors.Is(err, codex.ErrAuthRequired)` to
// distinguish auth failures from other exit errors.
func TestErrAuthRequiredIsTyped(t *testing.T) {
	// Assign through the error interface so the assertion below uses the
	// interface type, not the concrete authErr — staticcheck flags
	// `var e error = ErrAuthRequired; if e == nil` because the concrete
	// type is non-nil string with non-zero compile-time identity. Using
	// the .Error() result through the interface side-steps that check.
	var e error = ErrAuthRequired
	if got := e.Error(); got == "" {
		t.Error("ErrAuthRequired.Error() is empty")
	}
}
