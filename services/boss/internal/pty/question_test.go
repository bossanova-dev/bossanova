package pty

import (
	"bytes"
	"testing"
)

// Delegation smoke tests. Full coverage lives in lib/bossalib/statusdetect.

func TestStripANSI_Delegation(t *testing.T) {
	got := stripANSI([]byte("\x1b[32mgreen\x1b[0m"))
	if !bytes.Equal(got, []byte("green")) {
		t.Errorf("stripANSI delegation failed: got %q", got)
	}
}

func TestHasQuestionPrompt_Delegation(t *testing.T) {
	// AskUserQuestion prompt should be detected.
	data := "  ❯ Allow\n    Allow once\n    Deny\n"
	if !hasQuestionPrompt([]byte(data)) {
		t.Error("hasQuestionPrompt delegation failed: should detect question")
	}

	// Plain text should not be detected.
	if hasQuestionPrompt([]byte("just plain text\n")) {
		t.Error("hasQuestionPrompt delegation failed: should not detect question")
	}
}

func TestLastNLines_Delegation(t *testing.T) {
	data := []byte("line1\nline2\nline3\n")
	got := lastNLines(data, 1)
	want := []byte("line3\n")
	if !bytes.Equal(got, want) {
		t.Errorf("lastNLines delegation failed: got %q, want %q", got, want)
	}
}
