package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

type recordingFactorySkillsInstaller struct {
	mu      sync.Mutex
	reasons []string
}

func (r *recordingFactorySkillsInstaller) InstallOnConsent(_ context.Context, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func (r *recordingFactorySkillsInstaller) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.reasons))
	copy(out, r.reasons)
	return out
}

type factorySkillsFailingCronStore struct {
	cronJobStore
	createErr error
}

func (s *factorySkillsFailingCronStore) Create(context.Context, domain.CreateCronJobParams) (*domain.CronJob, error) {
	return nil, s.createErr
}

func TestFactorySkillsInstalledOnConsent(t *testing.T) {
	seedWorktreeBaseSettings(t)

	tests := []struct {
		name        string
		run         func(t *testing.T, installer factorySkillsInstaller) error
		nilHook     bool
		wantErr     bool
		wantReasons []string
	}{
		{
			name: "RegisterRepo succeeds and installs",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				dir := t.TempDir()
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{isGitRepo: true},
					FactorySkills: installer,
				})
				_, err := s.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{LocalPath: dir}))
				return err
			},
			wantReasons: []string{"repo-register"},
		},
		{
			name: "RegisterRepo validation failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{isGitRepo: false},
					FactorySkills: installer,
				})
				_, err := s.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{LocalPath: t.TempDir()}))
				return err
			},
			wantErr: true,
		},
		{
			name: "RegisterRepo store failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s := New(Config{
					Repos:         &repoMgmtRepoStore{createErr: errors.New("db down")},
					Worktrees:     &repoMgmtWorktree{isGitRepo: true},
					FactorySkills: installer,
				})
				_, err := s.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{LocalPath: t.TempDir()}))
				return err
			},
			wantErr: true,
		},
		{
			name:    "RegisterRepo succeeds with nil installer",
			nilHook: true,
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{isGitRepo: true},
					FactorySkills: installer,
				})
				_, err := s.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{LocalPath: t.TempDir()}))
				return err
			},
		},
		{
			name: "CloneAndRegisterRepo succeeds and installs",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				target := filepath.Join(t.TempDir(), "cloned")
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{originURL: "git@github.com:o/r.git"},
					FactorySkills: installer,
				})
				_, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:o/r.git",
					LocalPath: target,
				}))
				return err
			},
			wantReasons: []string{"repo-register"},
		},
		{
			name: "CloneAndRegisterRepo validation failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{},
					FactorySkills: installer,
				})
				_, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:o/r.git",
					LocalPath: "relative/path",
				}))
				return err
			},
			wantErr: true,
		},
		{
			name: "CloneAndRegisterRepo clone failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				target := filepath.Join(t.TempDir(), "cloned")
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{cloneErr: errors.New("network")},
					FactorySkills: installer,
				})
				_, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:o/r.git",
					LocalPath: target,
				}))
				return err
			},
			wantErr: true,
		},
		{
			name: "CloneAndRegisterRepo store failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				target := filepath.Join(t.TempDir(), "cloned")
				s := New(Config{
					Repos:         &repoMgmtRepoStore{createErr: errors.New("db down")},
					Worktrees:     &repoMgmtWorktree{originURL: "git@github.com:o/r.git"},
					FactorySkills: installer,
				})
				_, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:o/r.git",
					LocalPath: target,
				}))
				return err
			},
			wantErr: true,
		},
		{
			name:    "CloneAndRegisterRepo succeeds with nil installer",
			nilHook: true,
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				existing := filepath.Join(t.TempDir(), "existing")
				if err := os.Mkdir(existing, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				s := New(Config{
					Repos:         &repoMgmtRepoStore{},
					Worktrees:     &repoMgmtWorktree{originURL: "git@github.com:o/r.git"},
					FactorySkills: installer,
				})
				_, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:o/r.git",
					LocalPath: existing,
				}))
				return err
			},
		},
		{
			name: "CreateCronJob succeeds and installs",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s, repoID, ctx := newCronTestServer(t)
				s.factorySkills = installer
				_, err := s.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
					RepoId:    repoID,
					Name:      "Build",
					Prompt:    "run boss-build",
					Schedule:  "@daily",
					AgentName: "codex",
					IsEnabled: true,
				}))
				return err
			},
			wantReasons: []string{"cron-create"},
		},
		{
			name: "CreateCronJob validation failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s, repoID, ctx := newCronTestServer(t)
				s.factorySkills = installer
				_, err := s.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
					RepoId:    repoID,
					Name:      "   ",
					Prompt:    "run boss-build",
					Schedule:  "@daily",
					AgentName: "codex",
				}))
				return err
			},
			wantErr: true,
		},
		{
			name: "CreateCronJob store failure skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s, repoID, ctx := newCronTestServer(t)
				s.cronJobs = &factorySkillsFailingCronStore{createErr: errors.New("db down")}
				s.factorySkills = installer
				_, err := s.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
					RepoId:    repoID,
					Name:      "Build",
					Prompt:    "run boss-build",
					Schedule:  "@daily",
					AgentName: "codex",
				}))
				return err
			},
			wantErr: true,
		},
		{
			name: "CreateCronJob AddJob rollback skips install",
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s, repoID, ctx := newCronTestServer(t)
				s.factorySkills = installer
				_, err := s.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
					RepoId:    repoID,
					Name:      "Bad schedule",
					Prompt:    "run boss-build",
					Schedule:  "not a cron expression",
					AgentName: "codex",
					IsEnabled: true,
				}))
				return err
			},
			wantErr: true,
		},
		{
			name:    "CreateCronJob succeeds with nil installer",
			nilHook: true,
			run: func(t *testing.T, installer factorySkillsInstaller) error {
				s, repoID, ctx := newCronTestServer(t)
				s.factorySkills = installer
				_, err := s.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
					RepoId:    repoID,
					Name:      "Build",
					Prompt:    "run boss-build",
					Schedule:  "@daily",
					AgentName: "codex",
					IsEnabled: true,
				}))
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				recorder  *recordingFactorySkillsInstaller
				installer factorySkillsInstaller
			)
			if !tc.nilHook {
				recorder = &recordingFactorySkillsInstaller{}
				installer = recorder
			}
			err := tc.run(t, installer)
			if tc.wantErr {
				if err == nil {
					t.Fatal("RPC error = nil, want error")
				}
			} else if err != nil {
				t.Fatalf("RPC error = %v", err)
			}
			if recorder == nil {
				return
			}
			got := recorder.snapshot()
			if len(tc.wantReasons) == 0 {
				if len(got) != 0 {
					t.Fatalf("InstallOnConsent calls = %v, want none", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.wantReasons) {
				t.Fatalf("InstallOnConsent calls = %v, want %v", got, tc.wantReasons)
			}
		})
	}
}

// TestFactorySkillsConsentWiring_BossSkills_AC271201 links AC271201 to the RPC
// consent-hook tests while keeping TestFactorySkillsInstalledOnConsent's exact name.
func TestFactorySkillsConsentWiring_BossSkills_AC271201(t *testing.T) {
	TestFactorySkillsInstalledOnConsent(t)
}
