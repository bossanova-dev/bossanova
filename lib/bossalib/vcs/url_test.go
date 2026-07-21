package vcs

import (
	"fmt"
	"testing"
)

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
		{"nested namespace https", "https://gitlab.example/group/subgroup/repo-a.git", "https://gitlab.example/group/subgroup/repo-a"},
		{"nested namespace ssh", "git@gitlab.example:group/subgroup/repo-a.git", "https://gitlab.example/group/subgroup/repo-a"},

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
			// at == 0 on the *full-URL* user@ strip (url.go:143), reached via a
			// protocol prefix rather than SSH shorthand. "ssh://@host/..." has an
			// empty user, so after stripping "ssh://" the path starts with "@".
			// The SSH-shorthand branch (line 130) is skipped because the byte
			// after the first ":" is "/", so the user@ strip must happen at the
			// full-URL guard on line 143 — a path the SSH-shorthand cases above
			// (which strip at line 133) never reach with at == 0.
			// Catches CONDITIONALS_BOUNDARY at url.go:143: `at >= 0` → `at > 0`
			// would leave the leading "@" in the host ("@github.com").
			name:      "full url empty user leading @",
			originURL: "ssh://@github.com/owner/repo.git",
			prNumber:  3,
			want:      "https://github.com/owner/repo/pull/3",
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

// TestGitHubPullRequestURL_PRNumberBoundary exercises the prNumber guard inside
// the github provider's pullRequestURL closure (url.go:25, `prNumber <= 0`).
// PullRequestWebLink gates prNumber <= 0 before reaching the closure, so the
// closure's own boundary can only be hit by calling it directly.
//
// Catches CONDITIONALS_BOUNDARY (`<=` → `<`): at the exact boundary prNumber==0
// the closure must return "" (with `<` it would build a URL with /pull/0).
func TestGitHubPullRequestURL_PRNumberBoundary(t *testing.T) {
	provider := webLinkProviderForHost("github.com")
	if provider == nil {
		t.Fatal("expected a provider for github.com")
	}

	tests := []struct {
		name     string
		prNumber int
		want     string
	}{
		// Boundary: prNumber == 0 must yield "" (kills `<=` → `<`).
		{"zero pr", 0, ""},
		// Negative side: still "".
		{"negative pr", -1, ""},
		// Positive side: 1 is the smallest valid PR and must build a URL.
		{"smallest valid pr", 1, "https://github.com/owner/repo/pull/1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.pullRequestURL("github.com", "owner/repo", tt.prNumber)
			if got != tt.want {
				t.Errorf("pullRequestURL(owner/repo, %d) = %q, want %q", tt.prNumber, got, tt.want)
			}
		})
	}
}

// TestParseOriginURL_Boundaries targets the comparison boundaries in
// parseOriginURL that the higher-level helpers don't isolate.
func TestParseOriginURL_Boundaries(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		wantHost  string
		wantSlug  string
	}{
		// url.go:130 `idx > 0` (CONDITIONALS_BOUNDARY `>` → `>=`).
		// idx == 0: a leading colon must NOT be treated as the SSH separator.
		// With `>=` the SSH rewrite would fire on s[:0]="" and corrupt parsing.
		{"leading colon not SSH", ":owner/repo.git", "", ""},
		// Other side of url.go:130: idx > 0 (a real SSH shorthand) parses.
		{"ssh shorthand idx gt 0", "host:owner/repo.git", "host", "owner/repo"},

		// url.go:133 `at >= 0` inside the SSH-shorthand host (CONDITIONALS_BOUNDARY
		// `>=` → `>` and CONDITIONALS_NEGATION `>=` → `<`).
		// at == 0: leading "@" in the host must trigger the user@ strip.
		{"ssh shorthand leading at", "@host:owner/repo.git", "host", "owner/repo"},
		// Negation/other side: no "@" (at == -1) leaves the host untouched.
		{"ssh shorthand no at", "myhost:owner/repo.git", "myhost", "owner/repo"},
		// A *doubled* "@" in the SSH host pins url.go:133 on its own. The single
		// leading-"@" case above can't: the full-URL user@ strip on line 143 also
		// removes one leading "@", so a mutant that skips line 133 (`at > 0` or
		// `at < 0`) still looks correct there. With "@git@host" the host region
		// holds two "@": line 133 must strip the outer one so line 143 can strip
		// the inner "git@" and leave "host". A line-133 mutant leaves "@git@host",
		// line 143 strips only the outer "@", and the host parses as "git@host".
		{"ssh shorthand double at", "@git@host:owner/repo.git", "host", "owner/repo"},

		// url.go:143 first comparison `at >= 0` for full URLs after protocol
		// strip (CONDITIONALS_BOUNDARY `>=` → `>`).
		// After stripping "ssh://", s == "@host/owner/repo.git", at == 0: the
		// user@ strip must fire. With `>` the "@host" prefix would remain and
		// host would parse as "@host".
		{"full url leading at after protocol", "ssh://@host/owner/repo.git", "host", "owner/repo"},
		// Other side: full URL with a real user@ (at > 0) still strips.
		{"full url user at", "ssh://git@host/owner/repo.git", "host", "owner/repo"},

		// url.go:143 second comparison `at < indexOfFirstSlash`: a "@" that
		// appears in the PATH (after the first slash) must NOT be stripped,
		// because the host segment has no "@".
		{"at in path not stripped", "host/own@er/repo.git", "host", "own@er/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseOriginURL(%q) panicked: %v", tt.originURL, r)
				}
			}()
			gotHost, gotSlug := parseOriginURL(tt.originURL)
			if gotHost != tt.wantHost || gotSlug != tt.wantSlug {
				t.Errorf("parseOriginURL(%q) = (%q, %q), want (%q, %q)",
					tt.originURL, gotHost, gotSlug, tt.wantHost, tt.wantSlug)
			}
		})
	}
}

// TestParseOriginURLParts_RejectsLeadingColon pins the SSH-shorthand boundary
// before parseOriginURL discards the malformed host. A leading colon has no
// host, so the parts parser must reject it rather than returning partial data.
func TestParseOriginURLParts_RejectsLeadingColon(t *testing.T) {
	host, parts := parseOriginURLParts(":owner/repo.git")
	if host != "" {
		t.Errorf("parseOriginURLParts returned host %q, want empty", host)
	}
	if parts != nil {
		t.Errorf("parseOriginURLParts returned parts %q, want nil", parts)
	}
}

// TestPullRequestWebLink_PRNumberBoundary pins the url.go:110 boundary
// (`prNumber <= 0`, CONDITIONALS_BOUNDARY `<=` → `<`) at the helper level:
// prNumber==0 hides the link; prNumber==1 (smallest valid) exposes it.
func TestPullRequestWebLink_PRNumberBoundary(t *testing.T) {
	if _, _, ok := PullRequestWebLink("git@github.com:owner/repo.git", 0); ok {
		t.Errorf("PullRequestWebLink with prNumber=0 should not be ok")
	}
	provider, url, ok := PullRequestWebLink("git@github.com:owner/repo.git", 1)
	if !ok || provider != "github" || url != "https://github.com/owner/repo/pull/1" {
		t.Errorf("PullRequestWebLink with prNumber=1 = (%q, %q, %v), want (github, .../pull/1, true)",
			provider, url, ok)
	}
}

// TestPullRequestWebLink_RejectsZeroBeforeProvider ensures the helper owns its
// PR-number validation instead of relying on each provider to reject zero.
func TestPullRequestWebLink_RejectsZeroBeforeProvider(t *testing.T) {
	originalProviders := webLinkProviders
	webLinkProviders = []webLinkProvider{{
		name: "zero-friendly",
		matchesHost: func(host string) bool {
			return host == "example.test"
		},
		pullRequestURL: func(_ string, slug string, prNumber int) string {
			return fmt.Sprintf("https://example.test/%s/pulls/%d", slug, prNumber)
		},
	}}
	t.Cleanup(func() { webLinkProviders = originalProviders })

	provider, webURL, ok := PullRequestWebLink("https://example.test/owner/repo.git", 0)
	if provider != "" || webURL != "" || ok {
		t.Errorf("PullRequestWebLink with prNumber=0 = (%q, %q, %v), want empty values and false", provider, webURL, ok)
	}
}
