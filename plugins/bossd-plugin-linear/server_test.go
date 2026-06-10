package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeHost is a stub hostClient that returns canned PRs (or an error) so
// ListAvailableIssues can be exercised without a live daemon connection.
type fakeHost struct {
	prs     []*bossanovav1.PRSummary
	err     error
	gotRepo string
}

func (f *fakeHost) ListOpenPRs(_ context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	f.gotRepo = repoOriginURL
	if f.err != nil {
		return nil, f.err
	}
	return f.prs, nil
}

// fakeFetcher is a stub issueFetcher returning canned issues (or an error),
// substituted via server.newFetcher to avoid hitting the real Linear API.
type fakeFetcher struct {
	issues    []linearIssue
	err       error
	gotAPIKey string
	gotQuery  string
}

func (f *fakeFetcher) FetchIssues(_ context.Context, titleQuery string) ([]linearIssue, error) {
	f.gotQuery = titleQuery
	if f.err != nil {
		return nil, f.err
	}
	return f.issues, nil
}

// newTestServer wires a server with the supplied fakes, bypassing the real
// Linear client factory.
func newTestServer(host hostClient, fetcher *fakeFetcher) *server {
	s := newServer(host, zerolog.Nop())
	s.newFetcher = func(apiKey string) issueFetcher {
		fetcher.gotAPIKey = apiKey
		return fetcher
	}
	return s
}

func TestGetInfo_ReportsLinearTaskSourceContract(t *testing.T) {
	s := newServer(&fakeHost{}, zerolog.Nop())

	resp, err := s.GetInfo(context.Background(), &bossanovav1.TaskSourceServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo returned error: %v", err)
	}

	info := resp.GetInfo()
	if info == nil {
		t.Fatal("GetInfo returned nil info")
	}
	if got := info.GetName(); got != "linear" {
		t.Errorf("plugin name = %q, want %q", got, "linear")
	}
	// The daemon dispatches plugins by capability string; pin the contract so a
	// rename can't silently make this plugin invisible as a task source.
	caps := info.GetCapabilities()
	if len(caps) != 1 || caps[0] != "task_source" {
		t.Errorf("capabilities = %v, want [task_source]", caps)
	}
}

func TestPollTasks_ReturnsEmptyForUserInitiatedPlugin(t *testing.T) {
	s := newServer(&fakeHost{}, zerolog.Nop())

	resp, err := s.PollTasks(context.Background(), &bossanovav1.PollTasksRequest{})
	if err != nil {
		t.Fatalf("PollTasks returned error: %v", err)
	}
	if got := len(resp.GetTasks()); got != 0 {
		t.Errorf("PollTasks returned %d tasks, want 0", got)
	}
}

func TestUpdateTaskStatus_IsNoOpSuccess(t *testing.T) {
	s := newServer(&fakeHost{}, zerolog.Nop())

	resp, err := s.UpdateTaskStatus(context.Background(), &bossanovav1.UpdateTaskStatusRequest{
		ExternalId: "ENG-1",
		Status:     bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_IN_PROGRESS,
		Details:    "working",
	})
	if err != nil {
		t.Fatalf("UpdateTaskStatus returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("UpdateTaskStatus returned nil response")
	}
}

func TestListAvailableIssues_RejectsMissingRepoURL(t *testing.T) {
	host := &fakeHost{}
	fetcher := &fakeFetcher{}
	s := newTestServer(host, fetcher)

	_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: "",
		Config:        map[string]string{"linear_api_key": "k"},
	})
	if err == nil || !strings.Contains(err.Error(), "repo_origin_url is required") {
		t.Fatalf("expected repo_origin_url validation error, got %v", err)
	}
	// Validation must short-circuit before any external call.
	if fetcher.gotAPIKey != "" {
		t.Error("fetcher should not be invoked when repo URL is missing")
	}
	if host.gotRepo != "" {
		t.Error("host should not be invoked when repo URL is missing")
	}
}

func TestListAvailableIssues_RejectsMissingAPIKey(t *testing.T) {
	host := &fakeHost{}
	fetcher := &fakeFetcher{}
	s := newTestServer(host, fetcher)

	_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: "https://github.com/acme/repo",
		Config:        map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "linear_api_key is required") {
		t.Fatalf("expected linear_api_key validation error, got %v", err)
	}
	if host.gotRepo != "" {
		t.Error("host should not be invoked when API key is missing")
	}
}

func TestListAvailableIssues_WrapsFetchError(t *testing.T) {
	host := &fakeHost{}
	fetcher := &fakeFetcher{err: errors.New("boom")}
	s := newTestServer(host, fetcher)

	_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: "https://github.com/acme/repo",
		Config:        map[string]string{"linear_api_key": "k"},
	})
	if err == nil || !strings.Contains(err.Error(), "fetch Linear issues") {
		t.Fatalf("expected wrapped fetch error, got %v", err)
	}
	// A fetch failure must abort before the host PR call.
	if host.gotRepo != "" {
		t.Error("host should not be invoked after a fetch failure")
	}
}

func TestListAvailableIssues_WrapsHostError(t *testing.T) {
	host := &fakeHost{err: errors.New("offline")}
	fetcher := &fakeFetcher{issues: []linearIssue{{Identifier: "ENG-1"}}}
	s := newTestServer(host, fetcher)

	_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: "https://github.com/acme/repo",
		Config:        map[string]string{"linear_api_key": "k"},
	})
	if err == nil || !strings.Contains(err.Error(), "list open PRs") {
		t.Fatalf("expected wrapped host error, got %v", err)
	}
}

func TestListAvailableIssues_MapsIssuesAndMatchesPRs(t *testing.T) {
	host := &fakeHost{prs: []*bossanovav1.PRSummary{
		{Number: 42, HeadBranch: "eng-1-fix-login", Title: "Fix login"},
	}}
	fetcher := &fakeFetcher{issues: []linearIssue{
		{
			Identifier:  "ENG-1",
			Title:       "Fix login bug",
			Description: "Users cannot log in",
			BranchName:  "eng-1-fix-login",
			URL:         "https://linear.app/issue/ENG-1",
			State:       "In Progress",
		},
		{
			Identifier:  "ENG-2",
			Title:       "Add dark mode",
			Description: "Toggle",
			BranchName:  "eng-2-dark-mode",
			URL:         "https://linear.app/issue/ENG-2",
			State:       "Todo",
		},
	}}
	s := newTestServer(host, fetcher)

	resp, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: "https://github.com/acme/repo",
		Config:        map[string]string{"linear_api_key": "secret"},
		Query:         "login",
	})
	if err != nil {
		t.Fatalf("ListAvailableIssues returned error: %v", err)
	}

	// Inputs are threaded through to the collaborators.
	if fetcher.gotAPIKey != "secret" {
		t.Errorf("fetcher API key = %q, want %q", fetcher.gotAPIKey, "secret")
	}
	if fetcher.gotQuery != "login" {
		t.Errorf("fetcher query = %q, want %q", fetcher.gotQuery, "login")
	}
	if host.gotRepo != "https://github.com/acme/repo" {
		t.Errorf("host repo = %q, want repo origin URL", host.gotRepo)
	}

	issues := resp.GetIssues()
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}

	// First issue: every field maps across, and the branch match attaches the PR.
	got := issues[0]
	if got.GetExternalId() != "ENG-1" {
		t.Errorf("ExternalId = %q, want ENG-1", got.GetExternalId())
	}
	if got.GetTitle() != "Fix login bug" {
		t.Errorf("Title = %q, want %q", got.GetTitle(), "Fix login bug")
	}
	if got.GetDescription() != "Users cannot log in" {
		t.Errorf("Description = %q, want %q", got.GetDescription(), "Users cannot log in")
	}
	if got.GetBranchName() != "eng-1-fix-login" {
		t.Errorf("BranchName = %q, want %q", got.GetBranchName(), "eng-1-fix-login")
	}
	if got.GetUrl() != "https://linear.app/issue/ENG-1" {
		t.Errorf("Url = %q, want issue URL", got.GetUrl())
	}
	if got.GetState() != "In Progress" {
		t.Errorf("State = %q, want %q", got.GetState(), "In Progress")
	}
	if got.GetPrNumber() != 42 {
		t.Errorf("PrNumber = %d, want 42", got.GetPrNumber())
	}
	if got.GetExistingBranch() != "eng-1-fix-login" {
		t.Errorf("ExistingBranch = %q, want matched PR branch", got.GetExistingBranch())
	}

	// Second issue has no matching PR — PR fields stay zero-valued.
	if pr := issues[1].GetPrNumber(); pr != 0 {
		t.Errorf("unmatched issue PrNumber = %d, want 0", pr)
	}
	if b := issues[1].GetExistingBranch(); b != "" {
		t.Errorf("unmatched issue ExistingBranch = %q, want empty", b)
	}
}
