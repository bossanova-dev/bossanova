package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/ghtoken"
	"github.com/rs/zerolog"
)

type readCredentialSource struct {
	credential ghtoken.Credential
	calls      []string
}

func (s *readCredentialSource) Credential(_ context.Context, origin string) ghtoken.Credential {
	s.calls = append(s.calls, origin)
	return s.credential
}

type readCredentialIssuer struct{}

func (readCredentialIssuer) IssueInstallationToken(context.Context, string, string) (ghtoken.MintedToken, error) {
	return ghtoken.MintedToken{Token: "ghs_read-credential-test-secret", InstallationID: 42, Permissions: map[string]string{"contents": "read", "statuses": "read"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func TestGetGitHubReadCredential_GitHubUsage_AC232201(t *testing.T) {
	source := ghtoken.New(ghtoken.Config{Client: readCredentialIssuer{}, Tokens: fixedDaemonSessionToken("signed-in")})
	credential := source.Credential(context.Background(), "git@github.com:acme/app.git")
	if credential.Token() == "" {
		t.Fatal("test issuer did not mint a credential")
	}
	tests := []struct {
		name      string
		args      []string
		wd        string
		modify    func(*ghtoken.Credential)
		nilSource bool
		reason    string
		calls     int
	}{
		{name: "installation", args: []string{"pr", "view", "1", "-R", "ACME/app"}, reason: "installation", calls: 1},
		{name: "checkout", args: []string{"run", "list"}, wd: "/checkout/subdir", reason: "installation", calls: 1},
		{name: "session worktree", args: []string{"pr", "checks", "1"}, wd: "/worktrees/session/subdir", reason: "installation", calls: 1},
		{name: "write permission", args: []string{"pr", "list", "-R", "acme/app"}, modify: func(c *ghtoken.Credential) { c.Permissions = map[string]string{"statuses": "write"} }, reason: "write_permission", calls: 1},
		{name: "empty permissions", args: []string{"pr", "list", "-R", "acme/app"}, modify: func(c *ghtoken.Credential) { c.Permissions = nil }, reason: "write_permission", calls: 1},
		{name: "unknown permission", args: []string{"pr", "list", "-R", "acme/app"}, modify: func(c *ghtoken.Credential) { c.Permissions = map[string]string{"contents": "admin"} }, reason: "write_permission", calls: 1},
		{name: "user identity", args: []string{"pr", "list", "-R", "acme/app"}, modify: func(c *ghtoken.Credential) {
			c.Identity = ghtoken.IdentityUser
			c.Reason = ghtoken.ReasonNoInstallation
		}, reason: "no_installation", calls: 1},
		{name: "expiring", args: []string{"pr", "list", "-R", "acme/app"}, modify: func(c *ghtoken.Credential) { c.ExpiresAt = time.Now().Add(30 * time.Second) }, reason: "expiring", calls: 1},
		{name: "write", args: []string{"pr", "merge", "1"}, reason: "not_read"},
		{name: "non repository", args: []string{"api", "user"}, reason: "not_single_repo"},
		{name: "unknown repository", args: []string{"pr", "list", "-R", "acme/unknown"}, reason: "unregistered_repo"},
		{name: "outside workspace", args: []string{"run", "list"}, wd: "/elsewhere", reason: "unregistered_repo"},
		{name: "empty workspace", args: []string{"run", "list"}, reason: "unregistered_repo"},
		{name: "nil source", args: []string{"pr", "list", "-R", "acme/app"}, nilSource: true, reason: "not_signed_in"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := credential
			if tc.modify != nil {
				tc.modify(&c)
			}
			fake := &readCredentialSource{credential: c}
			var logs bytes.Buffer
			srv := New(Config{GitHubReadCredentials: fake, Logger: zerolog.New(&logs)})
			srv.repos = prStateRepos{repos: []*domain.Repo{{ID: "repo", OriginURL: "git@github.com:acme/app.git", LocalPath: "/checkout"}}}
			srv.sessions = evidenceSessionList{session: &domain.Session{RepoID: "repo", WorktreePath: "/worktrees/session"}}
			if tc.nilSource {
				srv.gitHubReadCredentials = nil
			}
			response, err := srv.GetGitHubReadCredential(context.Background(), connect.NewRequest(&pb.GetGitHubReadCredentialRequest{GhArgs: tc.args, WorkingDirectory: tc.wd}))
			if err != nil {
				t.Fatal(err)
			}
			if response.Msg.Reason != tc.reason {
				t.Errorf("reason=%q, want %q", response.Msg.Reason, tc.reason)
			}
			if tc.reason == "installation" {
				if response.Msg.Token != credential.Token() || response.Msg.InstallationId != 42 {
					t.Fatal("installation response lacks the issued credential")
				}
			} else if response.Msg.Token != "" || response.Msg.InstallationId != 0 {
				t.Fatal("fallback response carries an installation credential")
			}
			if len(fake.calls) != tc.calls {
				t.Errorf("source called %d times, want %d", len(fake.calls), tc.calls)
			}
			for _, origin := range fake.calls {
				if origin != "git@github.com:acme/app.git" {
					t.Errorf("source origin=%q", origin)
				}
			}
			if strings.Contains(logs.String(), credential.Token()) {
				t.Fatal("handler logged credential")
			}
		})
	}
}
