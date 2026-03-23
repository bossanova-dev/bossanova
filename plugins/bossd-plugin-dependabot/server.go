package main

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// hostClient defines the methods server uses from the host service client.
// Both hostServiceClient and lazyHostServiceClient implement this interface.
type hostClient interface {
	ListDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error)
	ListClosedDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error)
	GetCheckResults(ctx context.Context, repoOriginURL string, prNumber int32) ([]*bossanovav1.CheckResult, error)
	GetPRStatus(ctx context.Context, repoOriginURL string, prNumber int32) (*bossanovav1.PRStatus, error)
}

// server implements the TaskSourceService gRPC server for the dependabot
// plugin. It uses a hostClient to call back into the daemon for
// VCS data, then classifies dependabot PRs into task actions.
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
			Name:         "dependabot",
			Version:      "0.1.0",
			Capabilities: []string{"task_source"},
		},
	}, nil
}

// PollTasks lists open dependabot PRs for the given repo, classifies each
// one based on check results and merge status, and returns TaskItems with
// the appropriate action: AUTO_MERGE, CREATE_SESSION, or NOTIFY_USER.
func (s *server) PollTasks(ctx context.Context, req *bossanovav1.PollTasksRequest) (*bossanovav1.PollTasksResponse, error) {
	repoURL := req.GetRepoOriginUrl()
	if repoURL == "" {
		return &bossanovav1.PollTasksResponse{}, nil
	}

	prs, err := s.host.ListDependabotPRs(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("list dependabot PRs: %w", err)
	}

	if len(prs) == 0 {
		return &bossanovav1.PollTasksResponse{}, nil
	}

	// Fetch recently-closed dependabot PRs to detect previously-rejected libraries.
	closedPRs, err := s.host.ListClosedDependabotPRs(ctx, repoURL)
	if err != nil {
		s.logger.Warn().Err(err).Msg("failed to list closed PRs, skipping rejection check")
		closedPRs = nil
	}

	var tasks []*bossanovav1.TaskItem
	for _, pr := range prs {
		// Check if a previous version of this library's PR was rejected.
		if isPreviouslyRejected(pr, closedPRs) {
			s.logger.Info().
				Int32("pr", pr.GetNumber()).
				Str("title", pr.GetTitle()).
				Msg("previously rejected library, notifying user")
			externalID := fmt.Sprintf("dependabot:pr:%s:%d", repoURL, pr.GetNumber())
			tasks = append(tasks, &bossanovav1.TaskItem{
				ExternalId:     externalID,
				Title:          pr.GetTitle(),
				RepoOriginUrl:  repoURL,
				BaseBranch:     "main",
				ExistingBranch: pr.GetHeadBranch(),
				Labels:         []string{"dependabot", parseDependabotLibrary(pr)},
				Action:         bossanovav1.TaskAction_TASK_ACTION_NOTIFY_USER,
			})
			continue
		}

		task, err := s.classifyPR(ctx, repoURL, pr)
		if err != nil {
			s.logger.Warn().Err(err).
				Int32("pr", pr.GetNumber()).
				Msg("failed to classify PR, skipping")
			continue
		}
		if task != nil {
			tasks = append(tasks, task)
		}
	}

	return &bossanovav1.PollTasksResponse{Tasks: tasks}, nil
}

// classifyPR determines what action to take for a dependabot PR.
// Returns nil if the PR is not yet actionable (checks still running).
func (s *server) classifyPR(ctx context.Context, repoURL string, pr *bossanovav1.PRSummary) (*bossanovav1.TaskItem, error) {
	prNumber := pr.GetNumber()

	// Fetch check results.
	checks, err := s.host.GetCheckResults(ctx, repoURL, prNumber)
	if err != nil {
		return nil, fmt.Errorf("get checks for PR #%d: %w", prNumber, err)
	}

	// No checks yet — skip, will re-evaluate on next poll.
	if len(checks) == 0 {
		s.logger.Debug().Int32("pr", prNumber).Msg("no checks yet, skipping")
		return nil, nil
	}

	checksStatus := aggregateCheckResults(checks)

	// Checks still pending — skip for now.
	if checksStatus == checksOverallPending {
		s.logger.Debug().Int32("pr", prNumber).Msg("checks still pending, skipping")
		return nil, nil
	}

	externalID := fmt.Sprintf("dependabot:pr:%s:%d", repoURL, prNumber)
	base := &bossanovav1.TaskItem{
		ExternalId:     externalID,
		Title:          pr.GetTitle(),
		RepoOriginUrl:  repoURL,
		BaseBranch:     "main",
		ExistingBranch: pr.GetHeadBranch(),
		Labels:         []string{"dependabot", parseDependabotLibrary(pr)},
	}

	// Checks failed → CREATE_SESSION to fix the issue.
	if checksStatus == checksOverallFailed {
		s.logger.Info().Int32("pr", prNumber).Str("title", pr.GetTitle()).Msg("checks failed, creating fix session")
		base.Action = bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION
		base.Plan = fmt.Sprintf(
			"Dependabot PR #%d (%s) has failing checks. "+
				"Checkout the existing branch, investigate test failures, "+
				"fix the code, and push. The PR will re-run checks automatically.",
			prNumber, pr.GetTitle(),
		)
		return base, nil
	}

	// Checks passed. Check mergeability.
	prStatus, err := s.host.GetPRStatus(ctx, repoURL, prNumber)
	if err != nil {
		return nil, fmt.Errorf("get PR status for #%d: %w", prNumber, err)
	}

	if prStatus.Mergeable != nil && !*prStatus.Mergeable {
		s.logger.Debug().Int32("pr", prNumber).Msg("not mergeable, skipping")
		return nil, nil
	}

	// Checks passed + mergeable → AUTO_MERGE.
	s.logger.Info().Int32("pr", prNumber).Str("title", pr.GetTitle()).Msg("checks passed, auto-merge")
	base.Action = bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE
	return base, nil
}

func (s *server) UpdateTaskStatus(_ context.Context, req *bossanovav1.UpdateTaskStatusRequest) (*bossanovav1.UpdateTaskStatusResponse, error) { //nolint:unparam // interface implementation
	s.logger.Info().
		Str("external_id", req.GetExternalId()).
		Str("status", req.GetStatus().String()).
		Str("details", req.GetDetails()).
		Msg("task status updated")
	return &bossanovav1.UpdateTaskStatusResponse{}, nil
}

// --- Check aggregation (plugin-local, mirrors daemon logic) ---

type checksOverall int

const (
	checksOverallPending checksOverall = iota
	checksOverallPassed
	checksOverallFailed
)

// aggregateCheckResults determines the overall status of a set of checks.
// Any failure/cancellation/timeout → failed. All completed with success/neutral/skipped → passed.
// Otherwise → pending.
func aggregateCheckResults(checks []*bossanovav1.CheckResult) checksOverall {
	allCompleted := true
	for _, c := range checks {
		if c.GetStatus() != bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED {
			allCompleted = false
			continue
		}
		if c.Conclusion == nil {
			return checksOverallFailed
		}
		switch *c.Conclusion {
		case bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE,
			bossanovav1.CheckConclusion_CHECK_CONCLUSION_CANCELLED,
			bossanovav1.CheckConclusion_CHECK_CONCLUSION_TIMED_OUT:
			return checksOverallFailed
		default:
			// SUCCESS, NEUTRAL, SKIPPED, UNSPECIFIED — not failures.
		}
	}
	if allCompleted {
		return checksOverallPassed
	}
	return checksOverallPending
}
