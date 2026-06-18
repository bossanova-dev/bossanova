package main

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// issueLister fetches unresolved Sentry issues for a query and returns them as
// TrackerIssue protos. *sentryClient implements it; the seam lets
// ListAvailableIssues be unit-tested without hitting the real Sentry API.
type issueLister interface {
	ListIssues(ctx context.Context, query string) ([]*bossanovav1.TrackerIssue, error)
}

var _ issueLister = (*sentryClient)(nil)

// server implements the TaskSourceService gRPC server for the Sentry plugin.
type server struct {
	logger zerolog.Logger
	// newLister builds an issueLister for the per-request credentials. It
	// defaults to a real Sentry client; tests override it to inject canned
	// results.
	newLister func(token, org string) issueLister
}

func newServer(logger zerolog.Logger) *server {
	return &server{
		logger: logger,
		newLister: func(token, org string) issueLister {
			return newSentryClient(token, org)
		},
	}
}

func (s *server) GetInfo(_ context.Context, _ *bossanovav1.TaskSourceServiceGetInfoRequest) (*bossanovav1.TaskSourceServiceGetInfoResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.TaskSourceServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:          "sentry",
			Version:       "0.1.0",
			Capabilities:  []string{"task_source"},
			UserInitiated: true,
		},
	}, nil
}

// PollTasks returns empty for the Sentry plugin. Sentry issues are fetched
// on-demand via ListAvailableIssues, not polled automatically.
func (s *server) PollTasks(_ context.Context, _ *bossanovav1.PollTasksRequest) (*bossanovav1.PollTasksResponse, error) { //nolint:unparam // interface implementation
	s.logger.Debug().Msg("PollTasks called (Sentry plugin is user-initiated, not polled)")
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

// ListAvailableIssues fetches unresolved Sentry issues across every project in
// the configured organization. The query is forwarded to Sentry's structured
// search so the picker's search box narrows results server-side.
func (s *server) ListAvailableIssues(ctx context.Context, req *bossanovav1.ListAvailableIssuesRequest) (*bossanovav1.ListAvailableIssuesResponse, error) {
	config := req.GetConfig()
	token := config["sentry_api_key"]
	org := config["sentry_org"]

	var missing []string
	if token == "" {
		missing = append(missing, "sentry_api_key")
	}
	if org == "" {
		missing = append(missing, "sentry_org")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required Sentry config: %v", missing)
	}

	issues, err := s.newLister(token, org).ListIssues(ctx, req.GetQuery())
	if err != nil {
		return nil, fmt.Errorf("fetch Sentry issues: %w", err)
	}

	s.logger.Info().
		Int("issue_count", len(issues)).
		Msg("fetched Sentry issues")

	return &bossanovav1.ListAvailableIssuesResponse{Issues: issues}, nil
}
