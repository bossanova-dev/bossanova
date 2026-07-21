package main

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
)

// capFixedNow anchors reset-time parsing deterministically.
var capFixedNow = time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)

// isolateCaptureDir redirects agenterr.CaptureBanner's appstate-resolved output
// (HOME on darwin, XDG_STATE_HOME on linux) into a throwaway dir so usage-cap
// tests never accumulate redacted banner snapshots in the developer's or CI's
// real state directory on every `go test`.
func isolateCaptureDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
}

// TestClassifyUsageCapFixture proves the vendored 429 error fixture — a
// retryable rate-limit ("rate limit exceeded, please retry") with no usage
// banner — is surfaced as the canonical agenterr.ErrRateLimited via the
// secondary status-code path, NOT ErrUsageLimited. A transient throttle must not
// be mislabeled as usage exhaustion (matches the claude twin, BOS-406).
func TestClassifyUsageCapFixture(t *testing.T) {
	isolateCaptureDir(t)
	err := classifyUsageCap(zerolog.Nop(), readFixture(t, "error_usage_429.jsonl"), capFixedNow)
	var rl agenterr.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("classifyUsageCap(429 fixture) = %v, want ErrRateLimited", err)
	}
	// It must NOT be the usage-exhaustion sentinel.
	var ul agenterr.ErrUsageLimited
	if errors.As(err, &ul) {
		t.Error("retryable 429 wrongly classified as ErrUsageLimited")
	}
}

// TestClassifyUsageCapWrappedTail proves the structured 429 path survives the
// PostExit tail shape. agentruntime's lineWriter wraps each event as NDJSON
// {"ts","text"}; before scanErrorEvents unwrapped the envelope, the secondary
// status-code path saw no top-level `error` object, so a wrapped bare-429 tail
// (whose message the taxonomy does not read as a usage banner) fell through to
// nil and the rate limit was lost. It must now classify as ErrRateLimited, and a
// wrapped clean/auth tail must still produce no cap.
func TestClassifyUsageCapWrappedTail(t *testing.T) {
	isolateCaptureDir(t)
	err := classifyUsageCap(zerolog.Nop(), wrapLog(t, readFixture(t, "error_usage_429.jsonl")), capFixedNow)
	var rl agenterr.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("classifyUsageCap(wrapped 429) = %v, want ErrRateLimited", err)
	}
	for _, name := range []string{"run_fresh.jsonl", "error_auth_401.jsonl"} {
		if err := classifyUsageCap(zerolog.Nop(), wrapLog(t, readFixture(t, name)), capFixedNow); err != nil {
			t.Errorf("classifyUsageCap(wrapped %s) = %v, want nil", name, err)
		}
	}
}

// TestClassifyUsageCapOnlyFromErrorFields guards the false-positive channel the
// positional-message fix opened: a cap token appearing anywhere OTHER than a
// provider error message must not manufacture a cap. Two non-error sources can
// carry the token — agentruntime's spawn preamble (which echoes the plan-bearing
// argv) and opencode's own assistant/tool events (which quote the task) — and a
// run that failed for an unrelated reason (here a 500) must classify as no cap.
// The paired positive case proves scanning only error fields never loses a real
// cap signal.
func TestClassifyUsageCapOnlyFromErrorFields(t *testing.T) {
	isolateCaptureDir(t)
	preamble := wrapLog(t, []byte(`[runner] spawning opencode: argv=[opencode run --format json --dir /w --auto fix the usage_limit_reached, resets at 15:00 bug] cwd=/w sessionID=ses_x PATH=/usr/bin`))

	t.Run("cap token in the argv preamble is not a cap on an unrelated failure", func(t *testing.T) {
		tail := append(append([]byte{}, preamble...), wrapLog(t, []byte(`{"type":"error","sessionID":"ses_x","error":{"statusCode":500,"message":"internal server error"}}`))...)
		if err := classifyUsageCap(zerolog.Nop(), tail, capFixedNow); err != nil {
			t.Fatalf("plan text in argv preamble misread as a cap: %v", err)
		}
	})

	t.Run("cap token in an assistant/tool event is not a cap on an unrelated failure", func(t *testing.T) {
		tail := wrapLog(t, []byte(`{"type":"step_start","sessionID":"ses_x"}
{"type":"text","sessionID":"ses_x","part":{"type":"text","text":"I'll fix the usage_limit_reached handling and the rate limit exceeded path"}}
{"type":"error","sessionID":"ses_x","error":{"statusCode":500,"message":"internal server error"}}`))
		if err := classifyUsageCap(zerolog.Nop(), tail, capFixedNow); err != nil {
			t.Fatalf("cap token in assistant text misread as a cap: %v", err)
		}
	})

	t.Run("a real 429 error line beside the same preamble still classifies", func(t *testing.T) {
		tail := append(append([]byte{}, preamble...), wrapLog(t, readFixture(t, "error_usage_429.jsonl"))...)
		err := classifyUsageCap(zerolog.Nop(), tail, capFixedNow)
		var rl agenterr.ErrRateLimited
		if !errors.As(err, &rl) {
			t.Fatalf("classifyUsageCap(preamble + real 429) = %v, want ErrRateLimited", err)
		}
	})

	t.Run("a real usage banner in an error message still classifies", func(t *testing.T) {
		tail := append(append([]byte{}, preamble...), wrapLog(t, []byte(`{"type":"error","sessionID":"ses_x","error":{"statusCode":429,"message":"stream error: usage_limit_reached, resets at 15:00"}}`))...)
		err := classifyUsageCap(zerolog.Nop(), tail, capFixedNow)
		var ul agenterr.ErrUsageLimited
		if !errors.As(err, &ul) {
			t.Fatalf("classifyUsageCap(preamble + real usage banner) = %v, want ErrUsageLimited", err)
		}
	})
}

// TestClassifyExitWrappedTail drives the REAL PostExit body against the wrapped
// tail agentruntime actually feeds it, locking the auth-precedence and 429
// contracts to production-shaped input rather than raw fixtures.
func TestClassifyExitWrappedTail(t *testing.T) {
	isolateCaptureDir(t)
	exit := errors.New("exit status 1")

	t.Run("wrapped 401 tail upgrades to ErrAuthRequired", func(t *testing.T) {
		got := classifyExit(zerolog.Nop(), exit, wrapLog(t, readFixture(t, "error_auth_401.jsonl")), capFixedNow)
		if !errors.Is(got, ErrAuthRequired) {
			t.Fatalf("got %v, want ErrAuthRequired on wrapped 401 tail", got)
		}
	})

	t.Run("wrapped 429 tail upgrades to ErrRateLimited", func(t *testing.T) {
		got := classifyExit(zerolog.Nop(), exit, wrapLog(t, readFixture(t, "error_usage_429.jsonl")), capFixedNow)
		var rl agenterr.ErrRateLimited
		if !errors.As(got, &rl) {
			t.Fatalf("got %v, want ErrRateLimited on wrapped 429 tail", got)
		}
	})
}

// TestClassifyUsageCapTextualBanner exercises the primary path: a usage banner
// the shared taxonomy recognizes as KindUsageExhausted, with a parseable reset.
func TestClassifyUsageCapTextualBanner(t *testing.T) {
	isolateCaptureDir(t)
	tail := []byte(`{"type":"error","error":{"statusCode":429,"message":"stream error: usage_limit_reached, resets at 15:00"}}`)
	err := classifyUsageCap(zerolog.Nop(), tail, capFixedNow)
	var ul agenterr.ErrUsageLimited
	if !errors.As(err, &ul) {
		t.Fatalf("classifyUsageCap = %v, want ErrUsageLimited", err)
	}
	if ul.ResetAt.IsZero() {
		t.Error("ResetAt is zero, want the parsed 15:00 reset time")
	}
	// The threaded reset must be exactly what the taxonomy produced (no drift).
	c := agenterr.Classify(string(tail), capFixedNow)
	if c.ResetAt == nil {
		t.Fatal("taxonomy produced no ResetAt for a 15:00 reset")
	}
	if !ul.ResetAt.Equal(*c.ResetAt) {
		t.Errorf("sentinel ResetAt = %v, taxonomy = %v", ul.ResetAt, *c.ResetAt)
	}
}

// TestClassifyUsageCapNilCases proves a clean run and an auth-only fixture never
// manufacture a usage cap.
func TestClassifyUsageCapNilCases(t *testing.T) {
	isolateCaptureDir(t)
	for _, name := range []string{"run_fresh.jsonl", "error_auth_401.jsonl", "error_auth_403.jsonl"} {
		if err := classifyUsageCap(zerolog.Nop(), readFixture(t, name), capFixedNow); err != nil {
			t.Errorf("classifyUsageCap(%s) = %v, want nil", name, err)
		}
	}
}

// TestClassifyExit drives the REAL PostExit body (classifyExit, the exact
// function wired into agentruntime.Options.PostExit by NewRunner) rather than a
// reconstruction of its branch logic, so the precedence contract and the
// non-nil-exit guard are locked to the code that actually runs. A reordering
// that checked usage before auth, or that dropped the orig!=nil guard, would fail
// here.
func TestClassifyExit(t *testing.T) {
	isolateCaptureDir(t)
	exit := errors.New("exit status 1")

	t.Run("nil exit is never reclassified even with a 429 in the tail", func(t *testing.T) {
		tail := []byte(`{"type":"error","error":{"statusCode":429,"message":"rate limit exceeded"}}`)
		if err := classifyExit(zerolog.Nop(), nil, tail, capFixedNow); err != nil {
			t.Fatalf("classifyExit(nil orig) = %v, want nil", err)
		}
	})

	t.Run("auth wins over usage when both markers are present", func(t *testing.T) {
		tail := []byte(`{"type":"error","error":{"statusCode":401,"message":"401 Unauthorized"}}
{"type":"error","error":{"statusCode":429,"message":"usage_limit_reached, resets at 15:00"}}`)
		got := classifyExit(zerolog.Nop(), exit, tail, capFixedNow)
		if !errors.Is(got, ErrAuthRequired) {
			t.Fatalf("got %v, want ErrAuthRequired when both markers present", got)
		}
		var ul agenterr.ErrUsageLimited
		if errors.As(got, &ul) {
			t.Error("auth tail wrongly classified as ErrUsageLimited")
		}
	})

	t.Run("retryable 429 maps to ErrRateLimited", func(t *testing.T) {
		tail := []byte(`{"type":"error","error":{"statusCode":429,"message":"429 Too Many Requests: rate limit exceeded, please retry"}}`)
		got := classifyExit(zerolog.Nop(), exit, tail, capFixedNow)
		var rl agenterr.ErrRateLimited
		if !errors.As(got, &rl) {
			t.Fatalf("got %v, want ErrRateLimited", got)
		}
	})

	t.Run("clean tail on a non-nil exit surfaces the original exit unchanged", func(t *testing.T) {
		if err := classifyExit(zerolog.Nop(), exit, readFixture(t, "run_fresh.jsonl"), capFixedNow); err != nil {
			t.Fatalf("classifyExit(clean tail) = %v, want nil (original exit preserved)", err)
		}
	})
}
