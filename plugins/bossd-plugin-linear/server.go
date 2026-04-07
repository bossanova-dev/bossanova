package main

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// hostClient defines the methods server uses from the host service client.
// Both hostServiceClient and eagerHostServiceClient implement this interface.
type hostClient interface {
	ListOpenPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error)
}

// server implements the TaskSourceService gRPC server for the Linear
// plugin. It uses a hostClient to call back into the daemon for
// PR data, then matches Linear issues to existing PRs.
type server struct {
	host   hostClient
	logger zerolog.Logger
}

func newServer(host hostClient, logger zerolog.Logger) *server {
	return &server{host: host, logger: logger}
}

func (s *server) GetInfo(_ context.Context, _ *bossanovav1.TaskSourceServiceGetInfoRequest) (*bossanovav1.TaskSourceServiceGetInfoResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.TaskSourceServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:         "linear",
			Version:      "0.1.0",
			Capabilities: []string{"task_source"},
		},
	}, nil
}

// PollTasks returns empty for Linear plugin. Linear issues are fetched
// on-demand via ListAvailableIssues, not polled automatically.
func (s *server) PollTasks(_ context.Context, _ *bossanovav1.PollTasksRequest) (*bossanovav1.PollTasksResponse, error) { //nolint:unparam // interface implementation
	s.logger.Debug().Msg("PollTasks called (Linear plugin is user-initiated, not polled)")
	return &bossanovav1.PollTasksResponse{}, nil
}

func (s *server) UpdateTaskStatus(_ context.Context, req *bossanovav1.UpdateTaskStatusRequest) (*bossanovav1.UpdateTaskStatusResponse, error) { //nolint:unparam // interface implementation
	s.logger.Info().
		Str("external_id", req.GetExternalId()).
		Str("status", req.GetStatus().String()).
		Str("details", req.GetDetails()).
		Msg("task status updated")
	return &bossanovav1.UpdateTaskStatusResponse{}, nil
}

// ListAvailableIssues fetches Linear issues and matches them to existing PRs.
func (s *server) ListAvailableIssues(ctx context.Context, req *bossanovav1.ListAvailableIssuesRequest) (*bossanovav1.ListAvailableIssuesResponse, error) {
	repoURL := req.GetRepoOriginUrl()
	if repoURL == "" {
		return nil, fmt.Errorf("repo_origin_url is required")
	}

	config := req.GetConfig()
	apiKey := config["linear_api_key"]

	if apiKey == "" {
		return nil, fmt.Errorf("linear_api_key is required in config")
	}

	// Create Linear client and fetch issues
	linearClient := newLinearClient(apiKey)
	issues, err := linearClient.FetchIssues(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch Linear issues: %w", err)
	}

	// Fetch open PRs for matching
	prs, err := s.host.ListOpenPRs(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	// Convert Linear issues to TrackerIssue protos, matching to PRs
	trackerIssues := make([]*bossanovav1.TrackerIssue, 0, len(issues))
	for _, issue := range issues {
		prNumber, branch := matchPR(issue, prs)
		trackerIssues = append(trackerIssues, &bossanovav1.TrackerIssue{
			ExternalId:     issue.Identifier,
			Title:          issue.Title,
			Description:    issue.Description,
			BranchName:     issue.BranchName,
			Url:            issue.URL,
			State:          issue.State,
			PrNumber:       prNumber,
			ExistingBranch: branch,
		})
	}

	s.logger.Info().
		Int("issue_count", len(trackerIssues)).
		Msg("fetched Linear issues")

	return &bossanovav1.ListAvailableIssuesResponse{Issues: trackerIssues}, nil
}
