package vcs

import "testing"

func TestConstructPRURL(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		prNumber  int
		want      string
	}{
		{"SSH format", "git@github.com:owner/repo.git", 42, "https://github.com/owner/repo/pull/42"},
		{"HTTPS format", "https://github.com/owner/repo.git", 7, "https://github.com/owner/repo/pull/7"},
		{"HTTPS no .git suffix", "https://github.com/owner/repo", 1, "https://github.com/owner/repo/pull/1"},
		{"empty URL", "", 1, ""},
		{"bare path no slash", "foobar", 1, ""},
		{"git protocol", "git://github.com/owner/repo.git", 5, "https://github.com/owner/repo/pull/5"},
		{"git protocol no .git", "git://github.com/owner/repo", 3, "https://github.com/owner/repo/pull/3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConstructPRURL(tt.originURL, tt.prNumber)
			if got != tt.want {
				t.Errorf("ConstructPRURL(%q, %d) = %q, want %q", tt.originURL, tt.prNumber, got, tt.want)
			}
		})
	}
}

func TestRepoSlug(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		want      string
	}{
		{"SSH format", "git@github.com:owner/repo.git", "owner/repo"},
		{"HTTPS format", "https://github.com/owner/repo.git", "owner/repo"},
		{"HTTPS no .git suffix", "https://github.com/owner/repo", "owner/repo"},
		{"git protocol", "git://github.com/owner/repo.git", "owner/repo"},
		{"git protocol no .git", "git://github.com/owner/repo", "owner/repo"},
		{"ssh:// protocol", "ssh://git@github.com/owner/repo.git", "owner/repo"},
		{"empty URL", "", ""},
		{"bare path no slash", "foobar", ""},
		{"trailing colon no path", "git@host:", ""},
		{"leading colon is not SSH", ":foo", ""},
		{"leading @ at index 0", "@host:owner/repo.git", "owner/repo"},
		{"ssh no user prefix", "host:owner/repo.git", "owner/repo"},
		{"trailing slash", "https://github.com/owner/repo/", "owner/repo"},
		{"extra path segments ignored", "https://github.com/owner/repo/extra", "owner/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RepoSlug(%q) panicked: %v", tt.originURL, r)
				}
			}()
			got := RepoSlug(tt.originURL)
			if got != tt.want {
				t.Errorf("RepoSlug(%q) = %q, want %q", tt.originURL, got, tt.want)
			}
		})
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		want      string
	}{
		// GitHub: SSH shorthand, ssh://, https — all collapse to canonical form.
		{"github ssh shorthand", "git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"github ssh url", "ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"github https with .git", "https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"github https no .git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"github https trailing slash", "https://github.com/owner/repo/", "https://github.com/owner/repo"},
		{"github git protocol", "git://github.com/owner/repo.git", "https://github.com/owner/repo"},

		// GitLab.com (host-agnostic — works without provider registration).
		{"gitlab ssh shorthand", "git@gitlab.com:owner/repo.git", "https://gitlab.com/owner/repo"},
		{"gitlab https", "https://gitlab.com/owner/repo", "https://gitlab.com/owner/repo"},

		// Self-hosted instances. The whole point of this helper: no
		// provider registry, so a private GitLab/Gitea on a custom host
		// still normalizes to the same form the matching webhook will
		// carry.
		{"self-hosted ssh", "git@git.company.com:team/service.git", "https://git.company.com/team/service"},
		{"self-hosted https", "https://git.company.com/team/service", "https://git.company.com/team/service"},
		{"self-hosted ssh:// with port-less host", "ssh://git@git.company.com/team/service.git", "https://git.company.com/team/service"},

		// Unparseable inputs return "" so callers can filter.
		{"empty", "", ""},
		{"bare name", "foobar", ""},
		{"trailing colon no path", "git@host:", ""},
		{"leading colon not SSH", ":foo", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeRepoURL(tt.originURL)
			if got != tt.want {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tt.originURL, got, tt.want)
			}
		})
	}
}

// TestNormalizeRepoURL_Idempotent confirms canonical output is a fixed
// point — feeding the result back through the normalizer yields the
// same string. This is the contract callers rely on when they put the
// snapshot side and the webhook side through the same helper.
func TestNormalizeRepoURL_Idempotent(t *testing.T) {
	inputs := []string{
		"git@github.com:owner/repo.git",
		"https://github.com/owner/repo",
		"ssh://git@git.company.com/team/service.git",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			once := NormalizeRepoURL(in)
			twice := NormalizeRepoURL(once)
			if once != twice {
				t.Errorf("NormalizeRepoURL not idempotent: first=%q second=%q", once, twice)
			}
		})
	}
}

func TestRepoWebLink(t *testing.T) {
	tests := []struct {
		name         string
		originURL    string
		wantProvider string
		wantURL      string
		wantOK       bool
	}{
		{
			name:         "github https",
			originURL:    "https://github.com/owner/repo.git",
			wantProvider: "github",
			wantURL:      "https://github.com/owner/repo",
			wantOK:       true,
		},
		{
			name:         "github ssh shorthand",
			originURL:    "git@github.com:owner/repo.git",
			wantProvider: "github",
			wantURL:      "https://github.com/owner/repo",
			wantOK:       true,
		},
		{
			name:         "github ssh url",
			originURL:    "ssh://git@github.com/owner/repo.git",
			wantProvider: "github",
			wantURL:      "https://github.com/owner/repo",
			wantOK:       true,
		},
		{
			name:      "gitlab hidden for now",
			originURL: "git@gitlab.com:owner/repo.git",
			wantOK:    false,
		},
		{
			name:      "malformed hidden",
			originURL: "not-a-repo",
			wantOK:    false,
		},
		{
			name:      "empty hidden",
			originURL: "",
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProvider, gotURL, gotOK := RepoWebLink(tt.originURL)
			if gotOK != tt.wantOK {
				t.Fatalf("RepoWebLink(%q) ok = %v, want %v", tt.originURL, gotOK, tt.wantOK)
			}
			if gotProvider != tt.wantProvider || gotURL != tt.wantURL {
				t.Errorf("RepoWebLink(%q) = (%q, %q), want (%q, %q)",
					tt.originURL, gotProvider, gotURL, tt.wantProvider, tt.wantURL)
			}
		})
	}
}

func TestPullRequestWebLink(t *testing.T) {
	tests := []struct {
		name         string
		originURL    string
		prNumber     int
		wantProvider string
		wantURL      string
		wantOK       bool
	}{
		{
			name:         "github https",
			originURL:    "https://github.com/owner/repo.git",
			prNumber:     42,
			wantProvider: "github",
			wantURL:      "https://github.com/owner/repo/pull/42",
			wantOK:       true,
		},
		{
			name:         "github ssh shorthand",
			originURL:    "git@github.com:owner/repo.git",
			prNumber:     7,
			wantProvider: "github",
			wantURL:      "https://github.com/owner/repo/pull/7",
			wantOK:       true,
		},
		{
			name:      "zero pr hidden",
			originURL: "git@github.com:owner/repo.git",
			prNumber:  0,
			wantOK:    false,
		},
		{
			name:      "gitlab hidden for now",
			originURL: "git@gitlab.com:owner/repo.git",
			prNumber:  42,
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProvider, gotURL, gotOK := PullRequestWebLink(tt.originURL, tt.prNumber)
			if gotOK != tt.wantOK {
				t.Fatalf("PullRequestWebLink(%q, %d) ok = %v, want %v", tt.originURL, tt.prNumber, gotOK, tt.wantOK)
			}
			if gotProvider != tt.wantProvider || gotURL != tt.wantURL {
				t.Errorf("PullRequestWebLink(%q, %d) = (%q, %q), want (%q, %q)",
					tt.originURL, tt.prNumber, gotProvider, gotURL, tt.wantProvider, tt.wantURL)
			}
		})
	}
}

// TestConstructPRURL_Boundaries covers boundary mutations on the SSH detection
// (`idx > 0`, `idx+1 >= len(s)`) and the user@ stripping (`at >= 0`).
func TestConstructPRURL_Boundaries(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		prNumber  int
		want      string
	}{
		{
			// idx == 0: leading colon must NOT be treated as SSH separator.
			// Catches mutation: idx > 0 → idx >= 0.
			name:      "leading colon is not SSH",
			originURL: ":foo",
			prNumber:  1,
			want:      "",
		},
		{
			// idx+1 == len(s): trailing colon means no path; must not panic
			// on s[idx+1] out-of-bounds.
			// Catches mutation: idx+1 >= len(s) → idx+1 > len(s).
			name:      "trailing colon no path",
			originURL: "git@host:",
			prNumber:  1,
			want:      "",
		},
		{
			// at == 0: leading "@" before host. The user@ strip must trigger
			// (at >= 0), turning "@host" into "host".
			// Catches mutation: at >= 0 → at > 0.
			name:      "leading @ at index 0",
			originURL: "@host:owner/repo.git",
			prNumber:  9,
			want:      "https://host/owner/repo/pull/9",
		},
		{
			// SSH with no user prefix (no '@'): at == -1, user@ strip skipped.
			name:      "ssh no user prefix",
			originURL: "host:owner/repo.git",
			prNumber:  4,
			want:      "https://host/owner/repo/pull/4",
		},
		{
			// "https://" — the colon is at idx=5, but s[6]='/', so SSH branch
			// must NOT be taken. This ensures the `s[idx+1] != '/'` guard
			// works; without it, we'd misparse https URLs as SSH.
			name:      "https not misdetected as SSH",
			originURL: "https://github.com/owner/repo.git",
			prNumber:  2,
			want:      "https://github.com/owner/repo/pull/2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ConstructPRURL(%q, %d) panicked: %v", tt.originURL, tt.prNumber, r)
				}
			}()
			got := ConstructPRURL(tt.originURL, tt.prNumber)
			if got != tt.want {
				t.Errorf("ConstructPRURL(%q, %d) = %q, want %q", tt.originURL, tt.prNumber, got, tt.want)
			}
		})
	}
}
