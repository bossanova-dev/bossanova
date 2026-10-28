package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/recurser/bossalib/contract/daemon/trackerref"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/session"
)

// errUnknownTrackerSource marks a tracker source name no plugin config exists
// for. ListTrackerIssues maps it to InvalidArgument; every other
// trackerSource failure is a missing precondition.
var errUnknownTrackerSource = errors.New("unknown tracker source")

// trackerSource validates the repo's credentials for the named tracker source,
// builds the plugin config map for it, and finds the loaded task-source plugin
// of that name. sources is nil when no plugin host is available. It is shared
// by ListTrackerIssues and TrackerResolver, so both route and authenticate the
// same way.
func trackerSource(ctx context.Context, sources func() []plugin.TaskSource, repo *models.Repo, want string) (plugin.TaskSource, map[string]string, error) {
	// Each source addresses its API differently, so the required fields
	// differ; a missing field is reported by name.
	var config map[string]string
	switch want {
	case "linear":
		if repo.LinearAPIKey == "" {
			return nil, nil, fmt.Errorf("linear_api_key not configured for this repo")
		}
		config = map[string]string{
			"linear_api_key": repo.LinearAPIKey,
		}
	case "sentry":
		var missing []string
		if repo.SentryAPIKey == "" {
			missing = append(missing, "sentry_api_key")
		}
		if repo.SentryOrg == "" {
			missing = append(missing, "sentry_org")
		}
		if len(missing) > 0 {
			return nil, nil, fmt.Errorf("sentry not configured for this repo: missing %s", strings.Join(missing, ", "))
		}
		config = map[string]string{
			"sentry_api_key": repo.SentryAPIKey,
			"sentry_org":     repo.SentryOrg,
		}
	default:
		return nil, nil, fmt.Errorf("%w %q", errUnknownTrackerSource, want)
	}

	if sources == nil {
		return nil, nil, fmt.Errorf("plugin host not available")
	}
	loaded := sources()
	if len(loaded) == 0 {
		return nil, nil, fmt.Errorf("no task source plugins available")
	}
	source := selectTaskSource(ctx, loaded, want)
	if source == nil {
		// Distinguish "no plugin by this name is loaded" from a runtime failure:
		// the usual cause is a plugin binary that exists but isn't loaded (config
		// stripped the entry, or the daemon wasn't restarted after it was added).
		// List what IS loaded and point at the fix instead of a bare "not found".
		names := loadedTaskSourceNames(ctx, loaded)
		available := "none"
		if len(names) > 0 {
			available = strings.Join(names, ", ")
		}
		return nil, nil, fmt.Errorf("no task source plugin named %q is loaded (loaded task sources: %s); add it to settings.json and restart bossd", want, available)
	}
	return source, config, nil
}

// linearConfigPRMatching is the "linear" plugin's ListAvailableIssues config
// key; set to linearPRMatchingSkip it makes the plugin skip the open-PR
// listing entirely and return the issues without PR fields, so a slow or
// failing GitHub listing neither spends the resolver's per-call budget nor
// fails the lookup. Only TrackerResolver sets it — it needs just the id and
// URL. ListTrackerIssues must not: the New Session picker reads a zero PR
// number as "start a fresh branch", so it needs the PR match and has to see a
// listing failure. The plugin cannot import this package, so it defines the
// same wire value (configKeyPRMatching in plugins/bossd-plugin-linear).
const (
	linearConfigPRMatching = "pr_matching"
	linearPRMatchingSkip   = "skip"
)

// GitHubIssueReader confirms an open GitHub issue, excluding pull requests.
type GitHubIssueReader interface {
	OpenIssueURL(ctx context.Context, host, owner, repo string, number int) (url string, found bool, err error)
}

// TrackerResolver confirms candidates against the repository's configured tracker.
type TrackerResolver struct {
	sources func() []plugin.TaskSource
	github  GitHubIssueReader
}

var _ session.TrackerIssueResolver = (*TrackerResolver)(nil)

// NewTrackerResolver builds a resolver over Linear task sources and GitHub issues.
func NewTrackerResolver(host *plugin.Host, github GitHubIssueReader) *TrackerResolver {
	r := &TrackerResolver{github: github}
	if host != nil {
		r.sources = host.GetTaskSources
	}
	return r
}

func trackerConfigError(adapter, reason string) error {
	return fmt.Errorf("%w: %s tracker: %s", session.ErrTrackerNotConfigured, adapter, reason)
}

var githubRepositoryRe = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+/)?[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

func githubRepository(repository string) (host, owner, repo string, ok bool) {
	if !githubRepositoryRe.MatchString(repository) {
		return "", "", "", false
	}
	parts := strings.Split(repository, "/")
	if len(parts) == 2 {
		return "github.com", parts[0], parts[1], true
	}
	return parts[0], parts[1], parts[2], true
}

// CheckCandidate checks configuration and identifier form without contacting a tracker.
func (r *TrackerResolver) CheckCandidate(repo *models.Repo, id string) error {
	_, err := r.candidateConfig(repo, id)
	return err
}

func (r *TrackerResolver) candidateConfig(repo *models.Repo, id string) (session.TrackerConfig, error) {
	if repo == nil {
		return session.TrackerConfig{}, trackerConfigError("linear", "repository not available")
	}
	config := session.ReadTrackerConfig(repo.LocalPath)
	switch config.Adapter {
	case "linear":
		if repo.LinearAPIKey == "" {
			return config, trackerConfigError("linear", "linear_api_key not configured for this repo")
		}
		if _, ok := trackerref.ParseID(id); !ok {
			return config, session.ErrTrackerIssueNotFound
		}
	case "github":
		host, owner, name, ok := githubRepository(config.GitHubRepository)
		if !ok {
			return config, trackerConfigError("github", "trackerConfig.github.repository must be [HOST/]OWNER/REPO")
		}
		if r.github == nil {
			return config, trackerConfigError("github", "gh provider not available")
		}
		ref, ok := trackerref.ParseGitHubIssue(id)
		if !ok || (ref.Repository != "" && !strings.EqualFold(ref.Repository, host+"/"+owner+"/"+name)) {
			return config, session.ErrTrackerIssueNotFound
		}
	default:
		return config, trackerConfigError(config.Adapter, fmt.Sprintf("tracker adapter %q does not support session linking", config.Adapter))
	}
	return config, nil
}

// ResolveTrackerIssue returns the canonical id and the open issue's own URL.
func (r *TrackerResolver) ResolveTrackerIssue(ctx context.Context, repo *models.Repo, id string) (string, string, error) {
	config, err := r.candidateConfig(repo, id)
	if err != nil {
		return "", "", err
	}
	if config.Adapter == "github" {
		return r.resolveGitHubIssue(ctx, config.GitHubRepository, id)
	}
	source, configMap, err := trackerSource(ctx, r.sources, repo, "linear")
	if err != nil {
		return "", "", trackerConfigError("linear", err.Error())
	}

	// Resolution needs only the id and URL, so the plugin skips the GitHub
	// open-PR listing (see linearConfigPRMatching).
	configMap[linearConfigPRMatching] = linearPRMatchingSkip
	want := strings.ToUpper(strings.TrimSpace(id))
	issues, err := source.ListAvailableIssues(ctx, repo.OriginURL, want, configMap)
	if err != nil {
		return "", "", fmt.Errorf("look up tracker issue %s: %w", want, err)
	}
	for _, issue := range issues {
		if issue.GetExternalId() == want && issue.GetUrl() != "" {
			return issue.GetExternalId(), issue.GetUrl(), nil
		}
	}
	return "", "", fmt.Errorf("%w: %s", session.ErrTrackerIssueNotFound, want)
}

func (r *TrackerResolver) resolveGitHubIssue(ctx context.Context, repository, id string) (string, string, error) {
	host, owner, repo, _ := githubRepository(repository)
	ref, _ := trackerref.ParseGitHubIssue(id)
	url, found, err := r.github.OpenIssueURL(ctx, host, owner, repo, ref.Number)
	if err != nil {
		return "", "", fmt.Errorf("look up github tracker issue %s: %w", id, err)
	}
	if !found {
		return "", "", fmt.Errorf("%w: %s", session.ErrTrackerIssueNotFound, id)
	}
	return fmt.Sprintf("GH-%d", ref.Number), url, nil
}
