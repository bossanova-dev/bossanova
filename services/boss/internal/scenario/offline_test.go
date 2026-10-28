package scenario

import (
	"os"
	"testing"
)

func TestPrepareOfflinePreservesCompiledInputs(t *testing.T) {
	doc := Document{Version: 1, Name: "offline", Repos: []Repo{{Key: "repo", DisplayName: "Offline repo", MergeStrategy: "squash"}}, Sessions: []Session{{Key: "session", Repo: "repo", Title: "Offline session", State: "BLOCKED", BlockedReason: "checks failed", PR: &PullRequest{Number: 1, State: "OPEN", Checks: "FAILED", Mergeable: true}}}}
	compiled, problems := Compile(doc)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	state := applyState{
		Keys:  Keys{Sessions: map[string]string{}, Repos: map[string]string{}},
		Paths: map[string]string{}, Origins: map[string]string{}, Branches: map[string]string{},
		Spec: compiled.Spec,
	}
	home := t.TempDir()
	if err := prepareOffline(doc, ApplyOptions{Home: home}, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Spec.Repos) != 1 || len(state.Spec.Sessions) != 1 {
		t.Fatalf("missing seed inputs: %+v", state.Spec)
	}
	repo, session := state.Spec.Repos[0], state.Spec.Sessions[0]
	if repo.Key != "repo" || repo.DisplayName != "Offline repo" || repo.MergeStrategy != "squash" {
		t.Fatalf("repo inputs changed: %+v", repo)
	}
	expected := compiled.Spec.Sessions[0]
	if session.RepoID != expected.RepoID || session.Title != expected.Title || session.State != expected.State || session.BlockedReason != expected.BlockedReason || session.PRNumber == nil || *session.PRNumber != *expected.PRNumber || len(session.CheckSnapshots) != 1 || session.CheckSnapshots[0] != expected.CheckSnapshots[0] {
		t.Fatalf("compiled session inputs changed: %+v", session)
	}
	for _, path := range []string{repo.LocalPath, session.WorktreePath} {
		if !inside(home, path) {
			t.Fatalf("outside-run directory: %q", path)
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("missing directory %q: %v", path, err)
		}
	}
	if state.Paths["repo"] != repo.LocalPath || repo.OriginURL != "https://github.com/"+state.Origins["repo"]+".git" {
		t.Fatalf("repo substitution missing: %+v", state)
	}
	if session.BranchName == "" || session.BranchName != state.Branches["session"] {
		t.Fatalf("session branch missing: %+v", session)
	}
	if len(state.Keys.Repos) != 0 || len(state.Keys.Sessions) != 0 {
		t.Fatal("preparation fabricated store identities")
	}
}
