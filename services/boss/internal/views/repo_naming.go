package views

import (
	"path/filepath"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

// RepoNameAndBranch derives the defaults shared by repo add and init.
func RepoNameAndBranch(localPath string, validation *pb.ValidateRepoPathResponse) (name, branch string) {
	name, branch = filepath.Base(localPath), "main"
	if validation.DefaultBranch != "" {
		branch = validation.DefaultBranch
	}
	if validation.IsGithub {
		if nwo := vcs.GitHubNWO(validation.OriginUrl); nwo != "" {
			name = "@" + nwo
		}
	} else if n := parseRepoNameFromURL(validation.OriginUrl); n != "" {
		name = n
	}
	return name, branch
}
