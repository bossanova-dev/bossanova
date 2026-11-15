package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/adapter/setupscript"
)

func TestRepoServeScript(t *testing.T) {
	seedWorktreeBaseSettings(t)
	for _, tc := range []struct {
		value   string
		invalid bool
	}{
		{"make serve", false},
		{`{"type":"command","argv":["npm","run","dev"]}`, false},
		{"", false},
		{`{"type":"script","path":"../escape"}`, true},
		{`{"type":"make","target":""}`, true},
		{"{broken", true},
	} {
		value := tc.value
		t.Run(value, func(t *testing.T) {
			invalid := tc.invalid
			for _, operation := range []string{"register", "clone", "update"} {
				t.Run(operation, func(t *testing.T) {
					repos := &repoMgmtRepoStore{}
					worktrees := &repoMgmtWorktree{isGitRepo: true, originURL: "https://github.com/o/r"}
					s := New(Config{Repos: repos, Worktrees: worktrees, ValidateRepoScript: setupscript.ValidateStored})
					var err error
					switch operation {
					case "register":
						_, err = s.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{LocalPath: t.TempDir(), ServeScript: &value}))
					case "clone":
						_, err = s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{LocalPath: t.TempDir(), CloneUrl: worktrees.originURL, ServeScript: &value}))
					case "update":
						_, err = s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{Id: "r1", ServeScript: &value}))
					}
					if invalid {
						if connect.CodeOf(err) != connect.CodeInvalidArgument {
							t.Fatalf("error = %v, want INVALID_ARGUMENT", err)
						}
						if repos.createCalls != 0 || repos.updateCalls != 0 || worktrees.cloneCalls != 0 {
							t.Fatal("invalid command caused a write")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					stored := repos.createParams.ServeScript
					if operation == "update" {
						if repos.updateParams.ServeScript == nil {
							t.Fatal("serve directive is absent")
						}
						stored = *repos.updateParams.ServeScript
						if repos.updateParams.SetupScript != nil {
							t.Fatal("serve update changed setup directive")
						}
					}
					if value == "" {
						if stored != nil {
							t.Fatal("empty command should store NULL")
						}
					} else if stored == nil || *stored != value {
						t.Fatalf("stored command = %v, want %q", stored, value)
					}
				})
			}
		})
	}
	store := &repoMgmtRepoStore{}
	s := New(Config{Repos: store, ValidateRepoScript: setupscript.ValidateStored})
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{Id: "r1"})); err != nil {
		t.Fatal(err)
	}
	if store.updateParams.ServeScript != nil {
		t.Fatal("absent serve field should leave the stored command alone")
	}
}
