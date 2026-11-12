package server

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
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
		defer time.Sleep(5 * time.Second)
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

type budgetCloudIdentityProvider struct {
	vcs.Provider
	read func(context.Context, string) (int64, string, error)
}

func (p budgetCloudIdentityProvider) GetRepositoryIdentity(ctx context.Context, origin string) (int64, string, error) {
	return p.read(ctx, origin)
}

func TestListRepoCloudControlsBudget_Settings_AC242901_AC242902(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", t.TempDir()+"/settings.json")
	for _, mode := range []string{"full", "private"} {
		t.Run(mode, func(t *testing.T) {
			machine := "private"
			if mode == "private" {
				machine = "full"
			}
			settings := config.Settings{CloudControl: machine, CloudControlRepos: []config.CloudControlRepoGrant{{Host: "github.com", RepoID: 42, Name: "acme/slow", Checkout: "/slow", Mode: mode}}}
			if err := config.Save(settings); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				defer time.Sleep(5 * time.Second)
				repos := &repoMgmtRepoStore{listRepos: []*models.Repo{
					{ID: "slow", DisplayName: "slow", OriginURL: "https://github.com/acme/slow", LocalPath: "/slow"},
					{ID: "fast", DisplayName: "fast", OriginURL: "https://github.com/acme/fast", LocalPath: "/fast"},
					{ID: "other", DisplayName: "other", OriginURL: "https://github.com/acme/other", LocalPath: "/other"},
					{ID: "fourth", DisplayName: "fourth", OriginURL: "https://github.com/acme/fourth", LocalPath: "/fourth"},
					{ID: "fifth", DisplayName: "fifth", OriginURL: "https://github.com/acme/fifth", LocalPath: "/fifth"},
				}}
				provider := budgetCloudIdentityProvider{read: func(ctx context.Context, origin string) (int64, string, error) {
					if strings.HasSuffix(origin, "/slow") {
						<-ctx.Done()
						return 0, "", ctx.Err()
					}
					return 42, "private", nil
				}}
				server := New(Config{Repos: repos, Provider: provider})
				started := time.Now()
				response, err := server.ListRepoCloudControls(context.Background(), connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
				if err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(started); elapsed > repoIdentityBudget {
					t.Fatalf("list budget exceeded: %v", elapsed)
				}
				if len(response.Msg.Repos) != len(repos.listRepos) {
					t.Fatal(response.Msg)
				}
				for i, row := range response.Msg.Repos {
					registered := repos.listRepos[i]
					if row.DisplayName != registered.DisplayName || row.Checkout != registered.LocalPath || row.OriginUrl != registered.OriginURL || row.Host != "github.com" || row.Name != "acme/"+registered.ID {
						t.Fatalf("metadata: %+v", row)
					}
					if row.IsIdVerified != (i > 0) {
						t.Fatalf("verification: %+v", row)
					}
				}
				slow := response.Msg.Repos[0]
				if slow.ProviderRepoId != 0 || slow.EffectiveMode != "private" {
					t.Fatalf("failed closed/narrowed: %+v", slow)
				}
				if mode == "full" && !slow.HasUnverifiedGrant {
					t.Fatalf("missing unverified grant: %+v", slow)
				}
				if mode == "private" && slow.Source != "grant-narrowed" {
					t.Fatalf("narrowing source: %+v", slow)
				}
			})
		})
	}
}

func TestListRepoCloudControlsConcurrentAndCached(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", t.TempDir()+"/settings.json")
	synctest.Test(t, func(t *testing.T) {
		defer time.Sleep(5 * time.Second)
		var calls, active, high atomic.Int32
		repos := &repoMgmtRepoStore{}
		for i := range 8 {
			repos.listRepos = append(repos.listRepos, &models.Repo{ID: fmt.Sprint(i), OriginURL: fmt.Sprintf("https://github.com/acme/r%d", i)})
		}
		provider := budgetCloudIdentityProvider{read: func(context.Context, string) (int64, string, error) {
			calls.Add(1)
			n := active.Add(1)
			for prev := high.Load(); n > prev && !high.CompareAndSwap(prev, n); prev = high.Load() {
			}
			defer active.Add(-1)
			time.Sleep(time.Second)
			return 42, "private", nil
		}}
		server := New(Config{Repos: repos, Provider: provider})
		started := time.Now()
		for range 2 {
			response, err := server.ListRepoCloudControls(context.Background(), connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			for _, repo := range response.Msg.Repos {
				if !repo.IsIdVerified {
					t.Fatalf("unverified: %+v", repo)
				}
			}
		}
		if elapsed := time.Since(started); elapsed != 2*time.Second {
			t.Fatalf("fanout/cache elapsed %v", elapsed)
		}
		if calls.Load() != 8 || high.Load() > 4 {
			t.Fatalf("reads=%d concurrent=%d", calls.Load(), high.Load())
		}
	})
}

type suggestionIdentityProvider struct {
	vcs.Provider
	visibility string
	failure    error
}

func (p suggestionIdentityProvider) GetRepositoryIdentity(context.Context, string) (int64, string, error) {
	return 42, p.visibility, p.failure
}
func TestPublicRepoSuggestions(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", t.TempDir()+"/settings.json")
	checkout := t.TempDir()
	for _, tc := range []struct {
		visibility, origin, grant string
		suggested                 bool
	}{
		{"public", "https://github.com/acme/repo", "", true},
		{"internal", "https://github.com/acme/repo", "", true},
		{"private", "https://github.com/acme/repo", "", false},
		{"", "https://github.com/acme/repo", "", false},
		{"public", "https://enterprise.example/acme/repo", "", false},
		{"public", "https://gitlab.com/acme/repo", "", false},
		{"public", "https://github.com/acme/repo", "full", false},
		{"public", "https://github.com/acme/repo", "read-only", false},
	} {
		t.Run(tc.visibility+tc.origin+tc.grant, func(t *testing.T) {
			settings := config.Settings{}
			if tc.grant != "" {
				settings.CloudControlRepos = []config.CloudControlRepoGrant{{Host: "github.com", RepoID: 42, Name: "acme/repo", Checkout: checkout, Mode: tc.grant}}
			}
			if err := config.Save(settings); err != nil {
				t.Fatal(err)
			}
			repos := &repoMgmtRepoStore{listRepos: []*models.Repo{{ID: "repo", OriginURL: tc.origin, LocalPath: checkout}}}
			server := New(Config{Repos: repos, Provider: suggestionIdentityProvider{visibility: tc.visibility}})
			response, err := server.ListRepoCloudControls(t.Context(), connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			got := response.Msg.Repos[0]
			if got.IsSuggested != tc.suggested {
				t.Fatalf("suggestion: %+v", got)
			}
			if tc.grant == "" && got.EffectiveMode != "full" {
				t.Fatalf("visibility changed authority: %+v", got)
			}
		})
	}
}
