package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/plugin"
)

func (s *Server) ListTrackerIssues(ctx context.Context, req *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	if req.Msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}

	// Get repo from store.
	repo, err := s.repos.Get(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	// Resolve the task source. Defaults to "linear" so existing callers (and
	// older clients that don't set the field) keep working unchanged.
	want := req.Msg.GetSource()
	if want == "" {
		want = "linear"
	}

	// Validate credentials, build the plugin config map and find the plugin
	// for the chosen source.
	source, config, err := trackerSource(ctx, s.taskSources(), repo, want)
	if err != nil {
		if errors.Is(err, errUnknownTrackerSource) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	// Call ListAvailableIssues. The query is optional; when set, the plugin pushes
	// it down to the tracker API as a filter so the search isn't limited to the
	// first page that the plugin happens to have cached locally.
	query := strings.TrimSpace(req.Msg.Query)
	issues, err := source.ListAvailableIssues(ctx, repo.OriginURL, query, config)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list tracker issues: %w", err))
	}

	keys, err := s.activeSessionKeysForRepo(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}
	issues = keys.excludeIssues(issues)

	return connect.NewResponse(&pb.ListTrackerIssuesResponse{Issues: issues}), nil
}

// taskSources returns the plugin host's task-source lister, or nil when no
// plugin host is wired.
func (s *Server) taskSources() func() []plugin.TaskSource {
	if s.pluginHost == nil {
		return nil
	}
	return s.pluginHost.GetTaskSources
}

// selectTaskSource returns the first task source whose GetInfo name matches
// want, or nil if none does. This is how ListTrackerIssues routes a request to
// the Linear or Sentry plugin: the request's source name is matched against
// each loaded plugin's self-reported name. A plugin whose GetInfo errors is
// skipped rather than aborting the search.
func selectTaskSource(ctx context.Context, sources []plugin.TaskSource, want string) plugin.TaskSource {
	for _, src := range sources {
		info, err := src.GetInfo(ctx)
		if err != nil {
			continue
		}
		if info.Name == want {
			return src
		}
	}
	return nil
}

// loadedTaskSourceNames returns the GetInfo names of every task source that
// responds, sorted. It backs the actionable error ListTrackerIssues returns
// when a requested source isn't loaded. Sources whose GetInfo errors are
// skipped, matching selectTaskSource's behavior.
func loadedTaskSourceNames(ctx context.Context, sources []plugin.TaskSource) []string {
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		info, err := src.GetInfo(ctx)
		if err != nil {
			continue
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}
