package sessionreason

import (
	"errors"
	"testing"
	"time"
)

func TestFinalizeFailure(t *testing.T) {
	got := FinalizeFailure("chat_spawn_failed", errors.New("tmux missing"))
	want := "finalize failed (chat_spawn_failed): tmux missing"
	if got != want {
		t.Fatalf("FinalizeFailure = %q, want %q", got, want)
	}
}

func TestFinalizeFailureNilErr(t *testing.T) {
	got := FinalizeFailure("pr_failed", nil)
	want := "finalize failed (pr_failed)"
	if got != want {
		t.Fatalf("FinalizeFailure nil = %q, want %q", got, want)
	}
}

func TestFinalizeRecovered(t *testing.T) {
	if got := FinalizeRecovered(); got != "interrupted during finalize after daemon restart" {
		t.Fatalf("FinalizeRecovered = %q", got)
	}
}

func TestWithoutBlockedLabel(t *testing.T) {
	for in, want := range map[string]string{
		"run blocked: MAD-1751 implemented; awaiting review": "MAD-1751 implemented; awaiting review",
		RunBlocked("Run blocked: unresolved P1"):             "unresolved P1",
		"  BLOCKED:   needs a rerun  ":                       "needs a rerun",
		"Blocked: run blocked: run blocked: twice":           "twice",
		"finalize failed (pr_failed): blocked: boom":         "finalize failed (pr_failed): blocked: boom",
		"blocked on a person":                                "blocked on a person",
		" run blocked: ":                                     "run blocked:",
		"":                                                   "",
	} {
		if got := WithoutBlockedLabel(in); got != want {
			t.Errorf("WithoutBlockedLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFixLoopExhausted(t *testing.T) {
	if got := FixLoopExhausted(); got != "fix loop exhausted, needs human intervention" {
		t.Fatalf("FixLoopExhausted = %q", got)
	}
}

func TestRunBlocked(t *testing.T) {
	if got := RunBlocked("  unresolved P1 in review  "); got != "run blocked: unresolved P1 in review" {
		t.Fatalf("RunBlocked = %q", got)
	}
	if blank := RunBlocked(" "); blank != "run blocked: no reason given" {
		t.Fatalf("RunBlocked(blank) = %q", blank)
	}
}

func TestFinalizeQuotaWait(t *testing.T) {
	reset := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
	reason := FinalizeQuotaWait(reset)
	got, ok := ParseFinalizeQuotaWait(reason)
	if !ok || !got.Equal(reset) {
		t.Fatalf("round trip = %v, %v; want %v", got, ok, reset)
	}
	for _, reason := range []string{FinalizeFailure("pr_failed", errors.New("forbidden")), "finalize failed (quota_wait): invalid", "", FinalizeQuotaWait(time.Time{})} {
		if _, ok := ParseFinalizeQuotaWait(reason); ok {
			t.Errorf("parsed non-resumable reason %q", reason)
		}
	}
}
