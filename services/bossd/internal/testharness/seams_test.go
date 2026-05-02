package testharness_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/testharness"
)

// --- PostStopHook --------------------------------------------------------

// TestPostStopHook_ValidToken posts to the hook server with a valid bearer
// token and expects 200.
func TestPostStopHook_ValidToken(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := registerTestRepo(t, h, ctx)
	sessID := h.SeedCronSession(t, repoID, "test-secret-token")

	resp, err := h.PostStopHook(sessID, "test-secret-token")
	if err != nil {
		t.Fatalf("PostStopHook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestPostStopHook_WrongToken posts with the wrong bearer token and expects 401.
func TestPostStopHook_WrongToken(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := registerTestRepo(t, h, ctx)
	sessID := h.SeedCronSession(t, repoID, "right-token")

	resp, err := h.PostStopHook(sessID, "wrong-token")
	if err != nil {
		t.Fatalf("PostStopHook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// --- VCS modes -----------------------------------------------------------

// TestMockVCS_NoGitHub verifies that VCSModeNoGitHub causes CreateDraftPR
// to return ErrNoGitHub.
func TestMockVCS_NoGitHub(t *testing.T) {
	h := testharness.New(t)
	h.SetVCSMode(testharness.VCSModeNoGitHub)

	ctx := context.Background()
	_, err := h.VCS.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath: "/tmp/some-repo", HeadBranch: "feat", BaseBranch: "main", Title: "t",
	})
	if !errors.Is(err, testharness.ErrNoGitHub) {
		t.Errorf("CreateDraftPR err = %v, want ErrNoGitHub", err)
	}
}

// TestMockVCS_CreatePRFail verifies that VCSModeCreatePRFail causes
// CreateDraftPR to return a non-nil error (any error, not ErrNoGitHub).
func TestMockVCS_CreatePRFail(t *testing.T) {
	h := testharness.New(t)
	h.SetVCSMode(testharness.VCSModeCreatePRFail)

	ctx := context.Background()
	_, err := h.VCS.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath: "/tmp/some-repo", HeadBranch: "feat", BaseBranch: "main", Title: "t",
	})
	if err == nil {
		t.Error("CreateDraftPR: want error for VCSModeCreatePRFail, got nil")
	}
	if errors.Is(err, testharness.ErrNoGitHub) {
		t.Errorf("CreatePRFail should not return ErrNoGitHub; got %v", err)
	}
}

// TestMockVCS_PushFail verifies that VCSModePushFail causes the git mock
// Push to return an error.
func TestMockVCS_PushFail(t *testing.T) {
	h := testharness.New(t)
	h.SetVCSMode(testharness.VCSModePushFail)

	ctx := context.Background()
	err := h.Git.Push(ctx, "/tmp/some-worktree", "my-branch")
	if err == nil {
		t.Error("Push: want error for VCSModePushFail, got nil")
	}
}

// TestMockVCS_Success verifies the default mode allows CreateDraftPR.
func TestMockVCS_Success(t *testing.T) {
	h := testharness.New(t)
	// VCSModeSuccess is the default — no SetMode call needed.

	ctx := context.Background()
	info, err := h.VCS.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath: "/tmp/some-repo", HeadBranch: "feat", BaseBranch: "main", Title: "t",
	})
	if err != nil {
		t.Fatalf("CreateDraftPR: %v", err)
	}
	if info == nil || info.Number == 0 {
		t.Error("CreateDraftPR returned nil/zero PR info on success mode")
	}
}

// --- Mock claude runner --------------------------------------------------

// TestMockClaude_WithChanges verifies that WithChanges writes a file to
// workDir before the runner "exits".
func TestMockClaude_WithChanges(t *testing.T) {
	h := testharness.New(t)
	workDir := t.TempDir()

	h.Claude.WithChanges("result.txt", "hello from claude")

	ctx := context.Background()
	id, err := h.Claude.Start(ctx, workDir, "do the thing", nil, "test-session-id")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatal("Start returned empty session ID")
	}

	got, err := os.ReadFile(filepath.Join(workDir, "result.txt"))
	if err != nil {
		t.Fatalf("read result.txt: %v", err)
	}
	if string(got) != "hello from claude" {
		t.Errorf("result.txt = %q, want %q", got, "hello from claude")
	}

	// The session should not be running (exited cleanly).
	if h.Claude.IsRunning(id) {
		t.Error("session should not be running after WithChanges start")
	}

	// Exactly one start should have been recorded (double-append regression guard).
	if got := len(h.Claude.Started); got != 1 {
		t.Errorf("WithChanges: len(Started) = %d, want 1 (double-append?)", got)
	}
}

// TestMockClaude_NoChanges verifies that NoChanges exits cleanly without
// writing any file.
func TestMockClaude_NoChanges(t *testing.T) {
	h := testharness.New(t)
	workDir := t.TempDir()

	h.Claude.NoChanges()

	ctx := context.Background()
	id, err := h.Claude.Start(ctx, workDir, "do nothing", nil, "test-session-id-2")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatal("Start returned empty session ID")
	}

	// No files should have been created in workDir.
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("NoChanges: unexpected files in workDir: %v", names)
	}

	if h.Claude.IsRunning(id) {
		t.Error("session should not be running after NoChanges start")
	}

	// Exactly one start should have been recorded (double-append regression guard).
	if got := len(h.Claude.Started); got != 1 {
		t.Errorf("NoChanges: len(Started) = %d, want 1 (double-append?)", got)
	}
}
