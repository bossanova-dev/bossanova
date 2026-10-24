package repos

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type AddClient interface {
	ValidateRepoPath(context.Context, string) (*pb.ValidateRepoPathResponse, error)
	RegisterRepo(context.Context, *pb.RegisterRepoRequest) (*pb.Repo, error)
	CloneAndRegisterRepo(context.Context, *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error)
	UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error)
}
type ListClient interface {
	ListRepos(context.Context) ([]*pb.Repo, error)
	RemoveRepo(context.Context, string) error
}
type SettingsClient interface {
	ListRepos(context.Context) ([]*pb.Repo, error)
	UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error)
}

// GitHubActions supplies the platform browser and GitHub installation lookup.
type GitHubActions interface {
	OpenURL(string) error
	EnrichInstallURL(context.Context, string, string) string
}

func (m *RepoAddModel) SetGitHubActions(actions GitHubActions)      { m.githubActions = actions }
func (m *RepoSettingsModel) SetGitHubActions(actions GitHubActions) { m.githubActions = actions }
