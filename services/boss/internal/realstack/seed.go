package realstack

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// SeedRepo creates a local repository, registers its metadata and restores its local origin.
func SeedRepo(ctx context.Context, client realstackrpc.Client, home string, repo *pb.Repo) (*pb.Repo, string, error) {
	path, err := CreateRepo(home, "checkout")
	if err != nil {
		return nil, "", err
	}
	local := filepath.Join(filepath.Dir(path), "origin.git")
	if repo.OriginUrl != "" {
		if err = RunGit(path, "remote", "set-url", "origin", repo.OriginUrl); err != nil {
			return nil, "", err
		}
	}
	response, err := client.RegisterRepo(ctx, realstackrpc.Request(&pb.RegisterRepoRequest{LocalPath: path, DisplayName: repo.DisplayName, DefaultBaseBranch: "main", WorktreeBaseDir: filepath.Join(home, "worktrees"), IsTrusted: proto.Bool(true)}))
	if resetErr := RunGit(path, "remote", "set-url", "origin", local); resetErr != nil {
		return nil, "", resetErr
	}
	if err != nil {
		return nil, "", fmt.Errorf("register seed repo: %w", err)
	}
	return response.Msg.Repo, path, nil
}
