package upgrade

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/recurser/bossalib/config"
)

// ghTokenTimeout bounds the best-effort `gh auth token` subprocess so a missing
// or slow gh never blocks (or noticeably slows) the upgrade check.
const ghTokenTimeout = 3 * time.Second

// envGetter reads an environment variable, returning "" when unset or empty.
// Indirected through a var so tests can stub env precedence without mutating the
// process environment.
var envGetter = func(key string) string { return config.EnvOr(key, "") }

// runGH executes `gh <args...>` and returns its stdout. Indirected through a var
// so tests can stub the gh fallback without a real gh on PATH.
var runGH = func(ctx context.Context, args ...string) (string, error) {
	// gh is a fixed, trusted CLI resolved from PATH; args are literal. G204 is
	// globally excluded (a CLI shelling out by design) — see .golangci.yml.
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ResolveGitHubToken resolves a GitHub token for authenticating the upgrade
// release check, trying in order: GITHUB_TOKEN, GH_TOKEN, then a best-effort
// `gh auth token`. It returns "" when no token can be found; callers then make
// an unauthenticated request, preserving the prior (60 req/hr/IP) behavior. Any
// error or empty output from the gh subprocess is treated as "no token" so the
// check is never blocked by a missing, slow, or unauthenticated gh.
func ResolveGitHubToken(ctx context.Context) string {
	if v := strings.TrimSpace(envGetter("GITHUB_TOKEN")); v != "" {
		return v
	}
	if v := strings.TrimSpace(envGetter("GH_TOKEN")); v != "" {
		return v
	}
	tokenCtx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()
	out, err := runGH(tokenCtx, "auth", "token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
