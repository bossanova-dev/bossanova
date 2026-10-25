package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestJourney_Repos_AC1_Repo_Ls(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(cliRepos()...))
	const script = "echo fixture-setup"
	if configured := h.Run("repo", "update", "repo-1", "--setup-script", script); !configured.Success() {
		t.Fatalf("configure setup: %+v", configured)
	}
	res := h.Run("repo", "ls")
	if !res.Success() {
		t.Fatalf("ls: %+v", res)
	}
	for _, repo := range h.Repos(t) {
		var row string
		for _, line := range strings.Split(res.Stdout, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), repo.Id) {
				row = line
			}
		}
		fields := strings.Fields(row)
		if len(fields) < 5 {
			t.Fatalf("repository row missing columns: %q", row)
		}
		for _, want := range []string{repo.Id, repo.DisplayName, repo.DefaultBaseBranch} {
			if !strings.Contains(row, want) {
				t.Errorf("row missing %q: %s", want, row)
			}
		}
		// Long real checkout paths are elided by the terminal table.
		shownPath := strings.TrimSuffix(fields[2], "…")
		if shownPath == "" || !strings.HasPrefix(repo.LocalPath, shownPath) {
			t.Errorf("listed path %q does not identify %q", fields[2], repo.LocalPath)
		}
		if repo.Id == h.RepoIDs["repo-1"] && !strings.Contains(row, script) {
			t.Errorf("setup script missing from row: %s", row)
		}
	}
}

func TestJourney_Repos_AC2_Repo_Update_AllFlags(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(cliRepos()...))
	for _, tc := range []struct{ flag, value, want string }{
		{"--name", "new-name", "new-name"},
		{"--setup-script", "make deps", "make deps"},
		{"--setup-script", "", "new-name"},
	} {
		res := h.Run("repo", "update", "repo-1", tc.flag, tc.value)
		if res.ExitCode != 0 {
			t.Fatalf("update %s: exit=%d stderr=%q", tc.flag, res.ExitCode, res.Stderr)
		}
		var found bool
		for _, repo := range h.Repos(t) {
			if repo.Id != h.RepoIDs["repo-1"] {
				continue
			}
			found = true
			if tc.flag == "--name" && repo.DisplayName != tc.value {
				t.Fatalf("name = %q, want %q", repo.DisplayName, tc.value)
			}
			if tc.flag == "--setup-script" && repo.GetSetupScript() != tc.value {
				t.Fatalf("setup = %q, want %q", repo.GetSetupScript(), tc.value)
			}
		}
		if !found {
			t.Fatal("updated repository disappeared")
		}
		listed := h.Run("repo", "ls")
		if !listed.Success() || !strings.Contains(listed.Stdout, tc.want) {
			t.Fatalf("updated value missing from listing: %+v", listed)
		}
		if tc.flag == "--setup-script" && tc.value == "" && strings.Contains(listed.Stdout, "make deps") {
			t.Fatal("cleared setup script remains listed")
		}
	}
}
