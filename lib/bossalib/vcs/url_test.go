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

		// A GitHub transport port is not part of the repository's address;
		// another host's port is kept.
		{"github ssh port", "ssh://git@github.com:22/acme/widgets.git", "https://github.com/acme/widgets"},
		{"github https port", "https://github.com:443/acme/widgets", "https://github.com/acme/widgets"},
		{"self-hosted https port", "https://git.company.com:8443/team/service", "https://git.company.com:8443/team/service"},

		// Unparseable inputs return "" so callers can filter.
		{"empty", "", ""},
		{"bare name", "foobar", ""},
		{"trailing colon no path", "git@host:", ""},
		{"leading colon not SSH", ":foo", ""},
		{"non-decimal port", "https://github.com:abc/acme/widgets", ""},
		{"query hides the real host", "https://evil.com?@github.com/acme/widgets", ""},
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

// CanonicalRepoURL is the value a globally unique index is keyed on, so what
// matters is that every spelling of one repository reaches the same string and
// that the string is a fixed point -- not merely that one pass ran.
func TestCanonicalRepoURL(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		want      string
	}{
		{"github mixed case", "git@GitHub.com:Acme/Alpha.git", "https://github.com/acme/alpha"},
		{"github ssh-over-443 host", "https://ssh.github.com:443/Acme/Alpha", "https://github.com/acme/alpha"},
		{"github ssh-over-443 ssh url", "ssh://git@SSH.GitHub.com:443/acme/widgets.git", "https://github.com/acme/widgets"},
		{"github other subdomain keeps host", "https://api.github.com:443/Acme/Alpha", "https://api.github.com/acme/alpha"},
		{"github ssh port", "ssh://git@github.com:22/acme/widgets.git", "https://github.com/acme/widgets"},
		{"github ssh port mixed case", "ssh://git@GitHub.com:22/Acme/Widgets.git", "https://github.com/acme/widgets"},
		{"github git+ssh port", "git+ssh://git@github.com:22/acme/widgets.git", "https://github.com/acme/widgets"},
		{"non github case", "https://gitlab.example/Group/Repo", "https://gitlab.example/Group/Repo"},
		// Spellings NormalizeRepoURL already settles in a single pass.
		{"github ssh shorthand", "git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"github https no .git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"github https trailing slash", "https://github.com/owner/repo/", "https://github.com/owner/repo"},

		// The reason this function exists: ".git" is stripped before the
		// trailing slash, so one pass leaves ".git" behind and a caller that
		// stopped there would key the same repository twice.
		{"dot-git before trailing slash", "https://github.com/owner/repo.git/", "https://github.com/owner/repo"},
		{"ssh shorthand with .git/", "git@github.com:owner/repo.git/", "https://github.com/owner/repo"},
		{"repeated trailing slashes", "https://github.com/owner/repo.git//", "https://github.com/owner/repo"},
		{"doubled .git suffix", "https://github.com/owner/repo.git.git", "https://github.com/owner/repo"},
		{"surrounding whitespace", "  https://github.com/owner/repo.git/  ", "https://github.com/owner/repo"},
		{"dot-git path segment", "https://github.com/owner/repo/.git", "https://github.com/owner/repo"},

		// No fixed point: report "" rather than a spelling this function does
		// not reproduce. The second case only collapses on the *second* pass,
		// which is the case a single-pass caller would have accepted.
		{"empty", "", ""},
		{"bare name", "foobar", ""},
		{"unparseable on the first pass", "https://github.com/owner/.git", ""},
		{"collapses to unparseable on a later pass", "https://github.com/owner/.git/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanonicalRepoURL(tt.originURL)
			if got != tt.want {
				t.Errorf("CanonicalRepoURL(%q) = %q, want %q", tt.originURL, got, tt.want)
			}
		})
	}
}

// The invariant the callers rely on: the result is a NormalizeRepoURL fixed
// point, so re-canonicalizing a stored value is a no-op and two independently
// derived spellings of one repository compare equal.
func TestCanonicalRepoURL_IsANormalizeFixedPoint(t *testing.T) {
	inputs := []string{
		"git@GitHub.com:Acme/Alpha.GIT",
		"git@GitHub.com:Acme/Alpha.git",
		"git@github.com:owner/repo.git",
		"https://github.com/owner/repo.git/",
		"https://github.com/owner/repo.git//",
		"ssh://git@git.company.com/team/service.git",
		"https://gitlab.example/group/subgroup/repo-a.git/",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			canonical := CanonicalRepoURL(in)
			if canonical == "" {
				t.Fatalf("CanonicalRepoURL(%q) did not converge", in)
			}
			if once := NormalizeRepoURL(canonical); once != canonical {
				t.Errorf("NormalizeRepoURL(%q) = %q, want the input unchanged", canonical, once)
			}
			if again := CanonicalRepoURL(canonical); again != canonical {
				t.Errorf("CanonicalRepoURL(%q) = %q, want the input unchanged", canonical, again)
			}
		})
	}
}

// bossd publishes NormalizeRepoURL in its repo snapshot and bosso keys
// webhook routing, triggers and organization mappings on it and on
// CanonicalRepoURL, so a parser change that moves any of these re-keys a
// repository. Every row up to the self-hosted ports is what the previous parser
// produced. The leading-slash rows are git's absolute-path spelling: the
// GitHub ones key like their slash-less form, and the self-hosted one replaced
// the previous parser's malformed "https://myserver:/srv/...". The one-letter
// alias rows are what the previous parser produced, the bracketed IPv6 scp row
// replaced its malformed "https://[2001/db8::1]:group/proj", and the
// ssh.github.com rows pin the alias's case and trailing-dot tolerance.
func TestRepoURLRoutingKeysAreStable(t *testing.T) {
	for _, tt := range []struct {
		origin, normalized, canonical string
	}{
		{"git@github.com:owner/repo.git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"git@GitHub.com:Acme/Alpha.git", "https://GitHub.com/Acme/Alpha", "https://github.com/acme/alpha"},
		{"https://github.com/owner/repo", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo.git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo/", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo.git/", "https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"https://GitHub.com/Acme/Alpha", "https://GitHub.com/Acme/Alpha", "https://github.com/acme/alpha"},
		{"https://user:token@github.com/owner/repo.git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"git://github.com/owner/repo.git", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"git@gitlab.com:group/subgroup/repo.git", "https://gitlab.com/group/subgroup/repo", "https://gitlab.com/group/subgroup/repo"},
		{"https://gitlab.example/Group/Sub/Repo.git", "https://gitlab.example/Group/Sub/Repo", "https://gitlab.example/Group/Sub/Repo"},
		{"git@git.company.com:team/service.git", "https://git.company.com/team/service", "https://git.company.com/team/service"},
		{"http://git.internal/team/service", "https://git.internal/team/service", "https://git.internal/team/service"},
		{"https://git.company.com:8443/team/service.git", "https://git.company.com:8443/team/service", "https://git.company.com:8443/team/service"},
		{"ssh://git@git.company.com:2222/team/service.git", "https://git.company.com:2222/team/service", "https://git.company.com:2222/team/service"},
		{"git@github.com:/o/r.git", "https://github.com/o/r", "https://github.com/o/r"},
		{"ssh://git@github.com:/o/r.git", "https://github.com/o/r", "https://github.com/o/r"},
		{"git@myserver:/srv/git/team/repo.git", "https://myserver/srv/git/team/repo", "https://myserver/srv/git/team/repo"},
		{"a:team/widgets.git", "https://a/team/widgets", "https://a/team/widgets"},
		{"git@a:team/widgets.git", "https://a/team/widgets", "https://a/team/widgets"},
		{"git@[2001:db8::1]:group/proj.git", "https://[2001:db8::1]/group/proj", "https://[2001:db8::1]/group/proj"},
		{"[2001:db8::1]:group/proj.git", "https://[2001:db8::1]/group/proj", "https://[2001:db8::1]/group/proj"},
		{"ssh://git@[2001:db8::1]/group/proj.git", "https://[2001:db8::1]/group/proj", "https://[2001:db8::1]/group/proj"},
		{"ssh://git@SSH.GitHub.com:443/acme/widgets.git", "https://github.com/acme/widgets", "https://github.com/acme/widgets"},
		{"ssh://git@ssh.github.com.:443/acme/widgets.git", "https://github.com/acme/widgets", "https://github.com/acme/widgets"},
		{"https://SSH.GitHub.com./acme/widgets", "https://github.com/acme/widgets", "https://github.com/acme/widgets"},
	} {
		t.Run(tt.origin, func(t *testing.T) {
			if got := NormalizeRepoURL(tt.origin); got != tt.normalized {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tt.origin, got, tt.normalized)
			}
			if got := CanonicalRepoURL(tt.origin); got != tt.canonical {
				t.Errorf("CanonicalRepoURL(%q) = %q, want %q", tt.origin, got, tt.canonical)
			}
		})
	}
}

// git reads "host:/path" as scp-like with an absolute path, so its path is
// taken literally, as the slash-less form's is, and never URL-parsed.
func TestParseRemote_SCPLikeAbsolutePathIsSCPLike(t *testing.T) {
	abs, okAbs := parseRemote("git@myserver:/srv/my repo#1.git")
	rel, okRel := parseRemote("git@myserver:srv/my repo#1.git")
	if !okAbs || !okRel || abs != rel {
		t.Fatalf("parseRemote absolute = %+v (%v), relative = %+v (%v), want equal", abs, okAbs, rel, okRel)
	}
}

// A routing key keeps the path's percent-encoding: decoding it would collapse
// "a%2Fb" into two segments and let a raw space into the key. The other rows
// pin what the parser does with spellings the previous parser keyed
// differently or not at all.
func TestCanonicalRepoURL_PathEncodingAndSpelling(t *testing.T) {
	for _, tt := range []struct {
		origin, want string
	}{
		{"https://git.example/group/my%20repo.git", "https://git.example/group/my%20repo"},
		{"https://git.example/group/my repo.git", "https://git.example/group/my%20repo"},
		{"git@git.example:group/my repo.git", "https://git.example/group/my%20repo"},
		{"https://git.example/group/a%2Fb.git", "https://git.example/group/a%2Fb"},
		{"https://git.example/group/bad%zz.git", ""},
		{"https://github.com/acme/widgets?x=1", "https://github.com/acme/widgets"},
		{"https://github.com/acme/widgets#frag", "https://github.com/acme/widgets"},
		{"HTTPS://github.com/acme/widgets", "https://github.com/acme/widgets"},
		{"SSH://git@github.com/acme/widgets.git", "https://github.com/acme/widgets"},
		{"\tgit@github.com:acme/widgets.git\n", "https://github.com/acme/widgets"},
		{"ssh+git://git@github.com/acme/widgets.git", "https://github.com/acme/widgets"},
		{"C:/Users/dev/widgets.git", ""},
		{"c:/Users/dev/widgets.git", ""},
		{"z:/src/repo", ""},
		{`c:\src\repo`, ""},
		{`C:\src/team/widgets.git`, ""},
		{"C:src/widgets.git", "https://C/src/widgets"},
		{"git@[not-an-ip]:group/proj.git", ""},
		{"git@[2001:db8::1%eth0]:group/proj.git", ""},
		{"git@[2001:db8::1]group/proj.git", ""},
		{"x[2001:db8::1]:group/proj.git", ""},
		{"../origin/widgets.git", ""},
	} {
		t.Run(tt.origin, func(t *testing.T) {
			if got := CanonicalRepoURL(tt.origin); got != tt.want {
				t.Errorf("CanonicalRepoURL(%q) = %q, want %q", tt.origin, got, tt.want)
			}
		})
	}
	if got := RepoSlug("https://git.example/group/a%2Fb.git"); got != "group/a%2Fb" {
		t.Errorf("RepoSlug kept %q, want the escaped segment group/a%%2Fb", got)
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
			name:         "github ssh-over-443 host",
			originURL:    "ssh://git@ssh.github.com:443/owner/repo.git",
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
			name:         "github ssh-over-443 host",
			originURL:    "ssh://git@ssh.github.com:443/owner/repo.git",
			prNumber:     7,
			wantProvider: "github",
			wantURL:      "https://github.com/owner/repo/pull/7",
			wantOK:       true,
		},
		{
			name:         "github scp leading slash",
			originURL:    "git@github.com:/owner/repo.git",
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
			// An empty userinfo in a scheme URL must not leave "@" in the host.
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

// TestParseOriginURL_Boundaries pins how parseOriginURL splits host from slug
// at the edges the higher-level helpers don't isolate.
func TestParseOriginURL_Boundaries(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		wantHost  string
		wantSlug  string
	}{
		{"leading colon not SSH", ":owner/repo.git", "", ""},
		{"ssh shorthand idx gt 0", "host:owner/repo.git", "host", "owner/repo"},
		{"ssh shorthand leading at", "@host:owner/repo.git", "host", "owner/repo"},
		{"ssh shorthand no at", "myhost:owner/repo.git", "myhost", "owner/repo"},
		// Userinfo runs to the last "@", as it does for ssh itself.
		{"ssh shorthand double at", "@git@host:owner/repo.git", "host", "owner/repo"},
		{"full url leading at after protocol", "ssh://@host/owner/repo.git", "host", "owner/repo"},
		{"full url user at", "ssh://git@host/owner/repo.git", "host", "owner/repo"},
		{"at in path not stripped", "host/own@er/repo.git", "host", "own@er/repo"},

		{"github ssh port dropped", "ssh://git@github.com:22/acme/widgets.git", "github.com", "acme/widgets"},
		{"github https port dropped", "https://github.com:443/acme/widgets", "github.com", "acme/widgets"},
		{"non-github https port kept", "https://git.company.com:8443/team/service", "git.company.com:8443", "team/service"},
		{"ipv6 host keeps brackets", "https://[::1]:8443/team/service", "[::1]:8443", "team/service"},
		{"non-decimal port", "https://github.com:abc/acme/widgets", "", ""},
		{"empty port is no port", "https://github.com:/acme/widgets", "github.com", "acme/widgets"},
		{"self-hosted empty port is no port", "ssh://git@myserver:/srv/repo.git", "myserver", "srv/repo"},
		{"scp absolute path", "git@myserver:/srv/team/repo.git", "myserver", "srv/team"},
		{"scp url-like is not scp", "git@host://owner/repo.git", "", ""},
		{"ssh-over-443 host reads as github.com", "ssh://git@ssh.github.com:443/acme/widgets.git", "github.com", "acme/widgets"},
		{"windows drive path", "C:/Users/dev/widgets.git", "", ""},
		{"windows drive backslash path", `C:\Users\dev\widgets.git`, "", ""},
		{"lowercase drive path", "c:/Users/dev/widgets.git", "", ""},
		{"lowercase drive backslash path", `c:\Users\dev\widgets.git`, "", ""},
		{"one-letter scp host", "a:team/widgets.git", "a", "team/widgets"},
		{"ipv6 scp host", "git@[2001:db8::1]:group/proj.git", "[2001:db8::1]", "group/proj"},
		{"ssh-over-443 host in capitals", "ssh://git@SSH.GitHub.com:443/acme/widgets.git", "github.com", "acme/widgets"},
		{"ssh-over-443 host with trailing dot", "ssh://git@ssh.github.com.:443/acme/widgets.git", "github.com", "acme/widgets"},
		{"leading dot host", "https://.example.com/acme/widgets", "", ""},
		{"empty label host", "https://a..example.com/acme/widgets", "", ""},
		{"unknown scheme", "file:///srv/acme/widgets", "", ""},
		{"query before at names the host", "https://evil.com?@github.com/acme/widgets", "", ""},
		{"fragment before at names the host", "https://evil.com#@github.com/acme/widgets", "", ""},
		{"backslash before at", `https://evil.com\@github.com/acme/widgets`, "", ""},
		{"query and fragment dropped", "https://example.com/acme/widgets?x=1#y", "example.com", "acme/widgets"},
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

func TestIsGitHubHost(t *testing.T) {
	for _, tt := range []struct {
		host string
		want bool
	}{
		{"github.com", true}, {"GitHub.COM", true}, {"api.github.com", true}, {"github.com:443", true}, {"github.com.", true}, {"github.com.:443", true},
		{"gitlab.com", false}, {"notgithub.com", false}, {"github.com.evil.example", false}, {"evilgithub.com", false}, {"", false},
		{".github.com", false}, {"a..github.com", false}, {"github.com:abc", false}, {"github.com:", false},
	} {
		t.Run(tt.host, func(t *testing.T) {
			if got := IsGitHubHost(tt.host); got != tt.want {
				t.Fatalf("IsGitHubHost(%q)=%v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
