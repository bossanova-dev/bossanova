package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GitHubParser verifies and parses GitHub webhook payloads.
type GitHubParser struct{}

var _ Parser = (*GitHubParser)(nil)

func (p *GitHubParser) Provider() string { return "github" }

// VerifySignature checks the X-Hub-Signature-256 header against the secret.
func (p *GitHubParser) VerifySignature(r *http.Request, body []byte, secret string) error {
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}

	if !strings.HasPrefix(sig, "sha256=") {
		return fmt.Errorf("invalid signature format")
	}

	expected, err := hex.DecodeString(sig[7:])
	if err != nil {
		return fmt.Errorf("invalid signature hex: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	actual := mac.Sum(nil)

	if !hmac.Equal(expected, actual) {
		return fmt.Errorf("signature mismatch")
	}

	return nil
}

// Parse converts a GitHub webhook payload into a standard VCS event.
// Returns nil event for event types we don't handle.
func (p *GitHubParser) Parse(r *http.Request, body []byte) (*ParsedEvent, error) {
	eventType := r.Header.Get("X-GitHub-Event")
	if eventType == "" {
		return nil, fmt.Errorf("missing X-GitHub-Event header")
	}

	switch eventType {
	case "check_suite":
		return p.parseCheckSuite(body)
	case "check_run":
		return p.parseCheckRun(body)
	case "pull_request":
		return p.parsePullRequest(body)
	case "pull_request_review":
		return p.parsePullRequestReview(body)
	default:
		// Event type not relevant — ignore.
		return nil, nil
	}
}

// --- GitHub payload types ---

type ghRepo struct {
	HTMLURL string `json:"html_url"`
}

type ghCheckSuitePayload struct {
	Action     string `json:"action"`
	CheckSuite struct {
		ID           int64  `json:"id"`
		Conclusion   string `json:"conclusion"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	Repository ghRepo `json:"repository"`
}

type ghCheckRunPayload struct {
	Action   string `json:"action"`
	CheckRun struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	Repository ghRepo `json:"repository"`
}

type ghPullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number    int    `json:"number"`
		Mergeable *bool  `json:"mergeable"`
		State     string `json:"state"`
		Merged    bool   `json:"merged"`
	} `json:"pull_request"`
	Repository ghRepo `json:"repository"`
}

type ghPullRequestReviewPayload struct {
	Action string `json:"action"`
	Review struct {
		State string `json:"state"`
		Body  string `json:"body"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Repository ghRepo `json:"repository"`
}

func (p *GitHubParser) parseCheckSuite(body []byte) (*ParsedEvent, error) {
	var payload ghCheckSuitePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse check_suite: %w", err)
	}

	if payload.Action != "completed" {
		return nil, nil
	}

	if len(payload.CheckSuite.PullRequests) == 0 {
		return nil, nil
	}

	prNumber := payload.CheckSuite.PullRequests[0].Number
	repoURL := payload.Repository.HTMLURL

	switch payload.CheckSuite.Conclusion {
	case "success":
		return &ParsedEvent{
			RepoOriginURL: repoURL,
			Event: &pb.VCSEvent{
				Event: &pb.VCSEvent_ChecksPassed{
					ChecksPassed: &pb.ChecksPassedEvent{PrId: int32(prNumber)},
				},
			},
		}, nil
	case "failure", "timed_out":
		return &ParsedEvent{
			RepoOriginURL: repoURL,
			Event: &pb.VCSEvent{
				Event: &pb.VCSEvent_ChecksFailed{
					ChecksFailed: &pb.ChecksFailedEvent{PrId: int32(prNumber)},
				},
			},
		}, nil
	default:
		return nil, nil
	}
}

func (p *GitHubParser) parseCheckRun(body []byte) (*ParsedEvent, error) {
	var payload ghCheckRunPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse check_run: %w", err)
	}

	if payload.Action != "completed" {
		return nil, nil
	}

	if len(payload.CheckRun.PullRequests) == 0 {
		return nil, nil
	}

	prNumber := payload.CheckRun.PullRequests[0].Number
	repoURL := payload.Repository.HTMLURL

	switch payload.CheckRun.Conclusion {
	case "failure", "timed_out":
		return &ParsedEvent{
			RepoOriginURL: repoURL,
			Event: &pb.VCSEvent{
				Event: &pb.VCSEvent_ChecksFailed{
					ChecksFailed: &pb.ChecksFailedEvent{
						PrId: int32(prNumber),
						FailedChecks: []*pb.CheckResult{{
							Id:         fmt.Sprintf("%d", payload.CheckRun.ID),
							Name:       payload.CheckRun.Name,
							Status:     pb.CheckStatus_CHECK_STATUS_COMPLETED,
							Conclusion: conclusionPtr(pb.CheckConclusion_CHECK_CONCLUSION_FAILURE),
						}},
					},
				},
			},
		}, nil
	default:
		// Individual check_run successes don't aggregate well. Rely on check_suite
		// for the "all passed" signal.
		return nil, nil
	}
}

func (p *GitHubParser) parsePullRequest(body []byte) (*ParsedEvent, error) {
	var payload ghPullRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse pull_request: %w", err)
	}

	prNumber := payload.PullRequest.Number
	repoURL := payload.Repository.HTMLURL

	switch payload.Action {
	case "closed":
		if payload.PullRequest.Merged {
			return &ParsedEvent{
				RepoOriginURL: repoURL,
				Event: &pb.VCSEvent{
					Event: &pb.VCSEvent_PrMerged{
						PrMerged: &pb.PRMergedEvent{PrId: int32(prNumber)},
					},
				},
			}, nil
		}
		return &ParsedEvent{
			RepoOriginURL: repoURL,
			Event: &pb.VCSEvent{
				Event: &pb.VCSEvent_PrClosed{
					PrClosed: &pb.PRClosedEvent{PrId: int32(prNumber)},
				},
			},
		}, nil
	case "synchronize":
		// Check for merge conflicts on push.
		if payload.PullRequest.Mergeable != nil && !*payload.PullRequest.Mergeable {
			return &ParsedEvent{
				RepoOriginURL: repoURL,
				Event: &pb.VCSEvent{
					Event: &pb.VCSEvent_ConflictDetected{
						ConflictDetected: &pb.ConflictDetectedEvent{PrId: int32(prNumber)},
					},
				},
			}, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func (p *GitHubParser) parsePullRequestReview(body []byte) (*ParsedEvent, error) {
	var payload ghPullRequestReviewPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse pull_request_review: %w", err)
	}

	if payload.Action != "submitted" {
		return nil, nil
	}

	prNumber := payload.PullRequest.Number
	repoURL := payload.Repository.HTMLURL

	reviewState := mapReviewState(payload.Review.State)

	return &ParsedEvent{
		RepoOriginURL: repoURL,
		Event: &pb.VCSEvent{
			Event: &pb.VCSEvent_ReviewSubmitted{
				ReviewSubmitted: &pb.ReviewSubmittedEvent{
					PrId: int32(prNumber),
					Comments: []*pb.ReviewComment{{
						Author: payload.Review.User.Login,
						Body:   payload.Review.Body,
						State:  reviewState,
					}},
				},
			},
		},
	}, nil
}

func mapReviewState(state string) pb.ReviewState {
	switch state {
	case "approved":
		return pb.ReviewState_REVIEW_STATE_APPROVED
	case "changes_requested":
		return pb.ReviewState_REVIEW_STATE_CHANGES_REQUESTED
	case "commented":
		return pb.ReviewState_REVIEW_STATE_COMMENTED
	case "dismissed":
		return pb.ReviewState_REVIEW_STATE_DISMISSED
	default:
		return pb.ReviewState_REVIEW_STATE_UNSPECIFIED
	}
}

func conclusionPtr(c pb.CheckConclusion) *pb.CheckConclusion {
	return &c
}
