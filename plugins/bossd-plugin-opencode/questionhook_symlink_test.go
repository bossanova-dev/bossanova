package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuestionHookRefusesSymlinks(t *testing.T) {
	for _, component := range []string{"root", "plugins", "leaf"} {
		for _, inside := range []bool{false, true} {
			name := component + "/outside"
			if inside {
				name = component + "/inside"
			}
			t.Run(name, func(t *testing.T) {
				workDir, targetDir := t.TempDir(), t.TempDir()
				if inside {
					targetDir = filepath.Join(workDir, "target")
				}
				if err := os.MkdirAll(filepath.Join(targetDir, "plugins"), 0o750); err != nil {
					t.Fatal(err)
				}
				targetFile := filepath.Join(targetDir, questionHookFileName)
				if component == "root" {
					targetFile = filepath.Join(targetDir, "plugins", questionHookFileName)
				}
				const original = "user content"
				if err := os.WriteFile(targetFile, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(workDir, questionHookRootDir)
				target := targetDir
				if component != "root" {
					link = filepath.Join(link, questionHookPluginsDir)
				}
				if component == "leaf" {
					link = filepath.Join(link, questionHookFileName)
					target = targetFile
				}
				if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}

				installed, err := installQuestionHook(workDir, hookEnv())
				if err == nil || installed {
					t.Errorf("symlink install = (%v, %v), want refusal", installed, err)
				}
				assertQuestionHookTarget(t, targetFile, original)
				// Cleanup also runs after installation fails, so exercise that exact sequence.
				_ = removeQuestionHook(workDir)
				assertQuestionHookTarget(t, targetFile, original)
				info, err := os.Lstat(link)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("symlink removed: %v", err)
				}
			})
		}
	}
}

func assertQuestionHookTarget(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Fatalf("target bytes = %q, %v; want %q", got, err, want)
	}
}

func TestInstallQuestionHookSymlinkCreatesNothingOutside(t *testing.T) {
	for _, component := range []string{questionHookRootDir, filepath.Join(questionHookRootDir, questionHookPluginsDir)} {
		t.Run(component, func(t *testing.T) {
			workDir, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(workDir, component)
			if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			installed, err := installQuestionHook(workDir, hookEnv())
			if installed || err == nil {
				t.Errorf("symlink install = (%v, %v), want refusal", installed, err)
			}
			_ = removeQuestionHook(workDir)
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside directory changed: %v, %v", entries, err)
			}
		})
	}
}
