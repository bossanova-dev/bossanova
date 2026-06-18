package sessionreason

import (
	"errors"
	"testing"
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

func TestFixLoopExhausted(t *testing.T) {
	if got := FixLoopExhausted(); got != "fix loop exhausted, needs human intervention" {
		t.Fatalf("FixLoopExhausted = %q", got)
	}
}
