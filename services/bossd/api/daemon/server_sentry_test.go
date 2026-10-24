package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/rs/zerolog"
)

func ptr(s string) *string { return &s }

// TestUpdateRepoPersistsSentryFields verifies the Sentry credential fields
// round-trip through UpdateRepo and survive a reload.
func TestUpdateRepoPersistsSentryFields(t *testing.T) {
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	srv := &Server{repos: repos, logger: zerolog.Nop()}
	ctx := context.Background()

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/acme/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	resp, err := srv.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Id:           repo.ID,
		SentryApiKey: ptr("sntrys_secret"),
		SentryOrg:    ptr("acme"),
	}))
	if err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}

	// Returned proto carries the new values.
	got := resp.Msg.GetRepo()
	if got.GetSentryApiKey() != "sntrys_secret" || got.GetSentryOrg() != "acme" {
		t.Errorf("returned repo sentry = (%q,%q), want (sntrys_secret,acme)",
			got.GetSentryApiKey(), got.GetSentryOrg())
	}

	// And they survive a reload from the store.
	reloaded, err := repos.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("reload repo: %v", err)
	}
	if reloaded.SentryAPIKey != "sntrys_secret" || reloaded.SentryOrg != "acme" {
		t.Errorf("reloaded sentry = (%q,%q), want (sntrys_secret,acme)",
			reloaded.SentryAPIKey, reloaded.SentryOrg)
	}
	// A Sentry-only update must not disturb the Linear key.
	if reloaded.LinearAPIKey != "" {
		t.Errorf("LinearAPIKey = %q, want empty", reloaded.LinearAPIKey)
	}
}

// TestListTrackerIssuesCredentialValidation covers the per-source credential
// checks that run before any plugin is consulted.
func TestListTrackerIssuesCredentialValidation(t *testing.T) {
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	srv := &Server{repos: repos, logger: zerolog.Nop()}
	ctx := context.Background()

	mkRepo := func(t *testing.T, params db.UpdateRepoParams) string {
		t.Helper()
		name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
		repo, err := repos.Create(ctx, db.CreateRepoParams{
			DisplayName:       "repo",
			LocalPath:         "/tmp/repo-" + name,
			OriginURL:         "https://github.com/acme/repo-" + name,
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/wt",
		})
		if err != nil {
			t.Fatalf("create repo: %v", err)
		}
		if _, err := repos.Update(ctx, repo.ID, params); err != nil {
			t.Fatalf("seed repo: %v", err)
		}
		return repo.ID
	}

	cases := []struct {
		name    string
		params  db.UpdateRepoParams
		source  string
		wantErr string
	}{
		{
			name:    "linear without key",
			params:  db.UpdateRepoParams{},
			source:  "linear",
			wantErr: "linear_api_key not configured",
		},
		{
			name:    "sentry missing all",
			params:  db.UpdateRepoParams{},
			source:  "sentry",
			wantErr: "sentry_api_key, sentry_org",
		},
		{
			name:    "sentry missing org",
			params:  db.UpdateRepoParams{SentryAPIKey: ptr("t")},
			source:  "sentry",
			wantErr: "sentry_org",
		},
		{
			name:    "unknown source",
			params:  db.UpdateRepoParams{},
			source:  "jira",
			wantErr: "unknown tracker source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := mkRepo(t, tc.params)
			src := tc.source
			_, err := srv.ListTrackerIssues(ctx, connect.NewRequest(&pb.ListTrackerIssuesRequest{
				RepoId: id,
				Source: &src,
			}))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestSelectTaskSourceRoutesByName verifies the plugin selection helper that
// ListTrackerIssues uses to route a request to the Linear or Sentry plugin.
func TestSelectTaskSourceRoutesByName(t *testing.T) {
	linear := &stubTaskSource{name: "linear"}
	sentry := &stubTaskSource{name: "sentry"}
	broken := &stubTaskSource{getInfoErr: errors.New("down")}
	sources := []plugin.TaskSource{broken, linear, sentry}
	ctx := context.Background()

	if got := selectTaskSource(ctx, sources, "sentry"); got != plugin.TaskSource(sentry) {
		t.Errorf("want sentry source, got %v", got)
	}
	if got := selectTaskSource(ctx, sources, "linear"); got != plugin.TaskSource(linear) {
		t.Errorf("want linear source, got %v", got)
	}
	if got := selectTaskSource(ctx, sources, "missing"); got != nil {
		t.Errorf("want nil for unknown name, got %v", got)
	}
}

// TestLoadedTaskSourceNames verifies the helper that builds the actionable
// "plugin not loaded" error: it lists the names of every responding task source
// (sorted) and skips any whose GetInfo errors.
func TestLoadedTaskSourceNames(t *testing.T) {
	sources := []plugin.TaskSource{
		&stubTaskSource{name: "sentry"},
		&stubTaskSource{getInfoErr: errors.New("down")},
		&stubTaskSource{name: "linear"},
	}

	got := loadedTaskSourceNames(context.Background(), sources)

	if len(got) != 2 || got[0] != "linear" || got[1] != "sentry" {
		t.Fatalf("loadedTaskSourceNames = %v, want [linear sentry]", got)
	}
}
