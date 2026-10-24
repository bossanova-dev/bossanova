package settings

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/auth"
)

func TestCloudSettingsBlock(t *testing.T) {
	block := cloudSettingsBlock()
	for _, want := range []string{
		"Bossanova Cloud",
		"14-day free trial",
		"press [l]ogin from Home",
		"local mode stays free",
		"optional",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("cloudSettingsBlock() = %q, want %q", block, want)
		}
	}
}

func TestSettingsViewHidesCloudBlockWithoutAuth(t *testing.T) {
	m := NewModel(context.Background())
	view := m.View().Content
	if strings.Contains(view, "press [l]ogin from Home") {
		t.Fatalf("settings view showed cloud login prompt without auth configured: %q", view)
	}
}

func TestShouldShowCloudSettings(t *testing.T) {
	cases := []struct {
		name   string
		tokens *auth.Tokens
		want   bool
	}{
		{name: "never logged in", tokens: nil, want: true},
		{
			name: "signed-out account holder needing re-login",
			tokens: &auth.Tokens{
				Email:         "dev@example.test",
				NeedsRelogin:  true,
				ReloginReason: auth.ReloginReasonRefreshTokenRejected,
			},
			want: false,
		},
		{
			name: "logged in",
			tokens: &auth.Tokens{
				AccessToken:  "access",
				RefreshToken: "refresh",
				Email:        "dev@example.test",
				ExpiresAt:    time.Now().Add(time.Hour),
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := auth.NewManager(&repoAddTokenStore{tokens: tc.tokens}, auth.Config{})
			if got := shouldShowCloudSettings(mgr); got != tc.want {
				t.Fatalf("shouldShowCloudSettings = %v, want %v", got, tc.want)
			}
		})
	}

	if shouldShowCloudSettings() {
		t.Fatal("shouldShowCloudSettings() with no manager = true, want false")
	}
	if shouldShowCloudSettings(nil) {
		t.Fatal("shouldShowCloudSettings(nil) = true, want false")
	}
}
