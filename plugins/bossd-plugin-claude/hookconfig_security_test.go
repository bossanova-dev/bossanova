package main

import (
	"os"
	"path/filepath"
	"testing"
)

func hookConfigWriters() map[string]func(string) error {
	return map[string]func(string) error{
		"stop":   func(wt string) error { return WriteHookConfig(wt, "session", "agent", "token", 4242) },
		"remove": func(wt string) error { return RemoveRunHookConfig(wt, "agent") },
		"prompt": func(wt string) error { return WritePromptSubmitHookConfig(wt, "agent", 4242, "token") },
	}
}

func TestHookConfigRefusesSymlinkedClaudeDir(t *testing.T) {
	for name, write := range hookConfigWriters() {
		t.Run(name, func(t *testing.T) {
			worktree, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(outside, filepath.Join(worktree, ".claude")); err != nil {
				t.Fatal(err)
			}
			if err := write(worktree); err == nil {
				t.Error("hook writer accepted symlinked .claude directory")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside directory changed: entries=%v, error=%v", entries, err)
			}
		})
	}
}

func TestHookConfigRefusesSymlinkedSettingsFile(t *testing.T) {
	for name, write := range hookConfigWriters() {
		t.Run(name, func(t *testing.T) {
			worktree, outside := t.TempDir(), t.TempDir()
			claudeDir := filepath.Join(worktree, ".claude")
			if err := os.Mkdir(claudeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(outside, "credentials.json")
			const original = `{"private":"outside-only","hooks":{"Stop":[{"matcher":"bossd-agent-run-agent"}]}}`
			if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(claudeDir, "settings.local.json")
			if err := os.Symlink(target, leaf); err != nil {
				t.Fatal(err)
			}
			if err := write(worktree); err == nil {
				t.Error("hook writer accepted symlinked settings file")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != original {
				t.Fatalf("outside file changed: data=%q, error=%v", data, err)
			}
			info, err := os.Lstat(leaf)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("settings link replaced by a copy of outside data: info=%v, error=%v", info, err)
			}
			entries, err := os.ReadDir(claudeDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("unexpected settings copies or temporary files: entries=%v, error=%v", entries, err)
			}
		})
	}
}
