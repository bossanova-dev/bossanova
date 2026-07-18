package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func writeSessionMetaRollout(t *testing.T, root, id, workDir, originator string, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(root,
		modTime.Format("2006"),
		modTime.Format("01"),
		modTime.Format("02"),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "rollout-"+modTime.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	line := fmt.Sprintf(
		`{"timestamp":"%s","type":"session_meta","payload":{"id":%q,"cwd":%q,"originator":%q,"cli_version":"test"}}`+"\n",
		modTime.Format(time.RFC3339Nano),
		id,
		workDir,
		originator,
	)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write rollout %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes rollout %s: %v", path, err)
	}
	return path
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

// TestFindRolloutPathRejectsTraversalSessionID verifies the BOS-415
// root-containment guard: an agentSessionID carrying path separators or ".."
// is rejected before it can collapse the glob pattern out of the sessions
// root, even when a matching file is planted outside the shard tree.
func TestFindRolloutPathRejectsTraversalSessionID(t *testing.T) {
	root := t.TempDir()

	// Plant a rollout-shaped file one level above the sessions root that a
	// traversal id would otherwise glob into.
	outside := filepath.Join(filepath.Dir(root), "rollout-2026-05-08T07-45-47-escape.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, id := range []string{"../escape", "a/b", `a\b`, ".."} {
		if _, err := findRolloutPath(root, id); err == nil {
			t.Errorf("findRolloutPath(root, %q) = nil error, want rejection", id)
		}
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
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

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

func TestResolveInteractiveSessionIDAtFindsMatchingCodexTUIRollout(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	modTime := launchedAfter.Add(500 * time.Millisecond)
	path := writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", modTime)

	id, gotPath, ambiguous, reason := resolveInteractiveSessionIDAt(root, workDir, launchedAfter)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveInteractiveSessionIDAtIgnoresOldExecAndDifferentCWD(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)

	writeSessionMetaRollout(t, root, "old-session", workDir, "codex-tui", launchedAfter.Add(-3*time.Second))
	writeSessionMetaRollout(t, root, "exec-session", workDir, "codex_exec", launchedAfter.Add(time.Second))
	writeSessionMetaRollout(t, root, "other-cwd", otherDir, "codex-tui", launchedAfter.Add(2*time.Second))

	id, path, ambiguous, reason := resolveInteractiveSessionIDAt(root, workDir, launchedAfter)

	if id != "" || path != "" {
		t.Errorf("got id/path = %q/%q, want empty", id, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "no matching codex-tui rollout found" {
		t.Errorf("reason = %q, want no matching reason", reason)
	}
}

func TestResolveInteractiveSessionIDAtReturnsAmbiguousForDifferentIDsInWindow(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)

	writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", launchedAfter.Add(time.Second))
	writeSessionMetaRollout(t, root, "session-2", workDir, "codex-tui", launchedAfter.Add(2*time.Second))

	id, path, ambiguous, reason := resolveInteractiveSessionIDAt(root, workDir, launchedAfter)

	if id != "" || path != "" {
		t.Errorf("got id/path = %q/%q, want empty on ambiguity", id, path)
	}
	if !ambiguous {
		t.Error("ambiguous = false, want true")
	}
	if reason == "" {
		t.Error("reason empty, want ambiguity reason")
	}
}

func TestResolveInteractiveSessionIDAtAcceptsSymlinkEquivalentCWD(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real workdir: %v", err)
	}
	linkDir := filepath.Join(t.TempDir(), "linked-work")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink workdir: %v", err)
	}
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	path := writeSessionMetaRollout(t, root, "session-1", realDir, "codex-tui", launchedAfter.Add(time.Second))

	id, gotPath, ambiguous, reason := resolveInteractiveSessionIDAt(root, linkDir, launchedAfter)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveLegacyInteractiveSessionIDAtFindsSingleRolloutInCreatedWindow(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	now := createdAt.Add(10 * time.Minute)

	writeSessionMetaRollout(t, root, "too-old", workDir, "codex-tui", createdAt.Add(-6*time.Minute))
	writeSessionMetaRollout(t, root, "future", workDir, "codex-tui", now.Add(time.Second))
	writeSessionMetaRollout(t, root, "next-chat", workDir, "codex-tui", createdAt.Add(3*time.Minute))
	writeSessionMetaRollout(t, root, "exec-session", workDir, "codex_exec", createdAt.Add(time.Minute))
	path := writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", createdAt.Add(time.Minute))

	id, gotPath, ambiguous, reason := resolveLegacyInteractiveSessionIDAt(root, workDir, createdAt, now)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveLegacyInteractiveSessionIDAtUsesSessionMetaTimestamp(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	now := createdAt.Add(10 * time.Minute)

	path := writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", createdAt.Add(time.Minute))
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes rollout %s: %v", path, err)
	}

	id, gotPath, ambiguous, reason := resolveLegacyInteractiveSessionIDAt(root, workDir, createdAt, now)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveLegacyInteractiveSessionIDAtReturnsAmbiguousForMultipleRollouts(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	now := createdAt.Add(10 * time.Minute)

	writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", createdAt.Add(time.Minute))
	writeSessionMetaRollout(t, root, "session-2", workDir, "codex-tui", createdAt.Add(2*time.Minute))

	id, path, ambiguous, reason := resolveLegacyInteractiveSessionIDAt(root, workDir, createdAt, now)

	if id != "" || path != "" {
		t.Errorf("got id/path = %q/%q, want empty on ambiguity", id, path)
	}
	if !ambiguous {
		t.Error("ambiguous = false, want true")
	}
	if reason == "" {
		t.Error("reason empty, want ambiguity reason")
	}
}

// TestReadTranscriptAt covers the three key branches of readTranscriptAt:
// happy-path multi-turn parse, MaxMessages tail-cut, and missing-file
// (Exists=false, nil error).
func TestReadTranscriptAt(t *testing.T) {
	t.Run("parses ordered messages from rollout", func(t *testing.T) {
		root := t.TempDir()
		uuid := "read-transcript-uuid"
		dst := shardedRolloutPath(root, uuid)
		copyFixture(t, "testdata/transcripts/chat.jsonl", dst)

		resp, err := readTranscriptAt(root, "", uuid, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.Exists {
			t.Error("expected Exists=true")
		}
		// chat.jsonl has 4 chat messages: user, assistant, user, assistant.
		// token_count and task_complete envelopes must be skipped.
		if len(resp.Messages) != 4 {
			t.Errorf("len(Messages) = %d, want 4", len(resp.Messages))
		}
		wantRoles := []string{"user", "assistant", "user", "assistant"}
		wantTexts := []string{"hello codex", "hi there, how can I help?", "what is 2+2?", "4"}
		for i, msg := range resp.Messages {
			if msg.Role != wantRoles[i] {
				t.Errorf("Messages[%d].Role = %q, want %q", i, msg.Role, wantRoles[i])
			}
			if msg.Text != wantTexts[i] {
				t.Errorf("Messages[%d].Text = %q, want %q", i, msg.Text, wantTexts[i])
			}
			if msg.Kind != "text" {
				t.Errorf("Messages[%d].Kind = %q, want %q", i, msg.Kind, "text")
			}
			if msg.Timestamp == "" {
				t.Errorf("Messages[%d].Timestamp is empty", i)
			}
		}
		if resp.FinalAssistantText != "4" {
			t.Errorf("FinalAssistantText = %q, want %q", resp.FinalAssistantText, "4")
		}
	})

	t.Run("MaxMessages limits to most recent N", func(t *testing.T) {
		root := t.TempDir()
		uuid := "read-transcript-max-uuid"
		dst := shardedRolloutPath(root, uuid)
		copyFixture(t, "testdata/transcripts/chat.jsonl", dst)

		resp, err := readTranscriptAt(root, "", uuid, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.Exists {
			t.Error("expected Exists=true")
		}
		if len(resp.Messages) != 2 {
			t.Errorf("len(Messages) = %d, want 2 (tail-cut to MaxMessages=2)", len(resp.Messages))
		}
		// The tail-2 should be: user "what is 2+2?" + assistant "4".
		if len(resp.Messages) >= 2 {
			if resp.Messages[0].Text != "what is 2+2?" {
				t.Errorf("Messages[0].Text = %q, want %q", resp.Messages[0].Text, "what is 2+2?")
			}
			if resp.Messages[1].Text != "4" {
				t.Errorf("Messages[1].Text = %q, want %q", resp.Messages[1].Text, "4")
			}
		}
		// FinalAssistantText must reflect the true last assistant turn (not just the tail).
		if resp.FinalAssistantText != "4" {
			t.Errorf("FinalAssistantText = %q, want %q", resp.FinalAssistantText, "4")
		}
	})

	t.Run("no rollout file returns Exists=false nil error", func(t *testing.T) {
		root := t.TempDir()
		resp, err := readTranscriptAt(root, "", "no-such-uuid", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Exists {
			t.Error("expected Exists=false for missing rollout")
		}
		if resp.Messages != nil {
			t.Error("expected nil Messages for missing rollout")
		}
		if resp.FinalAssistantText != "" {
			t.Errorf("expected empty FinalAssistantText for missing rollout, got %q", resp.FinalAssistantText)
		}
	})
}

// TestCodexSessionsRootHonorsCodexHome is the core CODEX_HOME regression: the
// PUBLIC transcriptPath wrapper (not just the *At seam the rest of this file
// drives) must resolve rollouts under CODEX_HOME/sessions when CODEX_HOME is
// set, so a per-account codex home actually gets its own transcripts read.
func TestCodexSessionsRootHonorsCodexHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)

	uuid := "codex-home-uuid"
	dst := shardedRolloutPath(filepath.Join(tmp, "sessions"), uuid)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

	got, err := transcriptPath("", uuid)
	if err != nil {
		t.Fatalf("transcriptPath: %v", err)
	}
	if got != dst {
		t.Errorf("transcriptPath = %q, want %q", got, dst)
	}
}

// TestCodexSessionsRootUnsetIsByteIdenticalToToday pins the "CODEX_HOME
// unset" behavior of codexSessionsRoot: it must equal ~/.codex/sessions,
// exactly matching the pre-fix hardcoded resolution (home + codexSessionsDir).
func TestCodexSessionsRootUnsetIsByteIdenticalToToday(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")

	got, err := codexSessionsRoot()
	if err != nil {
		t.Fatalf("codexSessionsRoot: %v", err)
	}
	want := filepath.Join(tmpHome, ".codex", "sessions")
	if got != want {
		t.Errorf("codexSessionsRoot = %q, want %q", got, want)
	}
}

// TestTranscriptPathSharedSessionsResumeReachability locks the BOS-158
// cross-account-resume constraint at the daemon boundary: a per-account
// CODEX_HOME is only resume-reachable when its sessions/ directory is
// seeded (symlinked/shared) from a base home that actually holds the
// rollout. Seeding sessions/ is BOS-162's credmaterialize executor's job,
// NOT this RPC's/helper's — this test only asserts that transcriptPath
// resolves correctly THROUGH a properly-seeded symlink, and fails fast
// (not-found) when a per-account home's sessions/ is empty/unseeded. See
// docs/solutions/account-rotation/spike-cross-account-resume-credential-isolation.md.
func TestTranscriptPathSharedSessionsResumeReachability(t *testing.T) {
	uuid := "shared-sessions-uuid"

	t.Run("properly-seeded per-account home resolves through the symlink", func(t *testing.T) {
		base := t.TempDir()
		dst := shardedRolloutPath(filepath.Join(base, "sessions"), uuid)
		copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

		acct := t.TempDir()
		if err := os.Symlink(filepath.Join(base, "sessions"), filepath.Join(acct, "sessions")); err != nil {
			t.Fatalf("symlink sessions/: %v", err)
		}
		t.Setenv("CODEX_HOME", acct)

		got, err := transcriptPath("", uuid)
		if err != nil {
			t.Fatalf("transcriptPath through symlinked sessions/: %v", err)
		}
		wantSuffix := filepath.Join("sessions", "2026", "05", "08")
		if !strings.Contains(got, wantSuffix) {
			t.Errorf("transcriptPath = %q, want it to resolve through the shared sessions/ shard", got)
		}
	})

	t.Run("unseeded per-account home fails fast instead of silently resuming nothing", func(t *testing.T) {
		acct2 := t.TempDir()
		sessionsDir := filepath.Join(acct2, "sessions")
		if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
			t.Fatalf("mkdir empty sessions/: %v", err)
		}
		t.Setenv("CODEX_HOME", acct2)

		if _, err := transcriptPath("", uuid); err == nil {
			t.Fatal("expected an error for an unseeded per-account sessions/ dir, got nil")
		}
	})
}
