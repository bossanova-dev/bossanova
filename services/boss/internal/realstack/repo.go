package realstack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// InitRepo creates a committed main branch and local bare origin without network access.
func InitRepo(t testing.TB, home, name string) string {
	t.Helper()
	path, err := CreateRepo(home, name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// CreateRepo creates a checkout and local bare origin owned by the caller.
func CreateRepo(home, name string) (string, error) {
	root, err := os.MkdirTemp(home, "repo-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, name)
	for _, command := range []struct {
		dir  string
		args []string
	}{
		{root, []string{"init", "--bare", "origin.git"}}, {root, []string{"init", "-b", "main", path}},
		{path, []string{"config", "commit.gpgsign", "false"}}, {path, []string{"config", "user.name", "Boss E2E"}}, {path, []string{"config", "user.email", "e2e@example.invalid"}},
	} {
		if err = RunGit(command.dir, command.args...); err != nil {
			return "", err
		}
	}
	if err = os.WriteFile(filepath.Join(path, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "fixture"}, {"remote", "add", "origin", filepath.Join(root, "origin.git")}, {"push", "-u", "origin", "main"}} {
		if err = RunGit(path, args...); err != nil {
			return "", err
		}
	}
	return path, nil
}

// Git runs a bounded git command in an isolated fixture directory.
func Git(t testing.TB, dir string, args ...string) {
	t.Helper()
	if err := RunGit(dir, args...); err != nil {
		t.Fatal(err)
	}
}

// RunGit runs a bounded git command without ambient user configuration.
func RunGit(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(BaseEnv(os.Environ()), "HOME="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}
