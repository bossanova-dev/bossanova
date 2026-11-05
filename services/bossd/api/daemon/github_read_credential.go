package server

import (
	"context"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/ghtoken"
	"github.com/recurser/bossd/internal/ghusage"
)

// GitHubReadCredentials supplies repository installation credentials. The local
// read surface validates every granted permission before returning the secret.
type GitHubReadCredentials interface {
	Credential(ctx context.Context, repoOriginURL string) ghtoken.Credential
}

// GetGitHubReadCredential answers a local gh read with a repository token or a
// stable fallback reason. Responses carry secrets and must never be logged.
func (s *Server) GetGitHubReadCredential(ctx context.Context, req *connect.Request[pb.GetGitHubReadCredentialRequest]) (*connect.Response[pb.GetGitHubReadCredentialResponse], error) {
	reply := func(reason string) (*connect.Response[pb.GetGitHubReadCredentialResponse], error) {
		return connect.NewResponse(&pb.GetGitHubReadCredentialResponse{Reason: reason}), nil
	}
	route := ghusage.ClassifyRead(req.Msg.GhArgs, ghusage.ReadEnv{GHHost: req.Msg.GhHost, GHRepo: req.Msg.GhRepo})
	if !route.Routable {
		return reply(route.Reason)
	}
	if s.gitHubReadCredentials == nil {
		return reply("not_signed_in")
	}
	repo := s.readCredentialRepo(ctx, route.Repo, req.Msg.WorkingDirectory)
	if repo == nil {
		return reply("unregistered_repo")
	}
	credential := s.gitHubReadCredentials.Credential(ctx, repo.OriginURL)
	if credential.Identity != ghtoken.IdentityInstallation {
		return reply(string(credential.Reason))
	}
	if len(credential.Permissions) == 0 {
		return reply("write_permission")
	}
	for _, permission := range credential.Permissions {
		if permission != "read" {
			return reply("write_permission")
		}
	}
	if time.Until(credential.ExpiresAt) < time.Minute {
		return reply("expiring")
	}
	if credential.Token() == "" {
		return reply(string(credential.Reason))
	}
	return connect.NewResponse(&pb.GetGitHubReadCredentialResponse{Token: credential.Token(), InstallationId: credential.InstallationID, Reason: "installation"}), nil
}

func (s *Server) readCredentialRepo(ctx context.Context, slug, workingDirectory string) *domain.Repo {
	if s.repos == nil {
		return nil
	}
	if slug != "" {
		repo, err := s.registeredRepo(ctx, slug)
		if err == nil {
			return repo
		}
		return nil
	}
	if !filepath.IsAbs(workingDirectory) || s.sessions == nil {
		return nil
	}
	repo, _, err := s.workspaceOwner(ctx, workingDirectory)
	if err != nil {
		return nil
	}
	return repo
}
