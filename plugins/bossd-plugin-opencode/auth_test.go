package main

import "testing"

// TestDetectAuthFailureFixtures proves the two vendored real fixtures (401 and
// 403 error events) trip the detector, while a clean run and a 429-only usage
// fixture do not — auth detection keys off statusCode ∈ {401,403}, not any
// generic error presence.
func TestDetectAuthFailureFixtures(t *testing.T) {
	if !detectAuthFailure(readFixture(t, "error_auth_401.jsonl")) {
		t.Error("expected auth detection on the vendored 401 fixture")
	}
	if !detectAuthFailure(readFixture(t, "error_auth_403.jsonl")) {
		t.Error("expected auth detection on the vendored 403 fixture")
	}
	if detectAuthFailure(readFixture(t, "run_fresh.jsonl")) {
		t.Error("false positive: clean fresh run must not be an auth failure")
	}
	if detectAuthFailure(readFixture(t, "error_usage_429.jsonl")) {
		t.Error("false positive: a 429 usage/rate-limit event is not an auth failure")
	}
}

// TestDetectAuthFailureWrappedTail proves auth detection survives the PostExit
// tail shape: agentruntime's lineWriter wraps each opencode event as NDJSON
// {"ts","text"} with the real event escaped inside `text`. Before scanErrorEvents
// unwrapped the envelope, the top level carried no `error` object, so real
// 401/403 failures were misclassified as generic exits. The clean and 429 tails
// must still not read as auth even when wrapped.
func TestDetectAuthFailureWrappedTail(t *testing.T) {
	if !detectAuthFailure(wrapLog(t, readFixture(t, "error_auth_401.jsonl"))) {
		t.Error("expected auth detection on the wrapped 401 tail")
	}
	if !detectAuthFailure(wrapLog(t, readFixture(t, "error_auth_403.jsonl"))) {
		t.Error("expected auth detection on the wrapped 403 tail")
	}
	if detectAuthFailure(wrapLog(t, readFixture(t, "run_fresh.jsonl"))) {
		t.Error("false positive: wrapped clean fresh run must not be an auth failure")
	}
	if detectAuthFailure(wrapLog(t, readFixture(t, "error_usage_429.jsonl"))) {
		t.Error("false positive: wrapped 429 usage/rate-limit event is not an auth failure")
	}
}

// TestDetectAuthFailureIgnoresRunnerPreambleArgv proves a plan echoed into the
// argv spawn preamble cannot trip auth detection: the detector is structured
// (it needs an opencode `error` event), so an auth phrase in the `[runner]`
// argv line is never mistaken for a real 401/403.
func TestDetectAuthFailureIgnoresRunnerPreambleArgv(t *testing.T) {
	tail := append(
		wrapLog(t, []byte(`[runner] spawning opencode: argv=[opencode run --auto fix the 401 Unauthorized invalid_token bug] cwd=/w`)),
		wrapLog(t, readFixture(t, "run_fresh.jsonl"))...,
	)
	if detectAuthFailure(tail) {
		t.Error("auth phrase in the argv preamble was misread as a real auth failure")
	}
}

// TestDetectAuthFailureMessageMarkers proves each case-tolerant message marker
// trips the detector even without a 401/403 status code — opencode has no single
// canonical auth string, so a provider that returns an obscure status but a
// recognizable message is still caught.
func TestDetectAuthFailureMessageMarkers(t *testing.T) {
	for _, line := range []string{
		`{"type":"error","error":{"statusCode":500,"message":"Unauthorized"}}`,
		`{"type":"error","error":{"statusCode":0,"message":"Invalid API key provided"}}`,
		`{"type":"error","error":{"message":"request failed: invalid_token"}}`,
		`{"type":"error","error":{"message":"Token refresh failed after 3 attempts"}}`,
		// Case-insensitivity: an all-caps message still matches.
		`{"type":"error","error":{"message":"FATAL: UNAUTHORIZED"}}`,
	} {
		if !detectAuthFailure([]byte(line)) {
			t.Errorf("expected auth detection on %q", line)
		}
	}
}

// TestDetectAuthFailureIgnoresOtherErrors verifies the detector does not trip on
// unrelated failures. A false positive would mask real bugs by re-labelling them
// as auth failures.
func TestDetectAuthFailureIgnoresOtherErrors(t *testing.T) {
	for _, line := range []string{
		`{"type":"error","error":{"statusCode":500,"message":"internal server error"}}`,
		`{"type":"error","error":{"statusCode":429,"message":"Too Many Requests"}}`,
		`{"type":"text","part":{"text":"the 401 status code is documented in the api"}}`,
		`connection timed out`,
		`not json at all {oops`,
		``,
	} {
		if detectAuthFailure([]byte(line)) {
			t.Errorf("false positive on %q", line)
		}
	}
}

// TestErrAuthRequiredIsTyped verifies ErrAuthRequired implements error and
// carries a meaningful message — daemon-side callers do
// errors.Is(err, ErrAuthRequired) to distinguish auth failures from other exits.
func TestErrAuthRequiredIsTyped(t *testing.T) {
	// Assign through the error interface so the assertion uses the interface
	// type, not the concrete authErr (staticcheck flags a concrete non-nil
	// string compared to nil).
	var e error = ErrAuthRequired
	if got := e.Error(); got == "" {
		t.Error("ErrAuthRequired.Error() is empty")
	}
}
