package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// copyFixture copies a file from src to dst (creating dst's parent dirs).
// Used by tests that need to drop transcript fixtures into a temporary
// ~/.codex/sessions/ shard.
func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open src %s: %v", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create dst %s: %v", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy: %v", err)
	}
}

// shardedRolloutPath returns the canonical codex sessions filename for a
// given UUID anchored at root: root/YYYY/MM/DD/rollout-<iso>-<uuid>.jsonl.
// Date matches the test fixture timestamps for fidelity, not today's date.
func shardedRolloutPath(root, uuid string) string {
	ts := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)
	dir := filepath.Join(root,
		ts.Format("2006"),
		ts.Format("01"),
		ts.Format("02"),
	)
	name := "rollout-" + ts.Format("2006-01-02T15-04-05") + "-" + uuid + ".jsonl"
	return filepath.Join(dir, name)
}

// TestTranscriptPathFindsShardedFile verifies findRolloutPath globs the
// YYYY/MM/DD shard tree and returns the rollout file for a given UUID.
func TestTranscriptPathFindsShardedFile(t *testing.T) {
	root := t.TempDir()
	uuid := "abcd-1234"
	dst := shardedRolloutPath(root, uuid)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

	got, err := findRolloutPath(root, uuid)
	if err != nil {
		t.Fatalf("findRolloutPath: %v", err)
	}
	if got != dst {
		t.Errorf("path = %q, want %q", got, dst)
	}
}

// TestTranscriptPathReturnsErrorWhenMissing exercises the "no rollout for
// session" error path — used by transcriptExists to safely return false.
func TestTranscriptPathReturnsErrorWhenMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := findRolloutPath(root, "missing-uuid"); err == nil {
		t.Error("expected error for missing rollout, got nil")
	}
}

// TestTranscriptPathPicksMostRecentOnMultiMatch verifies that when more
// than one rollout file matches a UUID (clock skew, crashed-and-restarted
// runs, or a daemon that re-resumed) we return the most-recently-modified
// match rather than failing. The plan-spec'd behavior — earlier versions
// of this code returned an "ambiguous" error which propagated to
// transcriptExists() collapsing to false, defeating the resume path.
func TestTranscriptPathPicksMostRecentOnMultiMatch(t *testing.T) {
	root := t.TempDir()
	uuid := "duplicate-uuid"

	// Stamp the older rollout into a 2026-05-01 shard.
	older := filepath.Join(root, "2026", "05", "01",
		"rollout-2026-05-01T00-00-00-"+uuid+".jsonl")
	copyFixture(t, "testdata/transcripts/sample.jsonl", older)
	oldTime := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes older: %v", err)
	}

	// Stamp the newer rollout into a 2026-05-08 shard.
	newer := filepath.Join(root, "2026", "05", "08",
		"rollout-2026-05-08T07-45-47-"+uuid+".jsonl")
	copyFixture(t, "testdata/transcripts/sample.jsonl", newer)
	newTime := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)
	if err := os.Chtimes(newer, newTime, newTime); err != nil {
		t.Fatalf("chtimes newer: %v", err)
	}

	got, err := findRolloutPath(root, uuid)
	if err != nil {
		t.Fatalf("findRolloutPath: %v", err)
	}
	if got != newer {
		t.Errorf("findRolloutPath = %q, want %q (most recently modified)", got, newer)
	}
}

// TestChatTitleExtractsFirstUserMessage verifies the chat-title scan picks
// the first event_msg/user_message text out of a real codex transcript
// (sample.jsonl, which begins with the developer prompt + an
// environment_context user message + the real "say hello and exit").
func TestChatTitleExtractsFirstUserMessage(t *testing.T) {
	got := chatTitleAtPath("testdata/transcripts/sample.jsonl")
	want := "say hello and exit"
	if got != want {
		t.Errorf("chatTitleAtPath = %q, want %q", got, want)
	}
}

// TestLastTurnIsUserHandlesCodexFormat verifies the codex-specific JSONL
// envelope walker: it returns true when the last meaningful entry is an
// event_msg/user_message, and false when the transcript ends with an
// agent_message (or only contains assistant turns).
func TestLastTurnIsUserHandlesCodexFormat(t *testing.T) {
	if !lastTurnIsUser("testdata/transcripts/last_user.jsonl") {
		t.Error("expected lastTurnIsUser=true for last_user.jsonl (ends in user_message)")
	}
	// sample.jsonl ends with task_complete + agent_message — the last
	// meaningful turn is agent.
	if lastTurnIsUser("testdata/transcripts/sample.jsonl") {
		t.Error("expected lastTurnIsUser=false for sample.jsonl (ends in agent_message)")
	}
}

// TestLastTurnIsUserTreatsTaskCompleteAsAgentTurn pins the contract from
// the codex Lane 0 spike: a turn that ends with `task_complete` (the
// envelope codex emits when the agent finishes, regardless of whether it
// also produced an `agent_message`) belongs to the agent. The bug it
// guards against: a transcript shaped `user_message → task_complete` (the
// agent's response was all tool calls / no final text, so no
// `agent_message` was emitted) would walk past the task_complete, hit the
// preceding user_message, and wrongly report user-last — which suppresses
// legitimate question-state detection downstream.
func TestLastTurnIsUserTreatsTaskCompleteAsAgentTurn(t *testing.T) {
	if lastTurnIsUser("testdata/transcripts/user_then_task_complete.jsonl") {
		t.Error("expected lastTurnIsUser=false for user_then_task_complete.jsonl " +
			"(transcript ends with task_complete; agent finished its turn)")
	}
}

// TestTranscriptExistsAcrossStates covers the "happy path", "no file",
// and "empty file" branches of transcriptExists. We point HOME at a temp
// dir so transcriptPath's globbing sees only fixtures we control.
func TestTranscriptExistsAcrossStates(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// 1) Missing → false.
	if transcriptExists("/anywhere", "no-such-uuid") {
		t.Error("transcriptExists should be false when no rollout exists")
	}

	// 2) Empty → false (file present but zero bytes).
	emptyUUID := "empty-uuid"
	emptyDst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), emptyUUID)
	copyFixture(t, "testdata/transcripts/empty.jsonl", emptyDst)
	if transcriptExists("/anywhere", emptyUUID) {
		t.Error("transcriptExists should be false for empty rollout file")
	}

	// 3) Real → true.
	realUUID := "abcd-1234"
	realDst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), realUUID)
	copyFixture(t, "testdata/transcripts/sample.jsonl", realDst)
	if !transcriptExists("/anywhere", realUUID) {
		t.Error("transcriptExists should be true for non-empty rollout file")
	}
}
