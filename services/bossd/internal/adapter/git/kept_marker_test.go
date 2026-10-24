package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func markerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func markerFixture(t *testing.T) (string, string, PreparedWorktreeMarker) {
	t.Helper()
	repo := t.TempDir()
	markerGit(t, repo, "init", "-b", "main")
	markerGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	worktree := filepath.Join(t.TempDir(), "prepared")
	markerGit(t, repo, "worktree", "add", "-b", "prepared", worktree)
	now := time.Now().UTC()
	return repo, worktree, PreparedWorktreeMarker{
		Version: 1, SessionID: "session", RepoID: "repo", Branch: "prepared", IsBranchCreated: true,
		HeadSHA: markerGit(t, worktree, "rev-parse", "HEAD"), BaseBranch: "main",
		KeptAt: now, KeptUntil: now.Add(24 * time.Hour), Reason: "transient agent start failure",
	}
}

func TestPreparedWorktreeMarkerRoundTrip(t *testing.T) {
	repo, worktree, marker := markerFixture(t)
	ctx := context.Background()
	if _, err := ReadPreparedWorktreeMarker(ctx, worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing marker: %v", err)
	}
	if err := WritePreparedWorktreeMarker(ctx, worktree, marker); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPreparedWorktreeMarker(ctx, worktree)
	if err != nil || !reflect.DeepEqual(got, &marker) {
		t.Fatalf("round trip: got %+v, error %v; want %+v", got, err, marker)
	}
	gitDir := markerGit(t, worktree, "rev-parse", "--absolute-git-dir")
	markerPath := filepath.Join(gitDir, "boss-prepared.json")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatal(err)
	}
	if status := markerGit(t, worktree, "status", "--porcelain"); status != "" {
		t.Fatalf("marker dirtied worktree: %s", status)
	}
	marker.Reason = "replacement"
	if err := WritePreparedWorktreeMarker(ctx, worktree, marker); err != nil {
		t.Fatal(err)
	}
	got, err = ReadPreparedWorktreeMarker(ctx, worktree)
	if err != nil || !reflect.DeepEqual(got, &marker) {
		t.Fatalf("replace: got %+v, error %v", got, err)
	}
	markerGit(t, repo, "worktree", "remove", worktree)
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker survived registration removal: %v", err)
	}
}

func TestPreparedWorktreePathsAndHead(t *testing.T) {
	repo, worktree, marker := markerFixture(t)
	other := filepath.Join(t.TempDir(), "space and\nnewline")
	markerGit(t, repo, "worktree", "add", "--detach", other)
	paths, err := ListWorktreePaths(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{repo, worktree, other} {
		// macOS may canonicalize the temp directory's /var symlink.
		want, err = filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, path := range paths {
			found = found || path == want
		}
		if !found {
			t.Errorf("worktree %q missing from %q", want, paths)
		}
	}
	head, err := PreparedWorktreeHead(context.Background(), worktree)
	if err != nil || head != marker.HeadSHA {
		t.Fatalf("HEAD: got %q, error %v; want %q", head, err, marker.HeadSHA)
	}
}

func TestPreparedWorktreeMarkerRejectsInvalid(t *testing.T) {
	repo, worktree, marker := markerFixture(t)
	ctx := context.Background()
	if err := WritePreparedWorktreeMarker(ctx, repo, marker); err == nil {
		t.Fatal("accepted main checkout")
	}
	for _, change := range []func(*PreparedWorktreeMarker){
		func(m *PreparedWorktreeMarker) { m.Version = 2 },
		func(m *PreparedWorktreeMarker) { m.SessionID = "" },
		func(m *PreparedWorktreeMarker) { m.HeadSHA = "invalid" },
		func(m *PreparedWorktreeMarker) { m.KeptUntil = m.KeptAt },
		func(m *PreparedWorktreeMarker) { m.Reason = "line one\nline two" },
	} {
		invalid := marker
		change(&invalid)
		if err := WritePreparedWorktreeMarker(ctx, worktree, invalid); err == nil {
			t.Fatalf("accepted invalid marker: %+v", invalid)
		}
	}
	gitDir := markerGit(t, worktree, "rev-parse", "--absolute-git-dir")
	for _, contents := range []string{"{", `{}`, `{"version":2}`, `null`} {
		if err := os.WriteFile(filepath.Join(gitDir, "boss-prepared.json"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPreparedWorktreeMarker(ctx, worktree); err == nil {
			t.Fatalf("accepted malformed marker: %s", contents)
		}
	}
}
