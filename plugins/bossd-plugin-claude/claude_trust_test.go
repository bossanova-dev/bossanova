package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func TestClaudeFolderTrustLaunch_Sessions_AC117(t *testing.T) {
	for _, decision := range []bossanovav1.ProjectTrustDecision{0, 1, 2, 3} {
		t.Run(decision.String(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			work := t.TempDir()
			configDir := t.TempDir()
			configHome := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			paths := []string{filepath.Join(home, ".claude.json"), filepath.Join(configDir, ".claude.json"), filepath.Join(configHome, ".claude.json")}
			raw := `{"large":9007199254740993,"object": { "x" : 2 },"projects":{` + quoteTrustKey(work) + `:{"keep": [ 1, 2 ]}}}`
			for _, p := range paths {
				if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}
			// Untrusted launches verify isolation support; the trust writer is tested
			// directly on those decisions to avoid invoking an installed Claude CLI.
			if decision == bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT {
				_, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: work, ProjectTrust: decision, ConfigHomeEnv: map[string]string{"HOME": configHome}})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				srv.applyClaudeProjectTrust(work, map[string]string{"HOME": configHome}, decision)
			}
			for _, p := range paths {
				got, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if decision != 1 {
					if string(got) != raw {
						t.Fatal("non-grant edited config")
					}
					continue
				}
				var top map[string]json.RawMessage
				if err := json.Unmarshal(got, &top); err != nil {
					t.Fatal(err)
				}
				if string(top["large"]) != "9007199254740993" || string(top["object"]) != `{ "x" : 2 }` {
					t.Fatalf("unrelated bytes changed: %s", got)
				}
				var projects map[string]map[string]json.RawMessage
				if err := json.Unmarshal(top["projects"], &projects); err != nil {
					t.Fatal(err)
				}
				keys, _ := claudeProjectTrustPaths(work)
				for _, key := range keys {
					if string(projects[key]["hasTrustDialogAccepted"]) != "true" {
						t.Fatalf("trust missing for %s", key)
					}
				}
				if string(projects[work]["keep"]) != `[ 1, 2 ]` {
					t.Fatal("project bytes changed")
				}
				before, _ := os.Stat(p)
				srv.applyClaudeProjectTrust(work, map[string]string{"HOME": configHome}, decision)
				after, _ := os.Stat(p)
				if !before.ModTime().Equal(after.ModTime()) {
					t.Fatal("already trusted file rewritten")
				}
			}
		})
	}
}

func quoteTrustKey(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestClaudeTrustMissingAndMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	work := t.TempDir()
	srv := &Server{logger: zerolog.Nop()}
	run := func() {
		t.Helper()
		resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: work, ProjectTrust: 1})
		if err != nil || len(resp.GetArgv()) == 0 {
			t.Fatalf("launch: %v", err)
		}
	}
	run()
	p := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("missing file created")
	}
	for _, raw := range []string{`broken`, `null`, `{"projects":[]}`, `{"projects":{"` + work + `":[]}}`} {
		if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		run()
		got, _ := os.ReadFile(p)
		if string(got) != raw {
			t.Fatal("malformed config edited")
		}
	}
}

func TestClaudeTrustLinkedWorktree_Sessions_AC117(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	repo := t.TempDir()
	work := filepath.Join(t.TempDir(), "linked")
	for _, args := range [][]string{{"init"}, {"-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"}, {"worktree", "add", "-b", "linked", work}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(work, link); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(p, []byte(`{}`), 0640); err != nil {
		t.Fatal(err)
	}
	srv := &Server{logger: zerolog.Nop()}
	srv.applyClaudeProjectTrust(link, nil, 1)
	raw, _ := os.ReadFile(p)
	var top struct {
		Projects map[string]struct {
			Accepted bool `json:"hasTrustDialogAccepted"`
		}
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, repo} {
		resolved, _ := filepath.EvalSymlinks(path)
		if !top.Projects[resolved].Accepted || (path == link && !top.Projects[path].Accepted) {
			t.Fatalf("missing trust for %s", path)
		}
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0640 {
		t.Fatal("mode changed")
	}
}

func TestClaudeFolderTrustBlocksInput_Sessions_AC118(t *testing.T) {
	pane, err := os.ReadFile("testdata/panes/folder_trust.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	resp, err := srv.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: pane})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetBlocksInput() || resp.GetHasPrompt() {
		t.Fatalf("trust screen: blocks=%t prompt=%t", resp.GetBlocksInput(), resp.GetHasPrompt())
	}
}
