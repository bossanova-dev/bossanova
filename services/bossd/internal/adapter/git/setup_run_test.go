package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func createWithSetupScript(t *testing.T, script string, tracked map[string]string) string {
	t.Helper()
	repoDir := initTestRepo(t)
	for name, content := range tracked {
		writeFile(t, repoDir, name, content)
		mustGit(t, repoDir, "add", name)
	}
	if len(tracked) > 0 {
		mustGit(t, repoDir, "-c", "commit.gpgsign=false", "commit", "-m", "tracked files")
		mustGit(t, repoDir, "push", "origin", "main")
	}
	result, err := NewManager(zerolog.Nop()).Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: filepath.Join(t.TempDir(), "worktrees"),
		RepoName:        "my-repo",
		Title:           "Setup residue",
		SetupScript:     &script,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return result.WorktreePath
}

func TestSetupResidueCountsOnlyFilesUntouchedSinceSetup_Sessions_AC236401_AC236402(t *testing.T) {
	wt := createWithSetupScript(t, `printf '{"v":2}\n' > lock.json && rm gone.txt`, map[string]string{
		"lock.json": "{}\n",
		"gone.txt":  "x\n",
		"notes.txt": "a\n",
	})

	if got, want := SetupResidue(wt), []string{"gone.txt", "lock.json"}; !slices.Equal(got, want) {
		t.Fatalf("SetupResidue after setup = %v, want %v", got, want)
	}
	status, err := NewManager(zerolog.Nop()).Status(context.Background(), wt)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if strings.Contains(status, ".boss") {
		t.Fatalf("the residue record shows up in git status:\n%s", status)
	}

	writeFile(t, wt, "lock.json", "{\"v\":3}\n")
	writeFile(t, wt, "notes.txt", "b\n")
	if got, want := SetupResidue(wt), []string{"gone.txt"}; !slices.Equal(got, want) {
		t.Fatalf("SetupResidue after session edits = %v, want %v (an edited file is session work)", got, want)
	}
}

func TestSetupResidueIsEmptyWhenSetupLeavesTrackedFilesAlone(t *testing.T) {
	wt := createWithSetupScript(t, `echo hi > untracked.txt`, map[string]string{"lock.json": "{}\n"})

	if got := SetupResidue(wt); got != nil {
		t.Fatalf("SetupResidue = %v, want nil", got)
	}
	if _, err := os.Stat(filepath.Join(wt, setupResidueFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("residue record exists without residue: %v", err)
	}
}

func TestStatusKeepsTheFirstUnstagedEntryIntact(t *testing.T) {
	wt := createWithSetupScript(t, `true`, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	writeFile(t, wt, "a.txt", "changed\n")
	writeFile(t, wt, "b.txt", "changed\n")

	status, err := NewManager(zerolog.Nop()).Status(context.Background(), wt)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if want := " M a.txt\n M b.txt"; status != want {
		t.Fatalf("Status = %q, want %q", status, want)
	}
}
