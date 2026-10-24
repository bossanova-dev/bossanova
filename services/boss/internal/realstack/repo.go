package realstack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// InitRepo creates a committed main branch and local bare origin without network access.
func InitRepo(t testing.TB, home, name string) string {
	t.Helper()
	root, err := os.MkdirTemp(home, "repo-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	Git(t, root, "init", "--bare", "origin.git")
	Git(t, root, "init", "-b", "main", path)
	Git(t, path, "config", "commit.gpgsign", "false")
	Git(t, path, "config", "user.name", "Boss E2E")
	Git(t, path, "config", "user.email", "e2e@example.invalid")
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Git(t, path, "add", "README.md")
	Git(t, path, "commit", "-m", "fixture")
	Git(t, path, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	Git(t, path, "push", "-u", "origin", "main")
	return path
}

// Git runs a bounded git command in an isolated fixture directory.
func Git(t testing.TB, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(BaseEnv(os.Environ()), "HOME="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
