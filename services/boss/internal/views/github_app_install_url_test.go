package views

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnrichGitHubAppInstallURL_TargetsRepoOwner(t *testing.T) {
	originalLookup := lookupGitHubAppInstallTarget
	lookupGitHubAppInstallTarget = func(_ context.Context, nwo string) (githubAppInstallTarget, error) {
		if nwo != "recurser/bossanova" {
			t.Fatalf("nwo = %q, want recurser/bossanova", nwo)
		}
		return githubAppInstallTarget{id: "140061029", kind: "Organization"}, nil
	}
	t.Cleanup(func() { lookupGitHubAppInstallTarget = originalLookup })

	got := enrichGitHubAppInstallURL(
		context.Background(),
		"https://github.com/apps/bossanova-dev/installations/select_target?state=abc123",
		"https://github.com/recurser/bossanova.git",
	)
	want := "https://github.com/apps/bossanova-dev/installations/new/permissions?state=abc123&target_id=140061029&target_type=Organization"
	if got != want {
		t.Fatalf("enriched URL = %q, want %q", got, want)
	}
}

func TestEnrichGitHubAppInstallURL_FallsBackWhenLookupFails(t *testing.T) {
	originalLookup := lookupGitHubAppInstallTarget
	lookupGitHubAppInstallTarget = func(context.Context, string) (githubAppInstallTarget, error) {
		return githubAppInstallTarget{}, context.Canceled
	}
	t.Cleanup(func() { lookupGitHubAppInstallTarget = originalLookup })

	raw := "https://github.com/apps/bossanova-dev/installations/new?state=abc123"
	got := enrichGitHubAppInstallURL(context.Background(), raw, "https://github.com/recurser/bossanova.git")
	if got != raw {
		t.Fatalf("enriched URL = %q, want fallback %q", got, raw)
	}
}

func TestEnrichGitHubAppInstallURL_EnrichesNewInstallAndLeavesOtherPaths(t *testing.T) {
	originalLookup := lookupGitHubAppInstallTarget
	lookupGitHubAppInstallTarget = func(context.Context, string) (githubAppInstallTarget, error) {
		return githubAppInstallTarget{id: "123", kind: "User"}, nil
	}
	t.Cleanup(func() { lookupGitHubAppInstallTarget = originalLookup })

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "new install gains permissions path",
			raw:  "https://github.com/apps/bossanova/installations/new?state=keep",
			want: "https://github.com/apps/bossanova/installations/new/permissions?state=keep&target_id=123&target_type=User",
		},
		{
			name: "other GitHub app path keeps path",
			raw:  "https://github.com/apps/bossanova",
			want: "https://github.com/apps/bossanova?target_id=123&target_type=User",
		},
		{
			name: "non GitHub URL falls back",
			raw:  "https://example.com/apps/bossanova/installations/new",
			want: "https://example.com/apps/bossanova/installations/new",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := enrichGitHubAppInstallURL(context.Background(), tt.raw, "https://github.com/recurser/bossanova.git"); got != tt.want {
				t.Fatalf("enrichGitHubAppInstallURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnrichGitHubAppInstallURL_FallsBackWithoutGitHubRepository(t *testing.T) {
	originalLookup := lookupGitHubAppInstallTarget
	lookupGitHubAppInstallTarget = func(context.Context, string) (githubAppInstallTarget, error) {
		t.Fatal("lookup must not run for a non-GitHub repository")
		return githubAppInstallTarget{}, nil
	}
	t.Cleanup(func() { lookupGitHubAppInstallTarget = originalLookup })

	raw := "https://github.com/apps/bossanova/installations/new"
	if got := enrichGitHubAppInstallURL(context.Background(), raw, "https://gitlab.com/recurser/bossanova.git"); got != raw {
		t.Fatalf("enrichGitHubAppInstallURL() = %q, want fallback %q", got, raw)
	}
}

func TestLookupGitHubAppInstallTargetWithGH(t *testing.T) {
	originalTimeout := githubAppInstallTargetLookupTimeout
	githubAppInstallTargetLookupTimeout = 30 * time.Second
	t.Cleanup(func() { githubAppInstallTargetLookupTimeout = originalTimeout })

	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	t.Setenv("PATH", dir)

	writeGH := func(t *testing.T, script string) {
		t.Helper()
		if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("parses owner target", func(t *testing.T) {
		writeGH(t, "#!/bin/sh\n[ \"$1\" = api ] && [ \"$2\" = repos/recurser/bossanova ] && [ \"$3\" = --jq ] || exit 1\nprintf '140061029\\tOrganization\\n'\n")

		got, err := lookupGitHubAppInstallTargetWithGH(context.TODO(), "recurser/bossanova")
		if err != nil {
			t.Fatal(err)
		}
		if want := (githubAppInstallTarget{id: "140061029", kind: "Organization"}); got != want {
			t.Fatalf("target = %#v, want %#v", got, want)
		}
	})

	t.Run("returns empty target for incomplete output", func(t *testing.T) {
		writeGH(t, "#!/bin/sh\nprintf '140061029\\n'\n")

		got, err := lookupGitHubAppInstallTargetWithGH(context.Background(), "recurser/bossanova")
		if err != nil {
			t.Fatal(err)
		}
		if got != (githubAppInstallTarget{}) {
			t.Fatalf("target = %#v, want empty", got)
		}
	})

	t.Run("returns command error", func(t *testing.T) {
		writeGH(t, "#!/bin/sh\nexit 1\n")

		if _, err := lookupGitHubAppInstallTargetWithGH(context.Background(), "recurser/bossanova"); err == nil {
			t.Fatal("lookupGitHubAppInstallTargetWithGH() error = nil, want command error")
		}
	})
}
