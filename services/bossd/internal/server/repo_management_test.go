package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// repoMgmtRepoStore is a recording fake db.RepoStore for the repo-management
// RPC handler tests. Only the methods those handlers exercise are implemented;
// the embedded interface is nil so any unexpected call panics and fails loudly.
type repoMgmtRepoStore struct {
	db.RepoStore

	createParams db.CreateRepoParams
	createCalls  int
	createErr    error

	listRepos []*models.Repo
	listErr   error

	deleteID    string
	deleteCalls int
	deleteErr   error

	updateID     string
	updateParams db.UpdateRepoParams
	updateCalls  int
	updateErr    error
}

func (f *repoMgmtRepoStore) Create(_ context.Context, params db.CreateRepoParams) (*models.Repo, error) {
	f.createCalls++
	f.createParams = params
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &models.Repo{
		ID:          "repo-created",
		LocalPath:   params.LocalPath,
		OriginURL:   params.OriginURL,
		SetupScript: params.SetupScript,
	}, nil
}

func (f *repoMgmtRepoStore) List(context.Context) ([]*models.Repo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRepos, nil
}

func (f *repoMgmtRepoStore) Delete(_ context.Context, id string) error {
	f.deleteCalls++
	f.deleteID = id
	return f.deleteErr
}

func (f *repoMgmtRepoStore) Update(_ context.Context, id string, params db.UpdateRepoParams) (*models.Repo, error) {
	f.updateCalls++
	f.updateID = id
	f.updateParams = params
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &models.Repo{ID: id}, nil
}

// repoMgmtWorktree is a configurable fake worktree manager for the
// repo-management handler tests; it builds on setupStreamWorktree's no-op
// defaults and records whether Clone was called.
type repoMgmtWorktree struct {
	setupStreamWorktree

	isGitRepo  bool
	originURL  string
	cloneErr   error
	cloneCalls int
}

func (w *repoMgmtWorktree) IsGitRepo(context.Context, string) bool { return w.isGitRepo }

func (w *repoMgmtWorktree) DetectOriginURL(context.Context, string) (string, error) {
	return w.originURL, nil
}

func (w *repoMgmtWorktree) Clone(context.Context, string, string) error {
	w.cloneCalls++
	return w.cloneErr
}

func TestRegisterRepo(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "file")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	setup := "echo hi"

	tests := []struct {
		name       string
		req        *pb.RegisterRepoRequest
		worktrees  *repoMgmtWorktree
		createErr  error
		wantErrSub string
		wantSetup  *string
	}{
		{
			name:       "empty local path",
			req:        &pb.RegisterRepoRequest{},
			wantErrSub: "local_path is required",
		},
		{
			name:       "path does not exist",
			req:        &pb.RegisterRepoRequest{LocalPath: filepath.Join(dir, "missing")},
			wantErrSub: "path does not exist",
		},
		{
			name:       "path is a file",
			req:        &pb.RegisterRepoRequest{LocalPath: filePath},
			wantErrSub: "path is not a directory",
		},
		{
			name:       "not a git repo",
			req:        &pb.RegisterRepoRequest{LocalPath: dir},
			worktrees:  &repoMgmtWorktree{isGitRepo: false},
			wantErrSub: "not a git repository",
		},
		{
			name:      "valid with setup script",
			req:       &pb.RegisterRepoRequest{LocalPath: dir, SetupScript: &setup},
			worktrees: &repoMgmtWorktree{isGitRepo: true, originURL: "git@github.com:o/r.git"},
			wantSetup: &setup,
		},
		{
			name:      "valid without setup script",
			req:       &pb.RegisterRepoRequest{LocalPath: dir},
			worktrees: &repoMgmtWorktree{isGitRepo: true},
			wantSetup: nil,
		},
		{
			name:       "create fails",
			req:        &pb.RegisterRepoRequest{LocalPath: dir},
			worktrees:  &repoMgmtWorktree{isGitRepo: true},
			createErr:  errors.New("boom"),
			wantErrSub: "create repo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repos := &repoMgmtRepoStore{createErr: tc.createErr}
			wt := tc.worktrees
			if wt == nil {
				wt = &repoMgmtWorktree{}
			}
			s := New(Config{Repos: repos, Worktrees: wt})

			resp, err := s.RegisterRepo(context.Background(), connect.NewRequest(tc.req))
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("RegisterRepo() error = nil, want substring %q", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("RegisterRepo() error = %v, want substring %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("RegisterRepo() error = %v", err)
			}
			if resp.Msg.Repo == nil {
				t.Fatal("RegisterRepo() returned nil repo")
			}
			if (tc.wantSetup == nil) != (repos.createParams.SetupScript == nil) {
				t.Fatalf("Create SetupScript = %v, want nil=%v", repos.createParams.SetupScript, tc.wantSetup == nil)
			}
			if tc.wantSetup != nil && *repos.createParams.SetupScript != *tc.wantSetup {
				t.Fatalf("Create SetupScript = %q, want %q", *repos.createParams.SetupScript, *tc.wantSetup)
			}
		})
	}
}

func TestCloneAndRegisterRepo(t *testing.T) {
	baseDir := t.TempDir()
	existingDir := filepath.Join(baseDir, "exists")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missingDir := filepath.Join(baseDir, "missing")
	setup := "echo hi"

	tests := []struct {
		name       string
		req        *pb.CloneAndRegisterRepoRequest
		worktrees  *repoMgmtWorktree
		createErr  error
		wantErrSub string
		wantClone  bool
		wantSetup  *string
	}{
		{
			name:       "empty clone url",
			req:        &pb.CloneAndRegisterRepoRequest{LocalPath: missingDir},
			wantErrSub: "clone_url is required",
		},
		{
			name:       "empty local path",
			req:        &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git"},
			wantErrSub: "local_path is required",
		},
		{
			name:       "existing path not a git repo",
			req:        &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git", LocalPath: existingDir},
			worktrees:  &repoMgmtWorktree{originURL: ""},
			wantErrSub: "not a git repository",
		},
		{
			name:       "existing path different origin",
			req:        &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git", LocalPath: existingDir},
			worktrees:  &repoMgmtWorktree{originURL: "git@github.com:other/x.git"},
			wantErrSub: "different origin",
		},
		{
			name:      "existing path matching origin skips clone",
			req:       &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git", LocalPath: existingDir},
			worktrees: &repoMgmtWorktree{originURL: "git@github.com:o/r.git"},
			wantClone: false,
		},
		{
			name:      "missing path clones and registers",
			req:       &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git", LocalPath: missingDir, SetupScript: &setup},
			worktrees: &repoMgmtWorktree{originURL: "git@github.com:o/r.git"},
			wantClone: true,
			wantSetup: &setup,
		},
		{
			name:       "clone fails",
			req:        &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git", LocalPath: missingDir},
			worktrees:  &repoMgmtWorktree{cloneErr: errors.New("network")},
			wantErrSub: "clone",
		},
		{
			name:       "create fails",
			req:        &pb.CloneAndRegisterRepoRequest{CloneUrl: "git@github.com:o/r.git", LocalPath: missingDir},
			worktrees:  &repoMgmtWorktree{originURL: "git@github.com:o/r.git"},
			createErr:  errors.New("boom"),
			wantErrSub: "create repo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repos := &repoMgmtRepoStore{createErr: tc.createErr}
			wt := tc.worktrees
			if wt == nil {
				wt = &repoMgmtWorktree{}
			}
			s := New(Config{Repos: repos, Worktrees: wt})

			resp, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(tc.req))
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("CloneAndRegisterRepo() error = nil, want substring %q", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("CloneAndRegisterRepo() error = %v, want substring %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("CloneAndRegisterRepo() error = %v", err)
			}
			if resp.Msg.Repo == nil {
				t.Fatal("CloneAndRegisterRepo() returned nil repo")
			}
			if gotClone := wt.cloneCalls > 0; gotClone != tc.wantClone {
				t.Fatalf("Clone called = %v, want %v", gotClone, tc.wantClone)
			}
			if (tc.wantSetup == nil) != (repos.createParams.SetupScript == nil) {
				t.Fatalf("Create SetupScript = %v, want nil=%v", repos.createParams.SetupScript, tc.wantSetup == nil)
			}
			if tc.wantSetup != nil && *repos.createParams.SetupScript != *tc.wantSetup {
				t.Fatalf("Create SetupScript = %q, want %q", *repos.createParams.SetupScript, *tc.wantSetup)
			}
		})
	}
}

func TestListRepos(t *testing.T) {
	t.Run("list error surfaces", func(t *testing.T) {
		repos := &repoMgmtRepoStore{listErr: errors.New("db down")}
		s := New(Config{Repos: repos})

		_, err := s.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{}))
		if err == nil || !strings.Contains(err.Error(), "list repos") {
			t.Fatalf("ListRepos() error = %v, want list repos error", err)
		}
	})

	t.Run("success returns all repos", func(t *testing.T) {
		repos := &repoMgmtRepoStore{listRepos: []*models.Repo{{ID: "a"}, {ID: "b"}}}
		s := New(Config{Repos: repos})

		resp, err := s.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{}))
		if err != nil {
			t.Fatalf("ListRepos() error = %v", err)
		}
		if len(resp.Msg.Repos) != 2 {
			t.Fatalf("ListRepos() returned %d repos, want 2", len(resp.Msg.Repos))
		}
	})
}

func TestRemoveRepo(t *testing.T) {
	t.Run("empty id", func(t *testing.T) {
		repos := &repoMgmtRepoStore{}
		s := New(Config{Repos: repos})

		_, err := s.RemoveRepo(context.Background(), connect.NewRequest(&pb.RemoveRepoRequest{}))
		if err == nil || !strings.Contains(err.Error(), "id is required") {
			t.Fatalf("RemoveRepo() error = %v, want id required error", err)
		}
		if repos.deleteCalls != 0 {
			t.Fatalf("Delete called %d times, want 0", repos.deleteCalls)
		}
	})

	t.Run("delete error surfaces", func(t *testing.T) {
		repos := &repoMgmtRepoStore{deleteErr: errors.New("db down")}
		s := New(Config{Repos: repos})

		_, err := s.RemoveRepo(context.Background(), connect.NewRequest(&pb.RemoveRepoRequest{Id: "r1"}))
		if err == nil || !strings.Contains(err.Error(), "remove repo") {
			t.Fatalf("RemoveRepo() error = %v, want remove repo error", err)
		}
	})

	t.Run("success deletes by id", func(t *testing.T) {
		repos := &repoMgmtRepoStore{}
		s := New(Config{Repos: repos})

		if _, err := s.RemoveRepo(context.Background(), connect.NewRequest(&pb.RemoveRepoRequest{Id: "r1"})); err != nil {
			t.Fatalf("RemoveRepo() error = %v", err)
		}
		if repos.deleteCalls != 1 || repos.deleteID != "r1" {
			t.Fatalf("Delete calls=%d id=%q, want 1 r1", repos.deleteCalls, repos.deleteID)
		}
	})
}

func TestUpdateRepoSetupScript(t *testing.T) {
	empty := ""
	cmd := "echo hi"

	t.Run("empty setup script clears to NULL", func(t *testing.T) {
		repos := &repoMgmtRepoStore{}
		s := New(Config{Repos: repos})

		if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{Id: "r1", SetupScript: &empty})); err != nil {
			t.Fatalf("UpdateRepo() error = %v", err)
		}
		if repos.updateParams.SetupScript == nil {
			t.Fatal("UpdateRepo() left SetupScript directive nil, want clear directive")
		}
		if *repos.updateParams.SetupScript != nil {
			t.Fatalf("empty setup script should clear to NULL (inner nil), got inner %q", **repos.updateParams.SetupScript)
		}
	})

	t.Run("non-empty setup script sets value", func(t *testing.T) {
		repos := &repoMgmtRepoStore{}
		s := New(Config{Repos: repos})

		if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{Id: "r1", SetupScript: &cmd})); err != nil {
			t.Fatalf("UpdateRepo() error = %v", err)
		}
		if repos.updateParams.SetupScript == nil || *repos.updateParams.SetupScript == nil {
			t.Fatal("UpdateRepo() should set SetupScript to a non-nil value pointer")
		}
		if **repos.updateParams.SetupScript != cmd {
			t.Fatalf("SetupScript inner = %q, want %q", **repos.updateParams.SetupScript, cmd)
		}
	})
}
