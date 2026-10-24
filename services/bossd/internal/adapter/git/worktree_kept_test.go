package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func keptCreateFixture(t *testing.T) (*Manager, CreateOpts, string, PreparedWorktreeMarker) {
	t.Helper()
	repo := initTestRepo(t)
	markerGit(t, repo, "config", "commit.gpgsign", "false")
	opts := CreateOpts{RepoPath: repo, RepoID: "repo", BaseBranch: "main", RepoName: "repo", WorktreeBaseDir: t.TempDir(), BranchName: "prepared", Title: "prepared"}
	path := filepath.Join(opts.WorktreeBaseDir, opts.RepoName, "prepared")
	markerGit(t, repo, "worktree", "add", "-b", "prepared", path)
	now := time.Now().UTC()
	marker := PreparedWorktreeMarker{Version: 1, SessionID: "old", RepoID: "repo", Branch: "prepared", IsBranchCreated: true, HeadSHA: markerGit(t, path, "rev-parse", "HEAD"), BaseBranch: "main", KeptAt: now, KeptUntil: now.Add(time.Hour), Reason: "load"}
	if err := WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
		t.Fatal(err)
	}
	return NewManager(zerolog.Nop()), opts, path, marker
}

func TestCreateReuseKept(t *testing.T) {
	for _, derived := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "title"}[derived], func(t *testing.T) {
			mgr, opts, path, _ := keptCreateFixture(t)
			if derived {
				opts.BranchName = ""
			}
			script := "touch setup-ran"
			opts.SetupScript = &script
			added, ready := false, false
			opts.OnWorktreeAdded = func(got string) error { added = got == path; return nil }
			opts.OnWorktreeReady = func(_ context.Context, branch, got string, _ bool) { ready = got == path && branch == "prepared" }
			result, err := mgr.Create(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Reused || result.WorktreePath != path || result.BranchName != "prepared" || result.SetupScriptDuration != 0 || result.WorktreeAddDuration != 0 || !added || !ready || !result.IsBranchCreated {
				t.Fatalf("result=%+v hooks=%v/%v", result, added, ready)
			}
			if _, err := os.Stat(filepath.Join(path, "setup-ran")); !os.IsNotExist(err) {
				t.Fatalf("setup ran: %v", err)
			}
			if _, err := ReadPreparedWorktreeMarker(context.Background(), path); err != nil {
				t.Fatalf("marker consumed before start: %v", err)
			}
		})
	}
}

func TestCreateKeptRefusal(t *testing.T) {
	for _, name := range []string{"expired", "moved", "dirty", "branch", "repo", "existing", "forced", "claimed"} {
		t.Run(name, func(t *testing.T) {
			mgr, opts, path, marker := keptCreateFixture(t)
			switch name {
			case "expired":
				marker.KeptAt = time.Now().Add(-2 * time.Hour)
				marker.KeptUntil = time.Now().Add(-time.Hour)
			case "moved":
				markerGit(t, path, "commit", "--allow-empty", "-m", "moved")
			case "dirty":
				if err := os.WriteFile(filepath.Join(path, "untracked"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "branch":
				marker.Branch = "other"
			case "repo":
				marker.RepoID = "other"
			case "existing":
				opts.ExistingBranch = true
			case "forced":
				opts.Force = true
			case "claimed":
				marker.Claimed = true
			}
			if err := WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
				t.Fatal(err)
			}
			result, err := mgr.Create(context.Background(), opts)
			if opts.Force {
				if err != nil || result.Reused {
					t.Fatalf("force: %+v %v", result, err)
				}
			} else if !errors.Is(err, ErrBranchExists) {
				t.Fatalf("expected existing branch, got %+v %v", result, err)
			}
		})
	}
}

func TestCreateKeptConcurrent(t *testing.T) {
	mgr, opts, _, _ := keptCreateFixture(t)
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		wg.Go(func() {
			result, err := mgr.Create(context.Background(), opts)
			if err != nil && !errors.Is(err, ErrBranchExists) {
				t.Errorf("create: %v", err)
			}
			results <- err == nil && result.Reused
		})
	}
	wg.Wait()
	close(results)
	adopted := 0
	for ok := range results {
		if ok {
			adopted++
		}
	}
	if adopted != 1 {
		t.Fatalf("adopted=%d", adopted)
	}
}

func TestCreateReuseKeptMovedBaseAndIgnoredFiles(t *testing.T) {
	mgr, opts, path, marker := keptCreateFixture(t)
	markerGit(t, opts.RepoPath, "commit", "--allow-empty", "-m", "base advanced")
	markerGit(t, opts.RepoPath, "push", "origin", "main")
	exclude := markerGit(t, path, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nignored-setup-output\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "ignored-setup-output"), []byte("prepared"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := mgr.Create(context.Background(), opts)
	if err != nil || !result.Reused {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if head := markerGit(t, path, "rev-parse", "HEAD"); head != marker.HeadSHA {
		t.Fatalf("adoption changed HEAD: %s", head)
	}
	// A further transient failure refreshes the marker, making it available again.
	marker.KeptUntil = time.Now().Add(24 * time.Hour)
	if err := WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
		t.Fatal(err)
	}
	result, err = mgr.Create(context.Background(), opts)
	if err != nil || !result.Reused {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
}

func TestCreateKeptRefusalDerivedSuffix(t *testing.T) {
	mgr, opts, path, marker := keptCreateFixture(t)
	marker.KeptAt = time.Now().Add(-2 * time.Hour)
	marker.KeptUntil = time.Now().Add(-time.Hour)
	if err := WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
		t.Fatal(err)
	}
	opts.BranchName = ""
	script := "touch setup-ran"
	opts.SetupScript = &script
	result, err := mgr.Create(context.Background(), opts)
	if err != nil || result.Reused || result.BranchName != "prepared-2" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(result.WorktreePath, "setup-ran")); err != nil {
		t.Fatalf("fresh setup missing: %v", err)
	}
}

func TestCreateKeptRecordsOriginalOwnership(t *testing.T) {
	mgr, opts, path, marker := keptCreateFixture(t)
	marker.IsBranchCreated = false
	if err := WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
		t.Fatal(err)
	}
	// Inspect the durable session state written by the real callback in the
	// lifecycle tests; here the callback must see the original branch ownership.
	seen := true
	opts.OnWorktreeReady = func(_ context.Context, _ string, _ string, owned bool) { seen = owned }
	result, err := mgr.Create(context.Background(), opts)
	if err != nil || !result.Reused || seen {
		t.Fatalf("result=%+v callback ownership=%v err=%v", result, seen, err)
	}
}
