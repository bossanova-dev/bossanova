package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

type restPRRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type restPullRequest struct {
	pullRequestMergeability
	State        string    `json:"state"`
	Merged       bool      `json:"merged"`
	Draft        bool      `json:"draft"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	Head         restPRRef `json:"head"`
	Base         restPRRef `json:"base"`
}

func (p *Provider) readRESTPullRequest(ctx context.Context, repoPath string, prID int) (restPullRequest, error) {
	var raw restPullRequest
	out, err := p.ghConditional(ctx, fmt.Sprintf("repos/%s/pulls/%d", repoFlag(repoPath), prID))
	if err != nil {
		return raw, err
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return raw, fmt.Errorf("parse REST pull request: %w", err)
	}
	return raw, nil
}

func (p *Provider) fetchPRStatusREST(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	rest, err := p.readRESTPullRequest(ctx, repoPath, prID)
	if err != nil {
		return nil, err
	}
	raw := prStatusFields{
		State: strings.ToUpper(rest.State), IsDraft: rest.Draft, Title: rest.Title, Body: rest.Body,
		Additions: rest.Additions, Deletions: rest.Deletions, ChangedFiles: rest.ChangedFiles,
		HeadRefName: rest.Head.Ref, HeadRefOid: rest.Head.SHA, BaseRefName: rest.Base.Ref,
		IsCrossRepository: rest.Head.Repo == nil || rest.Base.Repo == nil || rest.Head.Repo.FullName != rest.Base.Repo.FullName,
	}
	if rest.Merged {
		raw.State = "MERGED"
	}
	reviews, err := p.listRawReviews(ctx, repoPath, prID)
	if err != nil {
		return nil, err
	}
	for _, r := range reviews {
		review := prReview{ID: r.NodeID, State: r.State, AuthorAssociation: r.AuthorAssociation}
		review.Author.Login = r.User.Login
		raw.Reviews = append(raw.Reviews, review)
	}
	trusted := func(r prReview) bool {
		if vcs.IsTrustedReviewAuthor(r.Author.Login, r.AuthorAssociation) {
			return true
		}
		p.logUntrustedReview(prID, r.Author.Login, r.AuthorAssociation, "pr status (rest)")
		return false
	}
	status := buildPRStatus(raw, &rest.pullRequestMergeability, trusted)
	// REST cannot establish the aggregate review decision. Keep it unknown;
	// display and automation must not infer satisfied approval from reviews alone.
	status.Degraded = true
	return status, nil
}

type restCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CheckSuite struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
	Output struct {
		Title string `json:"title"`
	} `json:"output"`
}

type restCommitStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
}

func (p *Provider) fetchCheckSetREST(ctx context.Context, repoPath string, prID int) (vcs.CheckSet, error) {
	rest, err := p.readRESTPullRequest(ctx, repoPath, prID)
	if err != nil {
		return vcs.CheckSet{}, err
	}
	if rest.Head.SHA == "" {
		return vcs.CheckSet{}, errors.New("REST pull request has no head SHA")
	}
	path := fmt.Sprintf("repos/%s/commits/%s", repoFlag(repoPath), rest.Head.SHA)
	out, err := p.ghConditionalSinglePage(ctx, path+"/check-runs?per_page=100")
	if err != nil {
		return vcs.CheckSet{}, err
	}
	pages, err := decodePaginatedObjects[struct {
		CheckRuns []restCheckRun `json:"check_runs"`
	}](out, "check runs")
	if err != nil {
		return vcs.CheckSet{}, err
	}
	out, err = p.ghConditionalSinglePage(ctx, path+"/status?per_page=100")
	if err != nil {
		return vcs.CheckSet{}, err
	}
	statuses, err := decodePaginatedObjects[struct {
		Statuses []restCommitStatus `json:"statuses"`
	}](out, "commit statuses")
	if err != nil {
		return vcs.CheckSet{}, err
	}
	workflows, err := p.checkRunWorkflows(ctx, repoPath, rest.Head.SHA)
	if err != nil {
		return vcs.CheckSet{}, err
	}
	var rows []checkRow
	for _, page := range pages {
		for _, r := range page.CheckRuns {
			state := strings.ToUpper(r.Status)
			if r.Status == "completed" {
				state = strings.ToUpper(r.Conclusion)
			}
			if r.Status == "requested" {
				state = "QUEUED"
			}
			rows = append(rows, checkRow{Name: r.Name, State: state, Workflow: workflows[r.CheckSuite.ID], Description: r.Output.Title})
		}
	}
	for _, page := range statuses {
		for _, r := range page.Statuses {
			rows = append(rows, checkRow{Name: r.Context, State: strings.ToUpper(r.State), Description: r.Description})
		}
	}
	set := p.buildCheckSet(rows)
	set.Degraded = true
	return set, nil
}

func (p *Provider) checkRunWorkflows(ctx context.Context, repoPath, sha string) (map[int64]string, error) {
	out, err := p.ghConditional(ctx, workflowRunsPath(repoFlag(repoPath), sha))
	if err != nil {
		return nil, err
	}
	var raw struct {
		TotalCount   int `json:"total_count"`
		WorkflowRuns []struct {
			Name         string `json:"name"`
			CheckSuiteID int64  `json:"check_suite_id"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse check workflows: %w", err)
	}
	if len(raw.WorkflowRuns) >= workflowRunsPageSize || raw.TotalCount > len(raw.WorkflowRuns) {
		return nil, errors.New("check workflows may be truncated")
	}
	workflows := make(map[int64]string, len(raw.WorkflowRuns))
	for _, r := range raw.WorkflowRuns {
		workflows[r.CheckSuiteID] = r.Name
	}
	return workflows, nil
}

// decodePaginatedObjects reads the object per page emitted by gh api --paginate.
func decodePaginatedObjects[T any](out, what string) ([]T, error) {
	var pages []T
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var page T
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", what, err)
		}
		pages = append(pages, page)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("parse %s: empty response", what)
	}
	return pages, nil
}
