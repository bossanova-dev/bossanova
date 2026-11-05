package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/adapter/github"
	"github.com/recurser/bossd/internal/ghtoken"
)

// TestWireUpstreamBuildsLocalOnlyGitHubCredentials proves a daemon with no
// upstream still gets a non-nil credential source, so a consumer never holds
// a nil *ghtoken.Source, and that it answers with the user's login without an
// RPC.
func TestWireUpstreamBuildsLocalOnlyGitHubCredentials(t *testing.T) {
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")
	w := &daemonWiring{}
	w.wireUpstream()

	if w.githubCredentials == nil {
		t.Fatal("wireUpstream left githubCredentials nil without an upstream")
	}
	got := w.githubCredentials.Credential(context.Background(), "git@github.com:owner/repo.git")
	if got.Identity != ghtoken.IdentityUser || got.Reason != ghtoken.ReasonNotSignedIn || got.Token() != "" {
		t.Fatalf("local-only Credential = %v, want user/not_signed_in", got)
	}
}

// recordingInstallationTokenRPC records each IssueGitHubInstallationToken
// request and answers with resp or err.
type recordingInstallationTokenRPC struct {
	origins []string
	bearers []string
	resp    *bossanovav1.IssueGitHubInstallationTokenResponse
	err     error
}

func (r *recordingInstallationTokenRPC) IssueGitHubInstallationToken(_ context.Context, req *connect.Request[bossanovav1.IssueGitHubInstallationTokenRequest]) (*connect.Response[bossanovav1.IssueGitHubInstallationTokenResponse], error) {
	r.origins = append(r.origins, req.Msg.GetRepoOriginUrl())
	r.bearers = append(r.bearers, req.Header().Get("Authorization"))
	if r.err != nil {
		return nil, r.err
	}
	return connect.NewResponse(r.resp), nil
}

// staticSessionTokens is a fixed daemon session token holder.
type staticSessionTokens struct{ tok string }

func (s *staticSessionTokens) Get() string { return s.tok }

func (s *staticSessionTokens) Set(tok string) { s.tok = tok }

func (s *staticSessionTokens) CompareAndSwap(old, tok string) bool {
	if s.tok != old {
		return false
	}
	s.tok = tok
	return true
}

var _ installationTokenRPC = bossanovav1connect.OrchestratorServiceClient(nil)

// TestGitHubInstallationTokenClientSendsDaemonBearer drives the signed-in
// source through the protobuf adapter: the request carries the canonical
// origin and the daemon session token as the bearer, and the response maps
// onto the credential.
func TestGitHubInstallationTokenClientSendsDaemonBearer(t *testing.T) {
	t.Parallel()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rpc := &recordingInstallationTokenRPC{resp: &bossanovav1.IssueGitHubInstallationTokenResponse{
		Token:          "ghs_wiring-test",
		ExpiresAt:      timestamppb.New(expires),
		InstallationId: 7,
		AccountLogin:   "owner",
		Permissions:    map[string]string{"contents": "read"},
	}}
	src := ghtoken.New(ghtoken.Config{
		Client: githubInstallationTokenClient{rpc: rpc},
		Tokens: &staticSessionTokens{tok: "daemon-session"},
	})

	got := src.Credential(context.Background(), "git@github.com:owner/repo.git")
	if got.Identity != ghtoken.IdentityInstallation || got.Token() != "ghs_wiring-test" || got.InstallationID != 7 ||
		got.AccountLogin != "owner" || !got.ExpiresAt.Equal(expires) || got.Permissions["contents"] != "read" {
		t.Fatalf("Credential = %v, want the minted installation token", got)
	}
	if len(rpc.origins) != 1 || rpc.origins[0] != "https://github.com/owner/repo" {
		t.Fatalf("request origins = %v, want the one canonical origin", rpc.origins)
	}
	if rpc.bearers[0] != "Bearer daemon-session" {
		t.Fatalf("Authorization = %q, want the daemon session bearer", rpc.bearers[0])
	}
}

// TestGitHubInstallationTokenClientKeepsErrorCode proves a refusal reaches the
// source with its connect code, so it maps to the right reason.
func TestGitHubInstallationTokenClientKeepsErrorCode(t *testing.T) {
	t.Parallel()
	rpc := &recordingInstallationTokenRPC{err: connect.NewError(connect.CodePermissionDenied, errors.New("refused"))}
	src := ghtoken.New(ghtoken.Config{
		Client: githubInstallationTokenClient{rpc: rpc},
		Tokens: &staticSessionTokens{tok: "daemon-session"},
	})
	if got := src.Credential(context.Background(), "https://github.com/owner/repo"); got.Identity != ghtoken.IdentityUser || got.Reason != ghtoken.ReasonNoInstallation {
		t.Fatalf("Credential = %v, want user/no_installation", got)
	}
}

func signedInGitHubCredentials(rpc *recordingInstallationTokenRPC) *ghtoken.Source {
	return ghtoken.New(ghtoken.Config{
		Client: githubInstallationTokenClient{rpc: rpc},
		Tokens: &staticSessionTokens{tok: "daemon-session"},
	})
}

func installationTokenResponse(token string) *bossanovav1.IssueGitHubInstallationTokenResponse {
	return &bossanovav1.IssueGitHubInstallationTokenResponse{
		Token:          token,
		ExpiresAt:      timestamppb.New(time.Now().Add(time.Hour)),
		InstallationId: 7,
		Permissions:    map[string]string{"pull_requests": "read", "statuses": "write"},
	}
}

// TestInstallationTokenSourceMapsNWOToOrigin proves the cmd adapter asks the
// credential source for the repo's github.com origin, hands the adapter the
// token with its installation and permissions, and maps Invalidate onto the
// same origin so the next Token call mints afresh.
func TestInstallationTokenSourceMapsNWOToOrigin(t *testing.T) {
	t.Parallel()
	rpc := &recordingInstallationTokenRPC{resp: installationTokenResponse("ghs_first")}
	adapter := installationTokenSource{src: signedInGitHubCredentials(rpc)}
	ctx := context.Background()

	tok, ok := adapter.Token(ctx, "owner/repo")
	if !ok || tok.Value != "ghs_first" || tok.InstallationID != 7 || tok.Permissions["statuses"] != "write" {
		t.Fatalf("Token = %v, %t; want the minted installation token", tok, ok)
	}
	if len(rpc.origins) != 1 || rpc.origins[0] != "https://github.com/owner/repo" {
		t.Fatalf("request origins = %v, want the repo's canonical origin", rpc.origins)
	}

	rpc.resp = installationTokenResponse("ghs_second")
	adapter.Invalidate("owner/repo", "ghs_first")
	if tok, ok := adapter.Token(ctx, "owner/repo"); !ok || tok.Value != "ghs_second" {
		t.Fatalf("Token after Invalidate = %v, %t; want a re-minted token", tok, ok)
	}

	rpc.err = connect.NewError(connect.CodePermissionDenied, errors.New("refused"))
	if _, ok := adapter.Token(ctx, "other/repo"); ok {
		t.Fatal("Token for a repo without an installation answered ok, want the user's login")
	}
}

// TestGitHubProviderRunsReadsWithTheMintedToken wires the real provider to a
// signed-in source and a stub gh, and checks a read's gh process sees the
// minted installation token while the daemon's own env is untouched.
func TestGitHubProviderRunsReadsWithTheMintedToken(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\necho \"GH_TOKEN=${GH_TOKEN-unset}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("GH_TOKEN", "operator-token")

	rpc := &recordingInstallationTokenRPC{resp: installationTokenResponse("ghs_wired")}
	p := github.New(zerolog.Nop())
	p.SetInstallationTokens(installationTokenSource{src: signedInGitHubCredentials(rpc)})

	out, err := p.GetFailedCheckLogs(context.Background(), "git@github.com:owner/repo.git", "1")
	if err != nil {
		t.Fatal(err)
	}
	if out != "GH_TOKEN=ghs_wired\n" {
		t.Fatalf("gh saw %q, want the minted installation token", out)
	}
	if got := os.Getenv("GH_TOKEN"); got != "operator-token" {
		t.Fatalf("daemon GH_TOKEN = %q after the call, want it unchanged", got)
	}
}

// TestSetGitHubCredentialReporterKeepsNilAnInterfaceNil proves a nil source
// leaves the server's reporter a nil interface, not a typed nil the handler
// would call, and that a real source reaches the Config field wire_daemon
// hands server.New.
func TestSetGitHubCredentialReporterKeepsNilAnInterfaceNil(t *testing.T) {
	t.Parallel()
	var cfg server.Config
	setGitHubCredentialReporter(&cfg, nil)
	if cfg.GitHubCredentials != nil || cfg.GitHubReadCredentials != nil {
		t.Fatalf("GitHubCredentials = %#v after a nil source, want a nil interface", cfg.GitHubCredentials)
	}
	src := ghtoken.New(ghtoken.Config{})
	setGitHubCredentialReporter(&cfg, src)
	if cfg.GitHubReadCredentials != src {
		t.Fatal("read credential source not wired")
	}
	if got, ok := cfg.GitHubCredentials.(*ghtoken.Source); !ok || got != src {
		t.Fatalf("GitHubCredentials = %#v, want src itself", cfg.GitHubCredentials)
	}
}
