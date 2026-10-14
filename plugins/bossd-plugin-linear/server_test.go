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
	calls   int
}

func (f *fakeHost) ListOpenPRs(_ context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	f.calls++
	f.gotRepo = repoOriginURL
	// prs is returned alongside err on purpose: a caller must not trust a
	// listing that came back with an error.
	return f.prs, f.err
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

// By default a host/GitHub failure fails the call: the New Session picker
// reads a zero PR number as "no PR, start a fresh branch", so an unknown PR
// state must not look like "no PR".
func TestListAvailableIssues_WrapsHostErrorByDefault(t *testing.T) {
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

// A caller that skips PR matching (the tracker resolver, which only needs the
// issue id and URL) gets the issues Linear returned without PR fields and
// without any open-PR listing, so a slow or failing GitHub listing can neither
// delay nor fail the call. The host would both fail and offer a matching PR,
// so a listing that happened would show up as an error or a PR annotation as
// well as in the call count.
func TestListAvailableIssues_SkipPRMatchingMakesNoListing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "host would fail", err: errors.New("offline")},
		{name: "host would succeed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeHost{
				err: tc.err,
				prs: []*bossanovav1.PRSummary{{Number: 42, HeadBranch: "eng-1-fix-login"}},
			}
			fetcher := &fakeFetcher{issues: []linearIssue{{
				Identifier: "ENG-1",
				Title:      "Fix login bug",
				BranchName: "eng-1-fix-login",
				URL:        "https://linear.app/issue/ENG-1",
			}}}
			s := newTestServer(host, fetcher)

			resp, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
				RepoOriginUrl: "https://github.com/acme/repo",
				Config:        map[string]string{"linear_api_key": "k", "pr_matching": "skip"},
			})
			if err != nil {
				t.Fatalf("ListAvailableIssues returned error: %v", err)
			}
			if host.calls != 0 {
				t.Errorf("ListOpenPRs called %d times, want 0 when PR matching is skipped", host.calls)
			}
			issues := resp.GetIssues()
			if len(issues) != 1 {
				t.Fatalf("got %d issues, want 1", len(issues))
			}
			got := issues[0]
			if got.GetExternalId() != "ENG-1" || got.GetUrl() != "https://linear.app/issue/ENG-1" {
				t.Errorf("issue = (%q, %q), want (ENG-1, its URL)", got.GetExternalId(), got.GetUrl())
			}
			if got.GetPrNumber() != 0 || got.GetExistingBranch() != "" {
				t.Errorf("PR annotation = (%d, %q), want none", got.GetPrNumber(), got.GetExistingBranch())
			}
		})
	}
}

// Only the exact skip value opts out: any other pr_matching value (including
// the retired "optional") keeps the strict default, so a host failure still
// fails the call.
func TestListAvailableIssues_OtherPRMatchingValuesStayStrict(t *testing.T) {
	for _, value := range []string{"", "optional", "SKIP"} {
		t.Run("value="+value, func(t *testing.T) {
			host := &fakeHost{err: errors.New("offline")}
			fetcher := &fakeFetcher{issues: []linearIssue{{Identifier: "ENG-1"}}}
			s := newTestServer(host, fetcher)

			_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
				RepoOriginUrl: "https://github.com/acme/repo",
				Config:        map[string]string{"linear_api_key": "k", "pr_matching": value},
			})
			if err == nil || !strings.Contains(err.Error(), "list open PRs") {
				t.Fatalf("expected wrapped host error, got %v", err)
			}
			if host.calls != 1 {
				t.Errorf("ListOpenPRs called %d times, want 1", host.calls)
			}
		})
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
