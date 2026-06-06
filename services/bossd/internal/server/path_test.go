package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestDefaultSocketPathForSettingsUsesSocketPath(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "custom.sock")

	got, err := DefaultSocketPathForSettings(config.Settings{SocketPath: socketPath})
	if err != nil {
		t.Fatalf("DefaultSocketPathForSettings() returned error: %v", err)
	}
	if got != socketPath {
		t.Fatalf("DefaultSocketPathForSettings() = %q, want %q", got, socketPath)
	}
}

func TestDefaultSocketPathForSettingsUsesAppDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	got, err := DefaultSocketPathForSettings(config.Settings{AppDataDir: dir})
	if err != nil {
		t.Fatalf("DefaultSocketPathForSettings() returned error: %v", err)
	}
	want := filepath.Join(dir, "bossd.sock")
	if got != want {
		t.Fatalf("DefaultSocketPathForSettings() = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathForSettingsRejectsRelativeSocketPath(t *testing.T) {
	_, err := DefaultSocketPathForSettings(config.Settings{SocketPath: "relative/bossd.sock"})
	if err == nil {
		t.Fatal("DefaultSocketPathForSettings() error = nil, want relative path error")
	}
}

func TestDefaultSocketPathSitsUnderConfigAppDataDir(t *testing.T) {
	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}

	base, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir() returned error: %v", err)
	}
	want := filepath.Join(base, "bossd.sock")
	if got != want {
		t.Fatalf("DefaultSocketPath() = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathForSettingsFallsBackToDefault(t *testing.T) {
	isolateHomeEnv(t)

	got, err := DefaultSocketPathForSettings(config.Settings{})
	if err != nil {
		t.Fatalf("DefaultSocketPathForSettings() returned error: %v", err)
	}

	want, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}
	if got != want {
		t.Fatalf("DefaultSocketPathForSettings() = %q, want fallback %q", got, want)
	}
}

func TestListenRejectsRegularFileSocketPathWithoutRemovingIt(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	contents := []byte("not a socket")
	if err := os.WriteFile(socketPath, contents, 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	s := New(Config{})
	err := s.Listen(socketPath)
	if err == nil {
		if s.listener != nil {
			_ = s.listener.Close()
		}
		t.Fatal("Listen() error = nil, want regular file rejection")
	}

	got, readErr := os.ReadFile(socketPath)
	if readErr != nil {
		t.Fatalf("regular file was not preserved: %v", readErr)
	}
	if string(got) != string(contents) {
		t.Fatalf("regular file contents = %q, want %q", got, contents)
	}
}

func TestValidateRepoPath(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-dir")
	if err := os.WriteFile(filePath, []byte("contents"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	tests := []struct {
		name              string
		path              string
		worktrees         *validateRepoPathWorktree
		wantValid         bool
		wantErrorContains string
		wantOriginURL     string
		wantIsGitHub      bool
		wantDefaultBranch string
	}{
		{
			name:              "empty path",
			wantErrorContains: "path is required",
		},
		{
			name:              "missing path",
			path:              filepath.Join(dir, "missing"),
			wantErrorContains: "path does not exist",
		},
		{
			name:              "regular file",
			path:              filePath,
			wantErrorContains: "path is not a directory",
		},
		{
			name:              "directory that is not git repo",
			path:              dir,
			worktrees:         &validateRepoPathWorktree{},
			wantErrorContains: "not a git repository",
		},
		{
			name:              "valid github repo",
			path:              dir,
			worktrees:         &validateRepoPathWorktree{isGitRepo: true, originURL: "git@github.com:recurser/bossanova.git", defaultBranch: "trunk"},
			wantValid:         true,
			wantOriginURL:     "git@github.com:recurser/bossanova.git",
			wantIsGitHub:      true,
			wantDefaultBranch: "trunk",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Worktrees: tc.worktrees})

			resp, err := s.ValidateRepoPath(context.Background(), connect.NewRequest(&pb.ValidateRepoPathRequest{
				LocalPath: tc.path,
			}))
			if err != nil {
				t.Fatalf("ValidateRepoPath() error = %v", err)
			}

			got := resp.Msg
			if got.IsValid != tc.wantValid {
				t.Fatalf("ValidateRepoPath().IsValid = %v, want %v", got.IsValid, tc.wantValid)
			}
			if tc.wantErrorContains != "" {
				if !strings.Contains(got.ErrorMessage, tc.wantErrorContains) {
					t.Fatalf("ValidateRepoPath().ErrorMessage = %q, want substring %q", got.ErrorMessage, tc.wantErrorContains)
				}
				return
			}
			if got.ErrorMessage != "" {
				t.Fatalf("ValidateRepoPath().ErrorMessage = %q, want empty", got.ErrorMessage)
			}
			if got.OriginUrl != tc.wantOriginURL {
				t.Fatalf("ValidateRepoPath().OriginUrl = %q, want %q", got.OriginUrl, tc.wantOriginURL)
			}
			if got.IsGithub != tc.wantIsGitHub {
				t.Fatalf("ValidateRepoPath().IsGithub = %v, want %v", got.IsGithub, tc.wantIsGitHub)
			}
			if got.DefaultBranch != tc.wantDefaultBranch {
				t.Fatalf("ValidateRepoPath().DefaultBranch = %q, want %q", got.DefaultBranch, tc.wantDefaultBranch)
			}
		})
	}
}

type validateRepoPathWorktree struct {
	setupStreamWorktree

	isGitRepo     bool
	originURL     string
	defaultBranch string
}

func (w *validateRepoPathWorktree) IsGitRepo(context.Context, string) bool {
	return w.isGitRepo
}

func (w *validateRepoPathWorktree) DetectOriginURL(context.Context, string) (string, error) {
	return w.originURL, nil
}

func (w *validateRepoPathWorktree) DetectDefaultBranch(context.Context, string) (string, error) {
	return w.defaultBranch, nil
}

func isolateHomeEnv(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "userprofile"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
}
