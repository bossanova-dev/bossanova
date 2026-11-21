package git

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRemoteBranchExistsReportsPresentAbsentAndUnreachable(t *testing.T) {
	repo := initTestRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, repo, "init", "--bare", origin)
	mustGit(t, repo, "remote", "set-url", "origin", origin)
	mustGit(t, repo, "push", "origin", "HEAD:refs/heads/integration")
	original := runGitRemoteFn
	t.Cleanup(func() { runGitRemoteFn = original })
	attempts := 0
	runGitRemoteFn = func(ctx context.Context, dir string, args ...string) (string, error) {
		attempts++
		return original(ctx, dir, args...)
	}
	for _, tc := range []struct {
		name string
		want bool
	}{{"integration", true}, {"integrtion", false}} {
		t.Run(tc.name, func(t *testing.T) {
			attempts = 0
			got, err := RemoteBranchExists(context.Background(), repo, tc.name)
			if err != nil || got != tc.want {
				t.Fatalf("RemoteBranchExists(%q) = %v, %v; want %v, nil", tc.name, got, err, tc.want)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d; want 1", attempts)
			}
		})
	}
	mustGit(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if got, err := RemoteBranchExists(context.Background(), repo, "integration"); got || err == nil {
		t.Fatalf("unreachable origin = %v, %v; want false, error", got, err)
	}
}

func TestRemoteBranchExistsDoesNotRetryNoMatchWithTransientStderr(t *testing.T) {
	// Git's no-match status must win over a misleading transport warning.
	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 2").Run()
	attempts, _ := scriptedGitRemote(t, fmt.Errorf("%w: %s", err, transientStderr))
	got, probeErr := RemoteBranchExists(context.Background(), t.TempDir(), "missing")
	if got || probeErr != nil {
		t.Fatalf("missing branch = %v, %v; want false, nil", got, probeErr)
	}
	if attempts() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts())
	}
}
