package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
)

// InspectPRHead fetches base and head, then inspects the exact head pinned by the
// caller's fresh PR read. Local-only commits cannot satisfy this inspection.
func (m *Manager) InspectPRHead(ctx context.Context, localPath, base, headBranch, headSHA string) (domain.PRHeadInspection, error) {
	var result domain.PRHeadInspection
	sha, ok := vcs.NormalizeHeadSHA(headSHA)
	if !ok || headBranch == "" {
		return result, fmt.Errorf("full head SHA and head branch are required")
	}
	if err := m.FetchBase(ctx, localPath, base); err != nil {
		return result, err
	}
	headRef := "refs/remotes/origin/" + headBranch
	if _, err := fetchWithRefLockRetry(ctx, localPath, runGitRemote, "fetch", "origin", "+refs/heads/"+headBranch+":"+headRef); err != nil {
		return result, fmt.Errorf("fetch head: %w", err)
	}
	if _, err := runGit(ctx, localPath, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return result, fmt.Errorf("head commit unavailable: %w", err)
	}
	fetched, err := runGit(ctx, localPath, "rev-parse", headRef)
	if err != nil {
		return result, err
	}
	if fetched != sha {
		return result, fmt.Errorf("fetched head moved from %s to %s", sha, fetched)
	}
	baseRef := "refs/remotes/origin/" + base
	result.BaseSHA, err = runGit(ctx, localPath, "rev-parse", baseRef)
	if err != nil {
		return result, err
	}
	result.ContainsBase, err = m.IsAncestor(ctx, localPath, result.BaseSHA, sha)
	if err != nil {
		return result, err
	}
	// The status prefix keeps runGit's outer whitespace trimming away from paths.
	files, err := runGit(ctx, localPath, "diff", "--name-status", "-z", "--no-renames", result.BaseSHA+"..."+sha)
	if err != nil {
		return result, err
	}
	if files != "" {
		fields := strings.Split(strings.TrimSuffix(files, "\x00"), "\x00")
		for i := 1; i < len(fields); i += 2 {
			result.ChangedFiles = append(result.ChangedFiles, fields[i])
		}
	}
	return result, nil
}
