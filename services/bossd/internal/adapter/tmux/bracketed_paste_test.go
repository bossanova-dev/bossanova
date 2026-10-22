package tmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForBracketedPasteWaitsForApplicationEnableSequence(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	if err := os.WriteFile(logPath, []byte("Codex is ready\n\x1b[?2004h"), 0o600); err != nil {
		t.Fatalf("write pane log: %v", err)
	}

	if err := waitForBracketedPaste(context.Background(), logPath, 20*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("waitForBracketedPaste() = %v, want nil after application enabled bracketed paste", err)
	}
}
