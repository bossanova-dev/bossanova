package git

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func commitFile(t *testing.T, repo, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", name)
	gitOutput(t, repo, "commit", "-m", name)
	return gitOutput(t, repo, "rev-parse", "HEAD")
}

// A rebased copy of a parent's commits has new SHAs and the same patch ids.
// A head only origin has is fetched by SHA before it is read; the unknown
// head fails the batched fetch, so the remote-only head arrives one at a time.
func TestPatchIDs(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	origin := gitOutput(t, repo, "remote", "get-url", "origin")
	gitOutput(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	gitOutput(t, repo, "checkout", "-b", "parent")
	commitFile(t, repo, "a.txt", "a")
	parent := commitFile(t, repo, "b.txt", "b")
	gitOutput(t, repo, "checkout", "-b", "rebased", "main")
	commitFile(t, repo, "base.txt", "unrelated")
	gitOutput(t, repo, "cherry-pick", "main..parent")
	rebased := commitFile(t, repo, "c.txt", "c")

	gitOutput(t, repo, "checkout", "main")
	mainTip := commitFile(t, repo, "main.txt", "main")
	gitOutput(t, repo, "push", "origin", "main")

	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	gitOutput(t, repo, "clone", "-q", origin, elsewhere)
	for _, kv := range [][2]string{{"user.email", "test@test.com"}, {"user.name", "Test"}, {"commit.gpgsign", "false"}} {
		gitOutput(t, elsewhere, "config", kv[0], kv[1])
	}
	gitOutput(t, elsewhere, "checkout", "-b", "remote-only")
	remoteOnly := commitFile(t, elsewhere, "d.txt", "d")
	gitOutput(t, elsewhere, "push", "origin", "remote-only")

	tip, err := mgr.FetchBaseTip(t.Context(), repo, "main")
	if err != nil || tip != mainTip {
		t.Fatalf("FetchBaseTip = %q, %v; want %s", tip, err, mainTip)
	}
	missing := strings.Repeat("f", 40)
	ids, err := mgr.PatchIDs(t.Context(), repo, tip, []string{parent, rebased, remoteOnly, missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("PatchIDs error = %v; want one naming the unreadable head %s", err, missing)
	}
	if _, ok := ids[missing]; ok {
		t.Fatalf("unreadable head %s has patch ids %v", missing, ids[missing])
	}
	if len(ids[parent]) != 2 || len(ids[rebased]) != 4 || len(ids[remoteOnly]) != 1 {
		t.Fatalf("patch ids = %v; want 2 for the parent, 4 for the rebased child, 1 for the remote-only head", ids)
	}
	for _, id := range ids[parent] {
		if !slices.Contains(ids[rebased], id) {
			t.Fatalf("rebased child %v lacks the parent's patch id %s", ids[rebased], id)
		}
	}
}

// Once the default branch takes a parent's commits under new SHAs (a
// rebase-merge), a branch still carrying the old copies counts only its own
// commit, so it is stacked on nothing that merged.
func TestPatchIDsDropCommitsTheDefaultBranchCarries(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	gitOutput(t, repo, "checkout", "-b", "parent")
	commitFile(t, repo, "a.txt", "a")
	commitFile(t, repo, "b.txt", "b")
	gitOutput(t, repo, "checkout", "-b", "child")
	child := commitFile(t, repo, "c.txt", "c")

	gitOutput(t, repo, "checkout", "main")
	commitFile(t, repo, "main.txt", "main")
	gitOutput(t, repo, "cherry-pick", "main~1..parent")
	gitOutput(t, repo, "push", "origin", "main")

	tip, err := mgr.FetchBaseTip(t.Context(), repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := mgr.PatchIDs(t.Context(), repo, tip, []string{child})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids[child]) != 1 {
		t.Fatalf("child patch ids = %v; want only its own commit once main carries the parent's under new SHAs", ids[child])
	}
}
