package views

import (
	"context"
	"testing"
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
