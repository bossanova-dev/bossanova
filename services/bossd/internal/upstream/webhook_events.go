package upstream

import (
	"fmt"
	"strconv"

	"github.com/google/go-github/v76/github"
	"github.com/recurser/bossalib/vcs"
)

func TranslateWebhook(eventType string, payload []byte) ([]vcs.Event, int, error) {
	event, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return nil, 0, fmt.Errorf("parse webhook %q: %w", eventType, err)
	}

	switch e := event.(type) {
	case *github.PullRequestEvent:
		return translatePullRequest(e)
	case *github.PullRequestReviewEvent:
		return translateReview(e)
	case *github.IssueCommentEvent:
		return translateIssueComment(e)
	case *github.CheckRunEvent:
		return translateCheckRun(e)
	case *github.CheckSuiteEvent:
		return translateCheckSuite(e)
	default:
		return nil, 0, nil
	}
}

func translatePullRequest(e *github.PullRequestEvent) ([]vcs.Event, int, error) {
	var pr int
	if e.PullRequest != nil && e.PullRequest.Number != nil {
		pr = *e.PullRequest.Number
	}

	if e.Action != nil {
		switch *e.Action {
		case "closed":
			merged := e.PullRequest != nil && e.PullRequest.Merged != nil && *e.PullRequest.Merged
			if merged {
				return []vcs.Event{vcs.PRMerged{PRID: pr}}, pr, nil
			}
			return []vcs.Event{vcs.PRClosed{PRID: pr}}, pr, nil
		case "synchronize":
			if e.PullRequest != nil && e.PullRequest.Mergeable != nil && !*e.PullRequest.Mergeable {
				return []vcs.Event{vcs.ConflictDetected{PRID: pr}}, pr, nil
			}
		}
	}

	return nil, pr, nil
}

func translateReview(e *github.PullRequestReviewEvent) ([]vcs.Event, int, error) {
	if e.GetAction() != "submitted" || e.Review == nil || e.PullRequest == nil {
		return nil, 0, nil
	}

	pr := e.PullRequest.GetNumber()
	if pr == 0 {
		return nil, 0, nil
	}

	state := mapReviewState(e.Review.GetState())
	if state == vcs.ReviewStateUnspecified {
		return nil, pr, nil
	}

	review := vcs.ReviewSubmitted{PRID: pr, State: state}
	if e.Review.GetBody() != "" {
		review.Comments = []vcs.ReviewComment{{
			Author: e.Review.GetUser().GetLogin(),
			Body:   e.Review.GetBody(),
			State:  state,
		}}
	}

	return []vcs.Event{review}, pr, nil
}

func translateIssueComment(e *github.IssueCommentEvent) ([]vcs.Event, int, error) {
	if e.Issue == nil || e.Issue.PullRequestLinks == nil {
		return nil, 0, nil
	}

	pr := 0
	if e.Issue.Number != nil {
		pr = *e.Issue.Number
	}
	return nil, pr, nil
}

func mapReviewState(state string) vcs.ReviewState {
	switch state {
	case "approved":
		return vcs.ReviewStateApproved
	case "changes_requested":
		return vcs.ReviewStateChangesRequested
	case "commented":
		return vcs.ReviewStateCommented
	case "dismissed":
		return vcs.ReviewStateDismissed
	default:
		return vcs.ReviewStateUnspecified
	}
}

func translateCheckRun(e *github.CheckRunEvent) ([]vcs.Event, int, error) {
	if e.GetAction() != "completed" || e.CheckRun == nil {
		return nil, 0, nil
	}

	pr := 0
	if len(e.CheckRun.PullRequests) > 0 && e.CheckRun.PullRequests[0].Number != nil {
		pr = *e.CheckRun.PullRequests[0].Number
	} else {
		return nil, 0, nil
	}

	var conclusion vcs.CheckConclusion
	switch e.CheckRun.GetConclusion() {
	case "failure":
		conclusion = vcs.CheckConclusionFailure
	case "timed_out":
		conclusion = vcs.CheckConclusionTimedOut
	default:
		return nil, pr, nil
	}

	failed := []vcs.CheckResult{{
		ID:         fmt.Sprintf("%d", e.CheckRun.GetID()),
		Name:       e.CheckRun.GetName(),
		Status:     vcs.CheckStatusCompleted,
		Conclusion: &conclusion,
	}}
	return []vcs.Event{vcs.ChecksFailed{PRID: pr, FailedChecks: failed}}, pr, nil
}

func translateCheckSuite(e *github.CheckSuiteEvent) ([]vcs.Event, int, error) {
	if e.GetAction() != "completed" || e.CheckSuite == nil {
		return nil, 0, nil
	}

	pr := 0
	if len(e.CheckSuite.PullRequests) > 0 && e.CheckSuite.PullRequests[0].Number != nil {
		pr = *e.CheckSuite.PullRequests[0].Number
	} else {
		return nil, 0, nil
	}

	switch e.CheckSuite.GetConclusion() {
	case "success":
		return nil, pr, nil
	case "failure":
		conclusion := vcs.CheckConclusionFailure
		return []vcs.Event{vcs.ChecksFailed{PRID: pr, FailedChecks: []vcs.CheckResult{checkSuiteResult(e.CheckSuite, conclusion)}}}, pr, nil
	case "timed_out":
		conclusion := vcs.CheckConclusionTimedOut
		return []vcs.Event{vcs.ChecksFailed{PRID: pr, FailedChecks: []vcs.CheckResult{checkSuiteResult(e.CheckSuite, conclusion)}}}, pr, nil
	default:
		return nil, pr, nil
	}
}

func checkSuiteResult(suite *github.CheckSuite, conclusion vcs.CheckConclusion) vcs.CheckResult {
	result := vcs.CheckResult{
		Status:     vcs.CheckStatusCompleted,
		Conclusion: &conclusion,
	}
	if id := suite.GetID(); id != 0 {
		result.ID = strconv.FormatInt(id, 10)
	}
	if app := suite.GetApp(); app != nil {
		result.Name = app.GetName()
	}
	if status := suite.GetStatus(); status != "" {
		result.Status = mapCheckStatus(status)
	}
	return result
}

func mapCheckStatus(status string) vcs.CheckStatus {
	switch status {
	case "queued":
		return vcs.CheckStatusQueued
	case "in_progress":
		return vcs.CheckStatusInProgress
	case "completed":
		return vcs.CheckStatusCompleted
	default:
		return vcs.CheckStatusCompleted
	}
}
