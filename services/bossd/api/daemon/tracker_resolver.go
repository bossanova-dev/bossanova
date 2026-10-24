package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// TrackerResolver confirms a candidate ticket id against the repo's Linear
// tracker (the "linear" task-source plugin) and returns the tracker's own id
// and URL. Nothing builds a URL from an id format.
//
// It reuses ListAvailableIssues with the id as the query, so it needs no
// plugin-protocol change; the plugin only searches open states (backlog,
// unstarted, started), so a completed, canceled or triage ticket does not
// resolve.
type TrackerResolver struct {
	sources func() []plugin.TaskSource
}

var _ session.TrackerIssueResolver = (*TrackerResolver)(nil)

// NewTrackerResolver builds a resolver over host's task sources. A nil host
// resolves nothing (ErrTrackerNotConfigured).
func NewTrackerResolver(host *plugin.Host) *TrackerResolver {
	r := &TrackerResolver{}
	if host != nil {
		r.sources = host.GetTaskSources
	}
	return r
}

// ResolveTrackerIssue returns the canonical id and URL of the one issue whose
// ExternalId is exactly id (upper-cased). It returns
// session.ErrTrackerNotConfigured when the repo has no Linear key or no
// "linear" plugin is loaded, session.ErrTrackerIssueNotFound when no issue
// matches exactly, and a plugin error wrapped as is.
func (r *TrackerResolver) ResolveTrackerIssue(ctx context.Context, repo *models.Repo, id string) (string, string, error) {
	if repo == nil || repo.LinearAPIKey == "" {
		return "", "", session.ErrTrackerNotConfigured
	}
	source, config, err := trackerSource(ctx, r.sources, repo, "linear")
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", session.ErrTrackerNotConfigured, err)
	}
	// Resolution needs only the id and URL, so the plugin skips the GitHub
	// open-PR listing (see linearConfigPRMatching).
	config[linearConfigPRMatching] = linearPRMatchingSkip
	want := strings.ToUpper(strings.TrimSpace(id))
	issues, err := source.ListAvailableIssues(ctx, repo.OriginURL, want, config)
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
