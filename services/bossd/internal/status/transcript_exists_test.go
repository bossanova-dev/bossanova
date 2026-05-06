package status

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptExists(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	const worktree = "/tmp/work/myrepo"
	const sessionID = "abc-123"

	// Initially: no transcript file → false.
	if got := TranscriptExists(worktree, sessionID); got {
		t.Fatalf("TranscriptExists with no file: got true, want false")
	}

	// Create the JSONL at the expected path.
	dir := filepath.Join(tmpHome, ".claude", "projects", "-tmp-work-myrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if got := TranscriptExists(worktree, sessionID); !got {
		t.Fatalf("TranscriptExists with file: got false, want true")
	}
}
