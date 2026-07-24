package upgrade

import (
	"context"
	"errors"
	"testing"
)

func TestResolveGitHubToken(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		ghOut  string
		ghErr  error
		want   string
		wantGH bool // whether the gh fallback should be consulted
	}{
		{
			name:   "GITHUB_TOKEN takes precedence",
			env:    map[string]string{"GITHUB_TOKEN": "gt", "GH_TOKEN": "ght"},
			want:   "gt",
			wantGH: false,
		},
		{
			name:   "GH_TOKEN used when GITHUB_TOKEN empty",
			env:    map[string]string{"GH_TOKEN": "ght"},
			want:   "ght",
			wantGH: false,
		},
		{
			name:   "gh fallback when env empty",
			ghOut:  "  gh-cli-token\n",
			want:   "gh-cli-token",
			wantGH: true,
		},
		{
			name:   "gh error yields empty token",
			ghErr:  errors.New("gh not found"),
			want:   "",
			wantGH: true,
		},
		{
			name:   "all sources empty yields empty token",
			ghOut:  "",
			want:   "",
			wantGH: true,
		},
		{
			name:   "whitespace-only env is ignored and falls through to gh",
			env:    map[string]string{"GITHUB_TOKEN": "   "},
			ghOut:  "tok",
			want:   "tok",
			wantGH: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldEnv, oldGH := envGetter, runGH
			defer func() { envGetter, runGH = oldEnv, oldGH }()

			envGetter = func(key string) string { return tt.env[key] }
			ghCalled := false
			runGH = func(_ context.Context, args ...string) (string, error) {
				ghCalled = true
				if len(args) != 2 || args[0] != "auth" || args[1] != "token" {
					t.Fatalf("runGH args = %v, want [auth token]", args)
				}
				return tt.ghOut, tt.ghErr
			}

			got := ResolveGitHubToken(context.Background())
			if got != tt.want {
				t.Fatalf("ResolveGitHubToken() = %q, want %q", got, tt.want)
			}
			if ghCalled != tt.wantGH {
				t.Fatalf("gh consulted = %v, want %v", ghCalled, tt.wantGH)
			}
		})
	}
}
