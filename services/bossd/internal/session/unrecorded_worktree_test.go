package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/rs/zerolog"
)

type reconcilePaths struct {
	paths []string
	err   error
}

func (p reconcilePaths) CurrentPaths(context.Context) ([]string, error) { return p.paths, p.err }

func TestReconcileUnrecordedWorktrees_Sessions_AC222901(t *testing.T) {
	for _, name := range []string{"idle", "archived", "tracked", "untracked", "ignored file", "ignored directory", "unreachable", "reflog only", "locked", "process", "pane", "recent", "prepared", "process error", "pane error", "new row", "new process", "new pane", "new process error", "new pane error", "outside", "checkout"} {
		t.Run(name, func(t *testing.T) {
			repo, path := keptFixture(t)
			repo, _ = filepath.EvalSymlinks(repo)
			path, _ = filepath.EvalSymlinks(path)
			base := filepath.Dir(filepath.Dir(path))
			display := filepath.Base(filepath.Dir(path))
			run := func(dir string, args ...string) {
				t.Helper()
				cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
				cmd.WaitDelay = time.Second
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s %v", args, out, err)
				}
			}
			run(path, "config", "commit.gpgsign", "false")
			run(path, "config", "user.name", "Test")
			run(path, "config", "user.email", "test@example.com")
			run(path, "commit", "--allow-empty", "-m", "reachable")
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo, DisplayName: display, WorktreeBaseDir: base}
			l := &Lifecycle{sessions: sessions, repos: repos, worktrees: gitpkg.NewManager(zerolog.Nop()), logger: zerolog.Nop()}
			process, panes := reconcilePaths{}, reconcilePaths{}
			now := time.Now().Add(48 * time.Hour)
			var protectedFile string
			switch name {
			case "archived":
				sessions.sessions["s"] = &models.Session{ID: "s", WorktreePath: path, ArchivedAt: &now}
			case "tracked":
				run(path, "config", "core.logAllRefUpdates", "true")
				if err := os.WriteFile(filepath.Join(path, "file"), []byte("first"), 0600); err != nil {
					t.Fatal(err)
				}
				run(path, "add", "file")
				run(path, "commit", "-m", "tracked")
				if err := os.WriteFile(filepath.Join(path, "file"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				if err := os.WriteFile(filepath.Join(path, "loose"), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ignored file", "ignored directory":
				if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(".env.local\nlocal-data/\n"), 0600); err != nil {
					t.Fatal(err)
				}
				protectedFile = filepath.Join(path, ".env.local")
				if name == "ignored directory" {
					if err := os.Mkdir(filepath.Join(path, "local-data"), 0700); err != nil {
						t.Fatal(err)
					}
					protectedFile = filepath.Join(path, "local-data", "database")
				}
				if err := os.WriteFile(protectedFile, []byte("local data"), 0600); err != nil {
					t.Fatal(err)
				}
				run(path, "check-ignore", protectedFile)
			case "unreachable", "reflog only":
				run(path, "checkout", "--detach")
				run(path, "commit", "--allow-empty", "-m", "unreachable")
				if name == "reflog only" {
					run(path, "checkout", "topic")
				}
			case "locked":
				run(repo, "worktree", "lock", path)
			case "process":
				process.paths = []string{filepath.Join(path, "subdir")}
			case "pane":
				panes.paths = []string{path}
			case "recent":
				now = time.Now()
			case "prepared":
				writeKeptFixture(t, path, time.Now().Add(-time.Hour))
			case "process error":
				process.err = errors.New("lsof failed")
			case "pane error":
				panes.err = errors.New("tmux failed")
			case "outside":
				repos.repos["r"].WorktreeBaseDir = t.TempDir()
			case "checkout":
				repos.repos["r"].LocalPath = path
			case "new row", "new process", "new pane", "new process error", "new pane error":
				calls := 0
				l.sessions = &keptMarkerRefreshStore{SessionStore: sessions, refresh: func() {
					calls++
					if calls == 2 {
						switch name {
						case "new row":
							sessions.sessions["new"] = &models.Session{ID: "new", WorktreePath: path}
						case "new process":
							process.paths = []string{path}
						case "new pane":
							panes.paths = []string{path}
						case "new process error":
							process.err = errors.New("lsof failed on refresh")
						case "new pane error":
							panes.err = errors.New("tmux failed on refresh")
						}
					}
				}}
			}
			l.SetUnrecordedWorktreePaths(&process, &panes)
			report, err := l.reconcileUnrecordedWorktrees(context.Background(), now)
			if err != nil {
				t.Fatal(err)
			}
			if protectedFile != "" {
				if data, err := os.ReadFile(protectedFile); err != nil || string(data) != "local data" || report.Held["dirty"] != 1 {
					t.Fatalf("ignored data must survive: data=%q err=%v report=%+v", data, err, report)
				}
			}
			_, statErr := os.Stat(path)
			if name == "idle" {
				if !os.IsNotExist(statErr) || report.Removed != 1 {
					t.Fatalf("report=%+v stat=%v", report, statErr)
				}
				run(repo, "show-ref", "--verify", "refs/heads/topic")
			} else if statErr != nil || report.Removed != 0 {
				t.Fatalf("held worktree: report=%+v stat=%v", report, statErr)
			}
		})
	}
}

func TestReconcileWorktreeResidue_Sessions_AC222901(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint("live-on-refresh=", live), func(t *testing.T) {
			repo, path := keptFixture(t)
			repo, _ = filepath.EvalSymlinks(repo)
			path, _ = filepath.EvalSymlinks(path)
			base := filepath.Dir(path)
			residue := filepath.Join(base, "nested", "old")
			for _, p := range []string{filepath.Join(residue, ".claude"), filepath.Join(base, "other", ".claude")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "settings.local.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(base, "other", "keep"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			sessions := newMockSessionStore()
			sessions.sessions["s"] = &models.Session{ID: "s", WorktreePath: path}
			repos := newMockRepoStore()
			repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo, DisplayName: filepath.Base(base), WorktreeBaseDir: filepath.Dir(base)}
			l := &Lifecycle{sessions: sessions, repos: repos, worktrees: gitpkg.NewManager(zerolog.Nop()), logger: zerolog.Nop()}
			process := &reconcilePaths{}
			l.SetUnrecordedWorktreePaths(process, reconcilePaths{})
			if live {
				calls := 0
				l.sessions = &keptMarkerRefreshStore{SessionStore: sessions, refresh: func() {
					calls++
					if calls == 2 {
						process.paths = []string{residue}
					}
				}}
			}
			report, err := l.reconcileUnrecordedWorktrees(context.Background(), time.Now().Add(48*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if live {
				if report.ResidueRemoved != 0 || report.Held["process"] != 1 {
					t.Fatalf("live residue: %+v", report)
				}
				if _, err := os.Stat(residue); err != nil {
					t.Fatal(err)
				}
				return
			}
			if report.ResidueRemoved != 1 {
				t.Fatalf("report=%+v", report)
			}
			if _, err := os.Stat(filepath.Join(base, "nested")); !os.IsNotExist(err) {
				t.Fatalf("empty parents remain: %v", err)
			}
			for _, p := range []string{base, path, repo, filepath.Join(base, "other", "keep")} {
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("protected %s: %v", p, err)
				}
			}

		})
	}
}
