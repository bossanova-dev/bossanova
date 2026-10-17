package vcs

import (
	"strings"
	"testing"
)

func TestIsGitHubURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"HTTPS", "https://github.com/owner/repo.git", true},
		{"SSH", "git@github.com:owner/repo.git", true},
		{"HTTPS no .git", "https://github.com/owner/repo", true},
		{"mixed case", "https://GitHub.COM/owner/repo.git", true},
		{"with whitespace", "  https://github.com/owner/repo.git  ", true},
		{"GitLab HTTPS", "https://gitlab.com/owner/repo.git", false},
		{"GitLab SSH", "git@gitlab.com:owner/repo.git", false},
		{"empty string", "", false},
		{"bare path", "/some/local/path", false},
		{"bitbucket", "https://bitbucket.org/owner/repo.git", false},
		{"ssh scheme with port", "ssh://git@github.com:22/owner/repo.git", true},
		{"suffix look-alike", "https://evilgithub.com/owner/repo", false},
		{"prefix look-alike", "https://github.com.evil.com/owner/repo", false},
		{"github in path", "https://evil.com/github.com/owner/repo", false},
		{"query then at", "https://evil.com?@github.com/owner/repo", false},
		{"bare look-alike", "evilgithub.com/owner/repo", false},
		{"scp leading slash", "git@github.com:/owner/repo.git", true},
		{"ssh empty port leading slash", "ssh://git@github.com:/owner/repo.git", true},
		{"ssh over 443 host", "ssh://git@ssh.github.com:443/owner/repo.git", true},
		{"windows drive path", "C:/Users/dev/github.com/repo.git", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsGitHubURL(tt.url)
			if got != tt.want {
				t.Errorf("IsGitHubURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestGitHubNWO(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"SSH with .git", "git@github.com:owner/repo.git", "owner/repo"},
		{"HTTPS with .git", "https://github.com/owner/repo.git", "owner/repo"},
		{"HTTPS no .git", "https://github.com/owner/repo", "owner/repo"},
		{"with whitespace", "  https://github.com/owner/repo.git  ", "owner/repo"},
		{"extra path segments", "https://github.com/owner/repo/tree/main", "owner/repo"},
		{"SSH extra segments", "git@github.com:owner/repo/extra", "owner/repo"},
		{"owner only", "https://github.com/owner", ""},
		{"non-github", "https://gitlab.com/owner/repo.git", ""},
		{"empty", "", ""},

		{"ssh scheme with port", "ssh://git@github.com:22/acme/widgets.git", "acme/widgets"},
		{"ssh scheme with port no .git", "ssh://git@github.com:22/acme/widgets", "acme/widgets"},
		{"ssh scheme no port", "ssh://git@github.com/acme/widgets", "acme/widgets"},
		{"ssh scheme no port with .git", "ssh://git@github.com/acme/widgets.git", "acme/widgets"},
		{"ssh over 443 host", "ssh://git@ssh.github.com:443/acme/widgets.git", "acme/widgets"},
		{"scp leading slash", "git@github.com:/acme/widgets.git", "acme/widgets"},
		{"ssh empty port leading slash", "ssh://git@github.com:/acme/widgets.git", "acme/widgets"},
		{"git+ssh scheme", "git+ssh://git@github.com/acme/widgets.git", "acme/widgets"},
		{"git+ssh scheme with port", "git+ssh://git@github.com:22/acme/widgets.git", "acme/widgets"},
		{"https userinfo token", "https://user:token@github.com/acme/widgets.git", "acme/widgets"},
		{"https userinfo token with port", "https://user:token@github.com:443/acme/widgets", "acme/widgets"},
		{"https with port", "https://github.com:443/acme/widgets", "acme/widgets"},
		{"https with port and .git", "https://github.com:443/acme/widgets.git", "acme/widgets"},
		{"trailing slash", "https://github.com/acme/widgets/", "acme/widgets"},
		{"trailing slash after .git", "https://github.com/acme/widgets.git/", "acme/widgets"},
		{"ssh port trailing slash", "ssh://git@github.com:22/acme/widgets.git/", "acme/widgets"},
		{"query and fragment", "https://github.com/acme/widgets?tab=readme#top", "acme/widgets"},
		{"uppercase host https", "https://GitHub.COM/acme/widgets.git", "acme/widgets"},
		{"uppercase host ssh scheme with port", "ssh://git@GITHUB.COM:22/acme/widgets.git", "acme/widgets"},
		{"uppercase host scp", "git@GitHub.com:acme/widgets.git", "acme/widgets"},
		{"owner case preserved", "ssh://git@github.com:22/Acme/Widgets.git", "Acme/Widgets"},
		{"scp owner that looks like a port", "git@github.com:22/widgets.git", "22/widgets"},
		{"ssh scheme port without repo", "ssh://git@github.com:22/acme", ""},
		{"ssh scheme port only", "ssh://git@github.com:22/", ""},
		{"non-github ssh scheme with port", "ssh://git@gitlab.com:22/acme/widgets.git", ""},
		{"non-github host naming github in path", "ssh://git@gitlab.com:22/github.com/widgets.git", ""},
		{"non-github https with port", "https://gitlab.com:443/acme/widgets", ""},
		{"trailing dot host", "https://github.com./acme/widgets", "acme/widgets"},
		{"bare host path", "github.com/acme/widgets", "acme/widgets"},
		{"scp no user", "github.com:acme/widgets.git", "acme/widgets"},

		// Look-alike hosts: GitHub must be the whole host, not a substring of it.
		{"suffix look-alike", "https://evilgithub.com/acme/widgets", ""},
		{"prefix look-alike", "https://github.com.evil.com/acme/widgets", ""},
		{"github as userinfo", "https://user@github.com@evil.com/acme/widgets", ""},
		{"github subdomain-shaped host", "https://github.company.com/acme/widgets", ""},
		{"github in path", "https://evil.com/github.com/acme/widgets", ""},
		{"file scheme", "file:///github.com/acme/widgets", ""},

		// The authority ends at the first "/", "?" or "#", so text after them
		// cannot name the host.
		{"query then at", "https://evil.com?@github.com/acme/widgets", ""},
		{"fragment then at", "https://evil.com#@github.com/acme/widgets", ""},
		{"backslash then at", `https://evil.com\@github.com/acme/widgets`, ""},

		// Without a scheme the host still has to match exactly.
		{"bare look-alike", "evilgithub.com/acme/widgets", ""},
		{"scp look-alike", "git@evilgithub.com:acme/widgets.git", ""},
		{"bare github in path", "evil.com/github.com/acme/widgets", ""},

		{"non-decimal port", "https://github.com:abc/acme/widgets", ""},
		{"empty port is no port", "https://github.com:/acme/widgets", "acme/widgets"},
		{"leading dot host", "https://.github.com/acme/widgets", ""},
		{"bare leading dot host", ".github.com/acme/widgets", ""},
		{"scp leading dot host", "git@.github.com:acme/widgets.git", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GitHubNWO(tt.url)
			if got != tt.want {
				t.Errorf("GitHubNWO(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestNormalizeHeadSHA(t *testing.T) {
	lower := strings.Repeat("ab12", 10)
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"lowercase 40 hex", lower, lower, true},
		{"uppercase normalized", strings.ToUpper(lower), lower, true},
		{"surrounding whitespace trimmed", "  " + lower + "\n", lower, true},
		{"empty", "", "", false},
		{"short", "abc", "", false},
		{"39 hex", lower[:39], "", false},
		{"41 hex", lower + "a", "", false},
		{"non-hex", strings.Repeat("g", 40), "", false},
		{"inner space", lower[:20] + " " + lower[21:], "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeHeadSHA(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("NormalizeHeadSHA(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseGitHubCheckState(t *testing.T) {
	tests := []struct {
		input          string
		wantStatus     CheckStatus
		wantConclusion CheckConclusion // 0 means nil
		wantRecognized bool
	}{
		{"success", CheckStatusCompleted, CheckConclusionSuccess, true},
		{"SUCCESS", CheckStatusCompleted, CheckConclusionSuccess, true},
		{"failure", CheckStatusCompleted, CheckConclusionFailure, true},
		{"error", CheckStatusCompleted, CheckConclusionFailure, true},
		{"action_required", CheckStatusCompleted, CheckConclusionFailure, true},
		{"stale", CheckStatusCompleted, CheckConclusionFailure, true},
		{"startup_failure", CheckStatusCompleted, CheckConclusionFailure, true},
		{"neutral", CheckStatusCompleted, CheckConclusionNeutral, true},
		{"cancelled", CheckStatusCompleted, CheckConclusionCancelled, true},
		{"skipped", CheckStatusCompleted, CheckConclusionSkipped, true},
		{"timed_out", CheckStatusCompleted, CheckConclusionTimedOut, true},
		{"in_progress", CheckStatusInProgress, 0, true},
		{"queued", CheckStatusQueued, 0, true},
		{"pending", CheckStatusQueued, 0, true},
		{"waiting", CheckStatusQueued, 0, true},
		{"requested", CheckStatusCompleted, 0, false},
		{"", CheckStatusCompleted, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			status, conclusion, recognized := ParseGitHubCheckState(tt.input)
			if status != tt.wantStatus || recognized != tt.wantRecognized {
				t.Fatalf("ParseGitHubCheckState(%q) = %v, recognized %v; want %v, %v", tt.input, status, recognized, tt.wantStatus, tt.wantRecognized)
			}
			switch {
			case tt.wantConclusion == 0 && conclusion != nil:
				t.Fatalf("conclusion = %v, want nil", *conclusion)
			case tt.wantConclusion != 0 && (conclusion == nil || *conclusion != tt.wantConclusion):
				t.Fatalf("conclusion = %v, want %v", conclusion, tt.wantConclusion)
			}
		})
	}
}

func TestIsTrustedReviewAuthor(t *testing.T) {
	tests := []struct {
		name        string
		login       string
		association string
		want        bool
	}{
		{"owner", "maintainer", "OWNER", true},
		{"member", "teammate", "MEMBER", true},
		{"collaborator", "helper", "COLLABORATOR", true},
		{"lowercase association", "teammate", "member", true},
		{"padded association", "teammate", " MEMBER ", true},
		{"contributor", "drive-by", "CONTRIBUTOR", false},
		{"first-time contributor", "drive-by", "FIRST_TIME_CONTRIBUTOR", false},
		{"first-timer", "drive-by", "FIRST_TIMER", false},
		{"mannequin", "ghost", "MANNEQUIN", false},
		{"none", "drive-by", "NONE", false},
		{"missing association", "drive-by", "", false},
		{"bot with no association", "chatgpt-codex-connector[bot]", "NONE", true},
		{"bot with empty association", "cursor[bot]", "", true},
		{"bare bot login is not a bot", "chatgpt-codex-connector", "NONE", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTrustedReviewAuthor(tt.login, tt.association); got != tt.want {
				t.Errorf("IsTrustedReviewAuthor(%q, %q) = %v, want %v", tt.login, tt.association, got, tt.want)
			}
		})
	}
}

func TestIsTrustedAuthorAssociation_IgnoresBotLogins(t *testing.T) {
	// The association predicate alone never trusts NONE, whoever posted it;
	// only IsTrustedReviewAuthor adds the bot-login escape.
	if IsTrustedAuthorAssociation("NONE") {
		t.Fatal("IsTrustedAuthorAssociation(NONE) = true, want false")
	}
	if !IsTrustedAuthorAssociation("OWNER") {
		t.Fatal("IsTrustedAuthorAssociation(OWNER) = false, want true")
	}
}
