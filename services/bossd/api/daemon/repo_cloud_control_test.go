package server

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
)

func TestListRepoCloudControlsLocalIdentity(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", t.TempDir()+"/settings.json")
	repos := &repoMgmtRepoStore{listRepos: []*models.Repo{{ID: "repo", OriginURL: "git@github.com:Acme/Repo.git", LocalPath: t.TempDir(), DisplayName: "work"}}}
	s := New(Config{Repos: repos})
	response, err := s.ListRepoCloudControls(context.Background(), connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Msg.Repos) != 1 {
		t.Fatal(response.Msg)
	}
	repo := response.Msg.Repos[0]
	if repo.Host != "github.com" || repo.Name != "acme/repo" || repo.IsIdVerified || repo.EffectiveMode != "full" || repo.Source != "default" {
		t.Fatalf("repo: %+v", repo)
	}
}

type localCloudIdentityProvider struct{ vcs.Provider }

func (localCloudIdentityProvider) GetRepositoryIdentity(context.Context, string) (int64, string, error) {
	return 42, "private", nil
}

func TestListRepoCloudControlsVerifiedGrant(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", t.TempDir()+"/settings.json")
	checkout := t.TempDir()
	settings := config.Settings{CloudControlRepos: []config.CloudControlRepoGrant{{Host: "github.com", RepoID: 42, Name: "acme/repo", Checkout: checkout, Mode: "full"}}}
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}
	repos := &repoMgmtRepoStore{listRepos: []*models.Repo{{ID: "repo", OriginURL: "git@github.com:Acme/Repo.git", LocalPath: checkout}}}
	server := New(Config{Repos: repos, Provider: localCloudIdentityProvider{}})
	response, err := server.ListRepoCloudControls(context.Background(), connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	repo := response.Msg.Repos[0]
	if !repo.IsIdVerified || repo.ProviderRepoId != 42 || repo.EffectiveMode != "full" || repo.Source != "grant" || repo.HasUnverifiedGrant {
		t.Fatalf("repo: %+v", repo)
	}
}

type waitingCloudIdentityProvider struct{ vcs.Provider }

func (waitingCloudIdentityProvider) GetRepositoryIdentity(ctx context.Context, _ string) (int64, string, error) {
	<-ctx.Done()
	return 0, "", ctx.Err()
}

func TestListRepoCloudControlsSlowProviderAllowsNarrowing(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", t.TempDir()+"/settings.json")
	first, second := t.TempDir(), t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		repos := &repoMgmtRepoStore{listRepos: []*models.Repo{
			{ID: "first", OriginURL: "https://github.com/acme/first", LocalPath: first},
			{ID: "second", OriginURL: "https://github.com/acme/second", LocalPath: second},
		}}
		server := New(Config{Repos: repos, Provider: waitingCloudIdentityProvider{}})
		response, err := server.ListRepoCloudControls(ctx, connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
		if err != nil || ctx.Err() != nil {
			t.Fatalf("listing exhausted caller deadline: response=%v err=%v context=%v", response, err, ctx.Err())
		}
		if len(response.Msg.Repos) != 2 {
			t.Fatalf("registered repos missing: %v", response.Msg.Repos)
		}
		for _, repo := range response.Msg.Repos {
			identity := config.RepoIdentity{Host: repo.Host, Name: repo.Name, LocalPath: repo.Checkout, ProviderRepoID: repo.ProviderRepoId, IsIDVerified: repo.IsIdVerified}
			var settings config.Settings
			if err := config.SetRepoCloudControl(&settings, identity, "read-only"); err != nil {
				t.Fatalf("narrowing %s: %v", repo.Name, err)
			}
			if err := config.SetRepoCloudControl(&settings, identity, "full"); err == nil {
				t.Fatalf("unverified full grant accepted for %s", repo.Name)
			}
		}
	})
}
