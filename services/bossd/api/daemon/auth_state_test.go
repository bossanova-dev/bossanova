package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/ghtoken"
)

// stubAuthStateReporter hands GetAuthState a fixed answer so the test pins the
// mapping from daemon state to wire fields, not the daemon that produces it.
type stubAuthStateReporter struct {
	state  DaemonAuthState
	called int
}

func (s *stubAuthStateReporter) AuthState(context.Context) DaemonAuthState {
	s.called++
	return s.state
}

func TestGetAuthStateReportsReporterValuesVerbatim(t *testing.T) {
	failingSince := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	registeredAt := time.Date(2026, 8, 19, 9, 15, 0, 0, time.UTC)
	reporter := &stubAuthStateReporter{state: DaemonAuthState{
		NeedsLogin:       true,
		UpstreamOrigin:   "https://orchestrator-staging.bossanova.dev",
		ReloginReason:    "refresh_outcome_unknown",
		LastRegisteredAt: registeredAt,
		Connected:        false,
		AuthFailingSince: failingSince,
	}}
	s := New(Config{AuthStateReporter: reporter})

	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v", err)
	}
	if reporter.called != 1 {
		t.Fatalf("reporter called %d times, want 1", reporter.called)
	}
	msg := resp.Msg
	if got := msg.GetUpstreamOrigin(); got != reporter.state.UpstreamOrigin {
		t.Errorf("upstream_origin = %q, want %q", got, reporter.state.UpstreamOrigin)
	}
	if !msg.GetUpstreamConfigured() {
		t.Error("upstream_configured = false, want true when a reporter is wired")
	}
	if !msg.GetNeedsLogin() {
		t.Error("needs_login = false, want true")
	}
	if got := msg.GetReloginReason(); got != "refresh_outcome_unknown" {
		t.Errorf("relogin_reason = %q, want %q", got, "refresh_outcome_unknown")
	}
	if msg.GetUpstreamConnected() {
		t.Error("upstream_connected = true, want false")
	}
	if got := msg.GetLastRegisteredAt().AsTime(); !got.Equal(registeredAt) {
		t.Errorf("last_registered_at = %v, want %v", got, registeredAt)
	}
	if got := msg.GetAuthFailingSince().AsTime(); !got.Equal(failingSince) {
		t.Errorf("auth_failing_since = %v, want %v", got, failingSince)
	}
}

// A healthy daemon must leave both timestamps unset rather than encoding the
// zero instant: the CLI renders "never" for unset and a 1970 date for a zero
// timestamp, and those are different sentences.
func TestGetAuthStateLeavesZeroTimestampsUnset(t *testing.T) {
	reporter := &stubAuthStateReporter{state: DaemonAuthState{Connected: true}}
	s := New(Config{AuthStateReporter: reporter})

	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v", err)
	}
	if resp.Msg.GetLastRegisteredAt() != nil {
		t.Errorf("last_registered_at = %v, want unset", resp.Msg.GetLastRegisteredAt())
	}
	if resp.Msg.GetAuthFailingSince() != nil {
		t.Errorf("auth_failing_since = %v, want unset", resp.Msg.GetAuthFailingSince())
	}
	if !resp.Msg.GetUpstreamConnected() {
		t.Error("upstream_connected = false, want true")
	}
}

// A local-only daemon has no upstream at all. That is a real answer, not a
// failure to produce one: nil error, upstream_configured=false, nothing else
// set. A caller that saw an error here would report a broken daemon.
func TestGetAuthStateNilReporterReportsUnconfigured(t *testing.T) {
	s := New(Config{})

	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v, want nil", err)
	}
	msg := resp.Msg
	if msg.GetUpstreamConfigured() {
		t.Error("upstream_configured = true, want false for a local-only daemon")
	}
	if msg.GetNeedsLogin() {
		t.Error("needs_login = true, want false")
	}
	if msg.GetReloginReason() != "" {
		t.Errorf("relogin_reason = %q, want empty", msg.GetReloginReason())
	}
	if msg.GetUpstreamConnected() {
		t.Error("upstream_connected = true, want false")
	}
	if msg.GetLastRegisteredAt() != nil {
		t.Errorf("last_registered_at = %v, want unset", msg.GetLastRegisteredAt())
	}
	if msg.GetAuthFailingSince() != nil {
		t.Errorf("auth_failing_since = %v, want unset", msg.GetAuthFailingSince())
	}
}

// stubGitHubCredentialReporter hands GetAuthState a fixed credential report.
type stubGitHubCredentialReporter struct {
	states []ghtoken.RepoCredentialState
	err    error
	called int
}

func (s *stubGitHubCredentialReporter) Report(context.Context) ([]ghtoken.RepoCredentialState, error) {
	s.called++
	return s.states, s.err
}

func getAuthState(t *testing.T, s *Server) *bossanovav1.GetAuthStateResponse {
	t.Helper()
	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v, want nil", err)
	}
	return resp.Msg
}

// The signed-in handler maps every report field verbatim and leaves a zero
// expiry or next attempt unset rather than encoding 1970.
func TestGetAuthStateMapsGitHubCredentialReport(t *testing.T) {
	expires := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	next := time.Date(2026, 11, 2, 10, 5, 0, 0, time.UTC)
	tests := []struct {
		name  string
		state ghtoken.RepoCredentialState
		want  *bossanovav1.GitHubCredentialState
	}{
		{
			name: "installation with expiry and refresh",
			state: ghtoken.RepoCredentialState{
				RepoID: "repo-1", Origin: "https://github.com/acme/app", Identity: ghtoken.IdentityInstallation,
				InstallationID: 42, AccountLogin: "acme", Reason: ghtoken.ReasonInstallation,
				ExpiresAt: expires, NextAttemptAt: next,
			},
			want: &bossanovav1.GitHubCredentialState{
				RepoId: "repo-1", RepoOriginUrl: "https://github.com/acme/app",
				Identity:       bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_INSTALLATION,
				InstallationId: 42, AccountLogin: "acme", Reason: "installation",
			},
		},
		{
			name: "user backing off",
			state: ghtoken.RepoCredentialState{
				RepoID: "repo-2", Origin: "https://github.com/acme/lib", Identity: ghtoken.IdentityUser,
				Reason: ghtoken.ReasonNoInstallation, NextAttemptAt: next,
			},
			want: &bossanovav1.GitHubCredentialState{
				RepoId: "repo-2", RepoOriginUrl: "https://github.com/acme/lib",
				Identity: bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_USER,
				Reason:   "no_installation",
			},
		},
		{
			name:  "unknown identity and reason with zero timestamps",
			state: ghtoken.RepoCredentialState{RepoID: "repo-3", Identity: "robot", Reason: "brand_new_reason"},
			want: &bossanovav1.GitHubCredentialState{
				RepoId:   "repo-3",
				Identity: bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_UNSPECIFIED,
				Reason:   "brand_new_reason",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creds := &stubGitHubCredentialReporter{states: []ghtoken.RepoCredentialState{tc.state}}
			msg := getAuthState(t, New(Config{AuthStateReporter: &stubAuthStateReporter{}, GitHubCredentials: creds}))

			repos := msg.GetGithubCredentialReport().GetRepos()
			if len(repos) != 1 {
				t.Fatalf("report repos = %d, want 1", len(repos))
			}
			got := repos[0]
			if got.GetExpiresAt().IsValid() != !tc.state.ExpiresAt.IsZero() ||
				(got.GetExpiresAt() != nil && !got.GetExpiresAt().AsTime().Equal(tc.state.ExpiresAt)) {
				t.Errorf("expires_at = %v, want %v (unset when zero)", got.GetExpiresAt(), tc.state.ExpiresAt)
			}
			if got.GetNextAttemptAt().IsValid() != !tc.state.NextAttemptAt.IsZero() ||
				(got.GetNextAttemptAt() != nil && !got.GetNextAttemptAt().AsTime().Equal(tc.state.NextAttemptAt)) {
				t.Errorf("next_attempt_at = %v, want %v (unset when zero)", got.GetNextAttemptAt(), tc.state.NextAttemptAt)
			}
			got.ExpiresAt, got.NextAttemptAt = nil, nil
			if !proto.Equal(got, tc.want) {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
			if creds.called != 1 {
				t.Errorf("Report called %d times, want 1", creds.called)
			}
		})
	}
}

// A local-only daemon still reports credentials: upstream_configured=false
// and a populated report together.
func TestGetAuthStateLocalOnlyCarriesGitHubCredentialReport(t *testing.T) {
	creds := &stubGitHubCredentialReporter{states: []ghtoken.RepoCredentialState{
		{RepoID: "repo-1", Origin: "https://github.com/acme/app", Identity: ghtoken.IdentityUser, Reason: ghtoken.ReasonNotSignedIn},
	}}
	msg := getAuthState(t, New(Config{GitHubCredentials: creds}))

	if msg.GetUpstreamConfigured() {
		t.Error("upstream_configured = true, want false")
	}
	repos := msg.GetGithubCredentialReport().GetRepos()
	if len(repos) != 1 || repos[0].GetReason() != "not_signed_in" {
		t.Fatalf("report repos = %v, want one not_signed_in repo", repos)
	}
}

// No source, a failing source and an empty source give three distinct
// answers: unset, unset with the auth fields intact, and set-but-empty.
func TestGetAuthStateGitHubCredentialReportPresence(t *testing.T) {
	t.Run("no reporter leaves it unset", func(t *testing.T) {
		msg := getAuthState(t, New(Config{AuthStateReporter: &stubAuthStateReporter{}}))
		if msg.GithubCredentialReport != nil {
			t.Fatalf("github_credential_report = %v, want unset", msg.GithubCredentialReport)
		}
	})
	t.Run("report error leaves it unset and keeps auth", func(t *testing.T) {
		auth := &stubAuthStateReporter{state: DaemonAuthState{NeedsLogin: true, ReloginReason: "refresh_token_rejected"}}
		creds := &stubGitHubCredentialReporter{err: errors.New("list repos: database is locked")}
		msg := getAuthState(t, New(Config{AuthStateReporter: auth, GitHubCredentials: creds}))
		if msg.GithubCredentialReport != nil {
			t.Fatalf("github_credential_report = %v, want unset on a Report error", msg.GithubCredentialReport)
		}
		if !msg.GetUpstreamConfigured() || !msg.GetNeedsLogin() || msg.GetReloginReason() != "refresh_token_rejected" {
			t.Fatalf("auth fields = %v, want them populated despite the Report error", msg)
		}
	})
	t.Run("no repos is set but empty", func(t *testing.T) {
		msg := getAuthState(t, New(Config{GitHubCredentials: &stubGitHubCredentialReporter{}}))
		if msg.GithubCredentialReport == nil || len(msg.GetGithubCredentialReport().GetRepos()) != 0 {
			t.Fatalf("github_credential_report = %v, want set with no repos", msg.GithubCredentialReport)
		}
	})
}

// stallingGitHubCredentialReporter models a repo listing stuck on a locked
// database or keychain: Report ignores its context and returns only when the
// test releases it.
type stallingGitHubCredentialReporter struct{ release chan struct{} }

func (s stallingGitHubCredentialReporter) Report(context.Context) ([]ghtoken.RepoCredentialState, error) {
	<-s.release
	return []ghtoken.RepoCredentialState{{RepoID: "late"}}, nil
}

// A stalled credential report gives up at its own bound, inside the doctor's
// 5s probe, so the auth half still answers and the report reads as unset.
func TestGetAuthStateStalledGitHubCredentialReportKeepsAuth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		creds := stallingGitHubCredentialReporter{release: make(chan struct{})}
		defer close(creds.release)
		auth := &stubAuthStateReporter{state: DaemonAuthState{NeedsLogin: true, ReloginReason: "refresh_token_rejected"}}
		start := time.Now()
		msg := getAuthState(t, New(Config{AuthStateReporter: auth, GitHubCredentials: creds}))
		if waited := time.Since(start); waited != githubCredentialReportTimeout {
			t.Fatalf("GetAuthState waited %v, want exactly the %v report bound", waited, githubCredentialReportTimeout)
		}
		if msg.GithubCredentialReport != nil {
			t.Fatalf("github_credential_report = %v, want unset when Report stalls", msg.GithubCredentialReport)
		}
		if !msg.GetNeedsLogin() || msg.GetReloginReason() != "refresh_token_rejected" {
			t.Fatalf("auth fields = %v, want them populated despite the stalled report", msg)
		}
	})
}

// fakeInstallationIssuer mints one distinctive installation token.
type fakeInstallationIssuer struct{ token string }

func (f fakeInstallationIssuer) IssueInstallationToken(context.Context, string, string) (ghtoken.MintedToken, error) {
	return ghtoken.MintedToken{Token: f.token, ExpiresAt: time.Now().Add(time.Hour), InstallationID: 9, AccountLogin: "acme"}, nil
}

// fixedDaemonSessionToken is a daemon session token that never changes.
type fixedDaemonSessionToken string

func (f fixedDaemonSessionToken) Get() string                   { return string(f) }
func (fixedDaemonSessionToken) Set(string)                      {}
func (fixedDaemonSessionToken) CompareAndSwap(_, _ string) bool { return false }

// fakeCredentialRepos lists fixed repositories.
type fakeCredentialRepos []*models.Repo

func (f fakeCredentialRepos) List(context.Context) ([]*models.Repo, error) { return f, nil }

// A real signed-in source with a cached installation token: the report says
// installation, and the token crosses the socket in neither encoding.
func TestGetAuthStateGitHubCredentialReportNeverCarriesToken(t *testing.T) {
	const token = "ghs_SECRET-installation-token-must-not-cross-the-socket"
	src := ghtoken.New(ghtoken.Config{
		Client: fakeInstallationIssuer{token: token},
		Tokens: fixedDaemonSessionToken("daemon-session"),
		Repos:  fakeCredentialRepos{{ID: "repo-1", OriginURL: "git@github.com:acme/app.git"}},
	})
	if got := src.Credential(context.Background(), "git@github.com:acme/app.git"); got.Token() != token {
		t.Fatalf("Credential token = %q, want the minted token cached", got.Token())
	}

	msg := getAuthState(t, New(Config{GitHubCredentials: src}))

	repos := msg.GetGithubCredentialReport().GetRepos()
	if len(repos) != 1 || repos[0].GetReason() != "installation" ||
		repos[0].GetIdentity() != bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_INSTALLATION {
		t.Fatalf("report repos = %v, want one installation repo", repos)
	}
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	js, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), token) || strings.Contains(string(js), token) {
		t.Fatal("GetAuthState response carries the installation token")
	}
}
