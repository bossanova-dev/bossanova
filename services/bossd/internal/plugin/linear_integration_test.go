package plugin_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/vcs"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// linearHarnessOpts configures a single test's Linear plugin harness.
//
// IssuesFixture and PRsFixture are filenames under testdata/linear/. If
// IssuesFixture is empty the mock server writes no body (allowing
// LinearHTTPStatus alone to exercise error paths). If PRsFixture is empty
// the host service returns no PRs.
type linearHarnessOpts struct {
	IssuesFixture    string
	PRsFixture       string
	LinearHTTPStatus int
	AssertRequest    func(t *testing.T, r *http.Request, body []byte)
}

// linearCapturedRequest records one request seen by the mock Linear server.
// Tests use this to verify auth headers and GraphQL query bodies.
type linearCapturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

// linearHarness wires together a mock Linear GraphQL server, a host service
// backed by a testVCSProvider, and a spawned Linear plugin binary. The
// harness registers t.Cleanup for all resources so tests only need to call
// newLinearHarness and use the returned TaskSource client.
type linearHarness struct {
	t           *testing.T
	Server      *httptest.Server
	TaskSource  pluginpkg.TaskSource
	VCSProvider *testVCSProvider

	mu       sync.Mutex
	requests []linearCapturedRequest
}

// Requests returns a snapshot of every request the mock Linear server has
// observed during the test. Callers must not mutate the returned slice.
func (h *linearHarness) Requests() []linearCapturedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]linearCapturedRequest, len(h.requests))
	copy(out, h.requests)
	return out
}

func newLinearHarness(t *testing.T, opts linearHarnessOpts) *linearHarness {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping Linear plugin integration test in short mode")
	}

	h := &linearHarness{t: t}

	var issuesBody []byte
	if opts.IssuesFixture != "" {
		body, err := os.ReadFile(filepath.Join("testdata", "linear", opts.IssuesFixture))
		if err != nil {
			t.Fatalf("read issues fixture %q: %v", opts.IssuesFixture, err)
		}
		issuesBody = body
	}

	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}

		h.mu.Lock()
		h.requests = append(h.requests, linearCapturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		})
		h.mu.Unlock()

		if opts.AssertRequest != nil {
			opts.AssertRequest(t, r, body)
		}

		w.Header().Set("Content-Type", "application/json")
		if opts.LinearHTTPStatus != 0 {
			w.WriteHeader(opts.LinearHTTPStatus)
		}
		if issuesBody != nil {
			_, _ = w.Write(issuesBody)
		}
	}))
	t.Cleanup(h.Server.Close)

	prs := []vcs.PRSummary{}
	if opts.PRsFixture != "" {
		body, err := os.ReadFile(filepath.Join("testdata", "linear", opts.PRsFixture))
		if err != nil {
			t.Fatalf("read PRs fixture %q: %v", opts.PRsFixture, err)
		}
		if err := json.Unmarshal(body, &prs); err != nil {
			t.Fatalf("unmarshal PRs fixture: %v", err)
		}
	}
	h.VCSProvider = &testVCSProvider{prs: prs}

	hostService := pluginpkg.NewHostServiceServer(h.VCSProvider)
	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource: &taskSourceWithBroker{hostService: hostService},
	}

	// Redirect the plugin's Linear GraphQL client at the mock server. The
	// LINEAR_API_ENDPOINT override is only honoured by e2e-tagged builds of
	// the plugin — production binaries have no env-var redirect surface, so
	// this harness builds with -tags e2e. t.Setenv is safe here because
	// pluginharness.SpawnPlugin inherits the test process's environment when
	// exec'ing the plugin binary.
	t.Setenv("LINEAR_API_ENDPOINT", h.Server.URL)

	binPath := pluginharness.BuildPluginWithTags(t, "bossd-plugin-linear", "e2e")
	client := pluginharness.SpawnPlugin(t, binPath, pluginMap)

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client.Client(): %v", err)
	}

	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeTaskSource)
	if err != nil {
		t.Fatalf("dispense TaskSource: %v", err)
	}
	taskSource, ok := raw.(pluginpkg.TaskSource)
	if !ok {
		t.Fatalf("dispensed type %T does not implement TaskSource", raw)
	}
	h.TaskSource = taskSource

	return h
}

// TestE2E_Linear_HarnessHandshake verifies the harness stands up end-to-end:
// the plugin builds, spawns, completes the go-plugin handshake, and serves
// its identity over GetInfo. Subsequent TestE2E_Linear_* tests build on this
// scaffolding.
func TestE2E_Linear_HarnessHandshake(t *testing.T) {
	h := newLinearHarness(t, linearHarnessOpts{
		IssuesFixture: "issues_empty.json",
	})

	info, err := h.TaskSource.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetName() != "linear" {
		t.Errorf("plugin name = %q, want %q", info.GetName(), "linear")
	}
	if info.GetVersion() == "" {
		t.Error("plugin version should not be empty")
	}
}

// TestE2E_Linear_PollTasks locks in the Linear plugin's user-initiated
// contract: PollTasks is a no-op that returns zero tasks and never contacts
// the Linear API. Issues are surfaced on-demand via ListAvailableIssues
// (covered by TestE2E_Linear_ListAvailableIssues / _MatchesExistingPR).
//
// The plan text mentions "TaskItems with tracker_id, tracker_url,
// branch_name" — those fields live on TrackerIssue (the ListAvailableIssues
// response), not on TaskItem. server.go:42 returns an empty
// PollTasksResponse unconditionally; this test pins that behaviour so any
// future change to start polling Linear is a deliberate decision, not
// accidental.
func TestE2E_Linear_PollTasks(t *testing.T) {
	h := newLinearHarness(t, linearHarnessOpts{
		// Load a populated fixture so we can assert PollTasks does NOT
		// fetch these issues — it should short-circuit before any HTTP call.
		IssuesFixture: "issues_response.json",
		PRsFixture:    "open_prs.json",
	})

	ctx := context.Background()
	tasks, err := h.TaskSource.PollTasks(ctx, "https://github.com/recurser/bossanova")
	if err != nil {
		t.Fatalf("PollTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("PollTasks returned %d tasks, want 0 (Linear is user-initiated)", len(tasks))
	}

	// Sanity: PollTasks must not contact the Linear API.
	if got := len(h.Requests()); got != 0 {
		t.Errorf("mock Linear server saw %d request(s), want 0 — PollTasks should short-circuit", got)
	}

	// Empty-URL case: still returns empty without error.
	tasks, err = h.TaskSource.PollTasks(ctx, "")
	if err != nil {
		t.Fatalf("PollTasks (empty URL): %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("PollTasks (empty URL) returned %d tasks, want 0", len(tasks))
	}
}

// TestE2E_Linear_ListAvailableIssues verifies the full user-initiated
// issue-fetch flow: the plugin authenticates against the Linear GraphQL
// API, calls back into the host for open PRs, and maps each Linear issue
// into a TrackerIssue with the documented fields populated.
func TestE2E_Linear_ListAvailableIssues(t *testing.T) {
	h := newLinearHarness(t, linearHarnessOpts{
		IssuesFixture: "issues_response.json",
		PRsFixture:    "open_prs.json",
	})

	ctx := context.Background()
	issues, err := h.TaskSource.ListAvailableIssues(ctx,
		"https://github.com/recurser/bossanova",
		"",
		map[string]string{"linear_api_key": "lin_api_test123"},
	)
	if err != nil {
		t.Fatalf("ListAvailableIssues: %v", err)
	}

	if len(issues) != 3 {
		t.Fatalf("got %d issues, want 3", len(issues))
	}

	// Index by external ID so we don't depend on return order.
	byID := map[string]int{}
	for i, iss := range issues {
		byID[iss.GetExternalId()] = i
	}
	for _, want := range []string{"ENG-123", "ENG-124", "ENG-125"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("missing issue %s in response", want)
		}
	}

	// Spot-check a full TrackerIssue round-trips every field.
	iss123 := issues[byID["ENG-123"]]
	if iss123.GetTitle() != "Fix login bug" {
		t.Errorf("ENG-123 title = %q", iss123.GetTitle())
	}
	if iss123.GetDescription() == "" {
		t.Error("ENG-123 description should not be empty")
	}
	if iss123.GetBranchName() != "eng-123-fix-login" {
		t.Errorf("ENG-123 branch_name = %q", iss123.GetBranchName())
	}
	if iss123.GetUrl() != "https://linear.app/recurser/issue/ENG-123" {
		t.Errorf("ENG-123 url = %q", iss123.GetUrl())
	}
	if iss123.GetState() != "In Progress" {
		t.Errorf("ENG-123 state = %q", iss123.GetState())
	}

	// Request-shape assertions: the plugin must POST a GraphQL query with
	// the API key in the Authorization header (no Bearer prefix).
	reqs := h.Requests()
	if len(reqs) != 1 {
		t.Fatalf("mock Linear server saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != "POST" {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.Auth != "lin_api_test123" {
		t.Errorf("Authorization = %q, want raw API key (no Bearer prefix)", req.Auth)
	}

	var reqBody struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(req.Body, &reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if !strings.Contains(reqBody.Query, "issues") || !strings.Contains(reqBody.Query, "branchName") {
		t.Errorf("GraphQL query missing expected fields:\n%s", reqBody.Query)
	}
}

// TestE2E_Linear_MatchesExistingPR exercises both branches of matchPR:
// primary branch-name match and the [ENG-NNN] title-tag fallback, plus the
// no-match case. Fixtures are shaped specifically for this:
//   - ENG-123 branch "eng-123-fix-login" matches PR#42 by branch
//   - ENG-124 branch "eng-124-dark-mode" has no PR (neither branch nor tag)
//   - ENG-125 branch "eng-125-refactor-auth" does NOT match PR#43's branch
//     ("feature/auth-refactor") but the PR title contains "[ENG-125]"
func TestE2E_Linear_MatchesExistingPR(t *testing.T) {
	h := newLinearHarness(t, linearHarnessOpts{
		IssuesFixture: "issues_response.json",
		PRsFixture:    "open_prs.json",
	})

	ctx := context.Background()
	issues, err := h.TaskSource.ListAvailableIssues(ctx,
		"https://github.com/recurser/bossanova",
		"",
		map[string]string{"linear_api_key": "lin_api_test"},
	)
	if err != nil {
		t.Fatalf("ListAvailableIssues: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("got %d issues, want 3", len(issues))
	}

	byID := map[string]int{}
	for i, iss := range issues {
		byID[iss.GetExternalId()] = i
	}

	// Primary path: branch match wins.
	iss123 := issues[byID["ENG-123"]]
	if iss123.GetPrNumber() != 42 {
		t.Errorf("ENG-123 pr_number = %d, want 42 (branch match)", iss123.GetPrNumber())
	}
	if iss123.GetExistingBranch() != "eng-123-fix-login" {
		t.Errorf("ENG-123 existing_branch = %q, want %q", iss123.GetExistingBranch(), "eng-123-fix-login")
	}

	// Fallback path: title tag match.
	iss125 := issues[byID["ENG-125"]]
	if iss125.GetPrNumber() != 43 {
		t.Errorf("ENG-125 pr_number = %d, want 43 (title tag match)", iss125.GetPrNumber())
	}
	if iss125.GetExistingBranch() != "feature/auth-refactor" {
		t.Errorf("ENG-125 existing_branch = %q, want PR#43's branch", iss125.GetExistingBranch())
	}

	// No-match path: issue surfaces without PR linkage.
	iss124 := issues[byID["ENG-124"]]
	if iss124.GetPrNumber() != 0 {
		t.Errorf("ENG-124 pr_number = %d, want 0 (no match)", iss124.GetPrNumber())
	}
	if iss124.GetExistingBranch() != "" {
		t.Errorf("ENG-124 existing_branch = %q, want empty", iss124.GetExistingBranch())
	}
}

// TestE2E_Linear_UpdateTaskStatus pins the Linear plugin's current
// UpdateTaskStatus contract: the RPC accepts status updates for any
// TaskItemStatus value, returns success, and does NOT call out to the
// Linear API (no issue-closing, no comment-posting).
//
// Plan↔code note: the task description and the P1 plan both describe
// "issue closed + comment posted on merge", but server.go:47-54 only
// logs. The full enhancement is tracked in pay-off-technical-debt-4tw;
// this test documents today's contract and will grow mutation-body
// assertions once the production code lands.
func TestE2E_Linear_UpdateTaskStatus(t *testing.T) {
	h := newLinearHarness(t, linearHarnessOpts{
		// No issues fixture: UpdateTaskStatus should not contact Linear at
		// all, so there's nothing to serve.
	})

	ctx := context.Background()
	cases := []struct {
		name    string
		extID   string
		status  bossanovav1.TaskItemStatus
		details string
	}{
		{"merged", "ENG-123", bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_COMPLETED, "PR #42 merged"},
		{"in_progress", "ENG-124", bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_IN_PROGRESS, "session started"},
		{"failed", "ENG-125", bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_FAILED, "CI broke"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.TaskSource.UpdateTaskStatus(ctx, tc.extID, tc.status, tc.details); err != nil {
				t.Fatalf("UpdateTaskStatus: %v", err)
			}
		})
	}

	// Contract check: UpdateTaskStatus must not touch Linear's API today.
	// If this assertion ever fails, update this test alongside the
	// production-code change (see pay-off-technical-debt-4tw).
	if got := len(h.Requests()); got != 0 {
		t.Errorf("Linear mock saw %d request(s), want 0 — UpdateTaskStatus should not make outbound HTTP calls yet", got)
	}
}
