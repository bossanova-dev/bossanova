package main

import (
	"context"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/adapter/github"
	"github.com/recurser/bossd/internal/ghtoken"
)

// installationTokenRPC is the one orchestrator RPC the GitHub credential
// source needs. bossanovav1connect.OrchestratorServiceClient satisfies it.
type installationTokenRPC interface {
	IssueGitHubInstallationToken(ctx context.Context, req *connect.Request[bossanovav1.IssueGitHubInstallationTokenRequest]) (*connect.Response[bossanovav1.IssueGitHubInstallationTokenResponse], error)
}

// githubInstallationTokenClient adapts the orchestrator client to
// ghtoken.Client, keeping protobuf at the API edge. It sends the daemon
// session token as the bearer; TestDaemonTokenProceduresCoverBossdDaemonTokenCalls
// scans this file for that call.
type githubInstallationTokenClient struct {
	rpc installationTokenRPC
}

// IssueInstallationToken implements ghtoken.Client. A connect error is
// returned unchanged so the source can read its code.
func (c githubInstallationTokenClient) IssueInstallationToken(ctx context.Context, sessionToken, repoOriginURL string) (ghtoken.MintedToken, error) {
	req := connect.NewRequest(&bossanovav1.IssueGitHubInstallationTokenRequest{RepoOriginUrl: repoOriginURL})
	req.Header().Set("Authorization", "Bearer "+sessionToken)
	resp, err := c.rpc.IssueGitHubInstallationToken(ctx, req)
	if err != nil {
		return ghtoken.MintedToken{}, err
	}
	msg := resp.Msg
	minted := ghtoken.MintedToken{
		Token:          msg.GetToken(),
		InstallationID: msg.GetInstallationId(),
		AccountLogin:   msg.GetAccountLogin(),
		Permissions:    msg.GetPermissions(),
	}
	if msg.GetExpiresAt().IsValid() {
		minted.ExpiresAt = msg.GetExpiresAt().AsTime()
	}
	return minted, nil
}

// installationTokenSource adapts the per-repo credential source to the GitHub
// adapter's github.InstallationTokenSource. The adapter keys repositories by
// owner/name and the source by origin URL, so this is the one place that maps
// between them; the adapter never imports ghtoken.
type installationTokenSource struct {
	src *ghtoken.Source
}

var _ github.InstallationTokenSource = installationTokenSource{}

// Token implements github.InstallationTokenSource: the repo's installation
// token while the source holds one, ok=false (the user's login) otherwise.
func (a installationTokenSource) Token(ctx context.Context, nwo string) (github.InstallationToken, bool) {
	c := a.src.Credential(ctx, githubOriginForNWO(nwo))
	if c.Identity != ghtoken.IdentityInstallation || c.Token() == "" {
		return github.InstallationToken{}, false
	}
	return github.InstallationToken{Value: c.Token(), InstallationID: c.InstallationID, Permissions: c.Permissions}, true
}

// Invalidate implements github.InstallationTokenSource.
func (a installationTokenSource) Invalidate(nwo, token string) {
	a.src.Invalidate(githubOriginForNWO(nwo), token)
}

// githubOriginForNWO is the github.com origin of an owner/name; the source
// canonicalises it to the same key a repo's own origin URL gets.
func githubOriginForNWO(nwo string) string { return "https://github.com/" + nwo }
