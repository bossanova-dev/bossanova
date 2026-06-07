package views

import (
	"context"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/recurser/bossalib/vcs"
)

type githubAppInstallTarget struct {
	id   string
	kind string
}

var lookupGitHubAppInstallTarget = lookupGitHubAppInstallTargetWithGH

func enrichGitHubAppInstallURL(ctx context.Context, rawURL, repoOrigin string) string {
	nwo := vcs.GitHubNWO(repoOrigin)
	if nwo == "" {
		return rawURL
	}
	target, err := lookupGitHubAppInstallTarget(ctx, nwo)
	if err != nil || target.id == "" || target.kind == "" {
		return rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.Scheme != "https" || u.Host != "github.com" {
		return rawURL
	}
	if strings.HasSuffix(u.Path, "/installations/select_target") {
		u.Path = strings.TrimSuffix(u.Path, "/installations/select_target") + "/installations/new/permissions"
	} else if strings.HasSuffix(u.Path, "/installations/new") {
		u.Path += "/permissions"
	}

	q := u.Query()
	q.Set("target_id", target.id)
	q.Set("target_type", target.kind)
	u.RawQuery = q.Encode()
	return u.String()
}

func lookupGitHubAppInstallTargetWithGH(ctx context.Context, nwo string) (githubAppInstallTarget, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+nwo, "--jq", ".owner | [.id, .type] | @tsv")
	out, err := cmd.Output()
	if err != nil {
		return githubAppInstallTarget{}, err
	}

	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 2 {
		return githubAppInstallTarget{}, nil
	}
	return githubAppInstallTarget{id: parts[0], kind: parts[1]}, nil
}
