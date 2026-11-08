package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWorktrees(t *testing.T) {
	entries := parseWorktrees("worktree /repo main\x00HEAD abc\x00branch refs/heads/main\x00\x00worktree /repo/topic\nname\x00HEAD def\x00detached\x00locked in use\x00\x00")
	if len(entries) != 2 || entries[0].Path != "/repo main" || entries[0].Locked || entries[1].Path != "/repo/topic\nname" || !entries[1].Locked || !entries[1].Detached {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestUnrecordedWorktreeReftableRecoveryRoots(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "init", "--ref-format=reftable", "-b", "main", repo)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "unknown option") {
			t.Skip("installed Git does not support reftable repositories")
		}
		t.Fatalf("init reftable repository: %s %v", out, err)
	}
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Test")
	mustGit(t, repo, "commit", "--allow-empty", "-m", "base")
	base := mustGit(t, repo, "rev-parse", "HEAD")
	path := filepath.Join(t.TempDir(), "candidate")
	mustGit(t, repo, "worktree", "add", "--detach", path, base)
	mustGit(t, path, "commit", "--allow-empty", "-m", "abandoned")
	abandoned := mustGit(t, path, "rev-parse", "HEAD")
	mustGit(t, path, "checkout", "--detach", base)
	private := mustGit(t, path, "rev-parse", "--absolute-git-dir")
	if _, err := os.Stat(filepath.Join(private, "logs", "HEAD")); !os.IsNotExist(err) {
		t.Fatalf("fixture must have no files-backend HEAD log: %v", err)
	}
	if log := mustGit(t, path, "reflog", "show", "--format=%H", "HEAD"); !strings.Contains(log, abandoned) {
		t.Fatal("fixture lost its private recovery root")
	}
	hold, err := UnrecordedWorktreeHold(context.Background(), WorktreeEntry{Path: path, Detached: true}, time.Now().Add(48*time.Hour))
	if err == nil && hold == "" {
		t.Fatal("reftable recovery roots must prevent removal")
	}
}

func TestUnrecordedWorktreePrivateRecoveryRoots(t *testing.T) {
	for _, name := range []string{"shared", "private", "old value", "no log", "invalid log"} {
		t.Run(name, func(t *testing.T) {
			repo := initTestRepo(t)
			base := mustGit(t, repo, "rev-parse", "HEAD")
			path := filepath.Join(t.TempDir(), "candidate")
			mustGit(t, repo, "worktree", "add", "--detach", path, base)
			private := mustGit(t, path, "rev-parse", "--absolute-git-dir")
			logPath := filepath.Join(private, "logs", "HEAD")
			if name == "shared" {
				mustGit(t, repo, "checkout", "-b", "other")
				mustGit(t, repo, "commit", "--allow-empty", "-m", "abandoned shared commit")
				mustGit(t, repo, "reset", "--hard", base)
			} else {
				mustGit(t, path, "commit", "--allow-empty", "-m", "abandoned private commit")
				mustGit(t, path, "checkout", "--detach", base)
			}
			switch name {
			case "old value":
				data, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
				if err := os.WriteFile(logPath, []byte(lines[len(lines)-1]+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "no log":
				if err := os.Remove(logPath); err != nil {
					t.Fatal(err)
				}
			case "invalid log":
				if err := os.WriteFile(logPath, []byte("unreadable recovery roots\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			hold, err := UnrecordedWorktreeHold(context.Background(), WorktreeEntry{Path: path, Detached: true}, time.Now().Add(48*time.Hour))
			if name == "invalid log" {
				if err == nil {
					t.Fatal("malformed recovery inventory must fail closed")
				}
				return
			}
			want := ""
			if name == "private" || name == "old value" {
				want = "unreachable-commits"
			}
			if err != nil || hold != want {
				t.Fatalf("hold=%q err=%v, want %q", hold, err, want)
			}
		})
	}
}

func TestRemoveWorktreeResidueContainment(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".claude", "settings.local.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base, outside, link} {
		if removed, err := RemoveWorktreeResidue(base, path); err == nil || removed {
			t.Fatalf("unsafe remove %s: %v %v", path, removed, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, ".claude", "settings.local.json")); err != nil {
		t.Fatal(err)
	}
}
