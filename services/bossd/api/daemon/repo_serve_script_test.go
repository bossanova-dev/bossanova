package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/adapter/setupscript"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
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

func TestUpdateRepoRepublishesActiveSessionsWhenSessionFieldsChange(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	repos := db.NewRepoStore(database)
	sessions := db.NewSessionStore(database)
	newRepo := func(name string) string {
		r, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: name, LocalPath: "/tmp/" + name, OriginURL: "https://github.com/o/" + name + ".git", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt"})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	newSession := func(repoID, branch string) string {
		sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repoID, Title: branch, WorktreePath: "/tmp/wt/" + branch, BranchName: branch, BaseBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		return sess.ID
	}
	target, other := newRepo("target"), newRepo("other")
	active := newSession(target, "active")
	if err := sessions.MarkArchived(ctx, newSession(target, "archived")); err != nil {
		t.Fatal(err)
	}
	newSession(other, "elsewhere")

	var published []*pb.Session
	s := New(Config{
		Repos:              repos,
		Sessions:           sessions,
		Logger:             zerolog.Nop(),
		ValidateRepoScript: setupscript.ValidateStored,
		OnSessionUpdated:   func(_ context.Context, p *pb.Session) { published = append(published, p) },
	})
	serve, name, merge := "make serve", "renamed", true
	for _, tc := range []struct {
		name     string
		req      *pb.UpdateRepoRequest
		wantName string
	}{
		{"serve command", &pb.UpdateRepoRequest{Id: target, ServeScript: &serve}, "target"},
		{"display name", &pb.UpdateRepoRequest{Id: target, DisplayName: &name}, "renamed"},
		{"unrelated flag", &pb.UpdateRepoRequest{Id: target, CanAutoMerge: &merge}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			published = nil
			if _, err := s.UpdateRepo(ctx, connect.NewRequest(tc.req)); err != nil {
				t.Fatal(err)
			}
			if tc.wantName == "" {
				if len(published) != 0 {
					t.Fatalf("published %d sessions, want none", len(published))
				}
				return
			}
			if len(published) != 1 || published[0].GetId() != active {
				t.Fatalf("published = %v, want only the active session %s", published, active)
			}
			if got := published[0].GetRepoDisplayName(); got != tc.wantName {
				t.Fatalf("repo display name = %q, want %q", got, tc.wantName)
			}
		})
	}
}
