package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// mustEvent decodes a latest-event JSON body into a *sentryEvent or fails.
func mustEvent(t *testing.T, body string) *sentryEvent {
	t.Helper()
	var e sentryEvent
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode event fixture: %v", err)
	}
	return &e
}

// stubSentry is a configurable fake Sentry API for the issues-list and
// latest-event endpoints. It records the query params the issues list was
// called with so query-building can be asserted.
type stubSentry struct {
	issuesStatus int
	issuesBody   string
	eventStatus  int
	eventBody    string
	// eventHang makes the latest-event endpoint block until the request is
	// cancelled, simulating a stalled enrichment fetch.
	eventHang bool

	gotIssuesQuery url.Values
	gotIssuesPath  string
	gotEventPaths  []string
	mu             sync.Mutex
}

func (s *stubSentry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events/latest/"):
			s.mu.Lock()
			s.gotEventPaths = append(s.gotEventPaths, r.URL.Path)
			s.mu.Unlock()
			if s.eventHang {
				<-r.Context().Done() // block until the enrichment budget cancels us
				return
			}
			status := s.eventStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(s.eventBody))
		case strings.HasSuffix(r.URL.Path, "/issues/"):
			s.mu.Lock()
			s.gotIssuesQuery = r.URL.Query()
			s.gotIssuesPath = r.URL.Path
			s.mu.Unlock()
			status := s.issuesStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(s.issuesBody))
		default:
			http.NotFound(w, r)
		}
	}
}

// newStubClient starts a stub server and returns a sentryClient pointed at it.
func newStubClient(t *testing.T, stub *stubSentry) *sentryClient {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c := newSentryClient("test-token", "acme")
	c.baseURL = srv.URL
	return c
}

const oneIssueBody = `[
  {
    "id": "123",
    "shortId": "WEB-1A",
    "title": "TypeError: undefined is not a function",
    "culprit": "app/main.js in handleClick",
    "permalink": "https://acme.sentry.io/issues/123/",
    "level": "error",
    "status": "unresolved",
    "count": "42",
    "userCount": 7,
    "firstSeen": "2026-06-01T00:00:00Z",
    "lastSeen": "2026-06-12T00:00:00Z",
    "metadata": {"type": "TypeError", "value": "undefined is not a function"},
    "project": {"slug": "bossanova-web", "name": "Web"}
  }
]`

const eventBody = `{
  "entries": [
    {"type": "exception", "data": {"values": [{"stacktrace": {"frames": [
      {"filename": "app/outer.js", "function": "outer", "lineNo": 10},
      {"filename": "app/inner.js", "function": "inner", "lineNo": 20}
    ]}}]}}
  ],
  "tags": [
    {"key": "environment", "value": "production"},
    {"key": "release", "value": "1.2.3"},
    {"key": "irrelevant", "value": "ignored"}
  ]
}`

func TestListIssues_BuildsUnresolvedQuery(t *testing.T) {
	cases := []struct {
		name      string
		userQuery string
		wantQuery string
	}{
		{"empty query defaults to unresolved", "", "is:unresolved"},
		{"user query is appended", "boom", "is:unresolved boom"},
		{"whitespace is trimmed", "  login  ", "is:unresolved login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubSentry{issuesBody: "[]"}
			c := newStubClient(t, stub)

			if _, err := c.ListIssues(context.Background(), tc.userQuery); err != nil {
				t.Fatalf("ListIssues error: %v", err)
			}
			if got := stub.gotIssuesQuery.Get("query"); got != tc.wantQuery {
				t.Errorf("query = %q, want %q", got, tc.wantQuery)
			}
			if got := stub.gotIssuesQuery.Get("sort"); got != "date" {
				t.Errorf("sort = %q, want date", got)
			}
			if got := stub.gotIssuesQuery.Get("limit"); got != "100" {
				t.Errorf("limit = %q, want 100", got)
			}
			if got := stub.gotIssuesQuery.Get("statsPeriod"); got != "14d" {
				t.Errorf("statsPeriod = %q, want 14d", got)
			}
			// project=-1 lists issues across every project in the org.
			if got := stub.gotIssuesQuery.Get("project"); got != "-1" {
				t.Errorf("project = %q, want -1 (all projects)", got)
			}
		})
	}
}

func TestListIssues_MapsIssueToTrackerIssue(t *testing.T) {
	stub := &stubSentry{issuesBody: oneIssueBody, eventBody: eventBody}
	c := newStubClient(t, stub)

	issues, err := c.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatalf("ListIssues error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	got := issues[0]
	if got.GetExternalId() != "WEB-1A" {
		t.Errorf("ExternalId = %q, want WEB-1A (shortId)", got.GetExternalId())
	}
	if got.GetTitle() != "TypeError: undefined is not a function" {
		t.Errorf("Title = %q", got.GetTitle())
	}
	if got.GetUrl() != "https://acme.sentry.io/issues/123/" {
		t.Errorf("Url = %q, want permalink", got.GetUrl())
	}
	if got.GetState() != "unresolved" {
		t.Errorf("State = %q, want unresolved", got.GetState())
	}
	if got.GetBranchName() != "fix/web-1a" {
		t.Errorf("BranchName = %q, want fix/web-1a", got.GetBranchName())
	}
	// The markdown body carries the enriched description.
	if !strings.Contains(got.GetDescription(), "### Latest stacktrace") {
		t.Errorf("Description missing stacktrace section:\n%s", got.GetDescription())
	}
	// The project slug identifies which service the issue belongs to in a
	// multi-project org.
	if !strings.Contains(got.GetDescription(), "**Project:** bossanova-web") {
		t.Errorf("Description missing project slug:\n%s", got.GetDescription())
	}
	// Issues are listed via the org-level endpoint (all projects), not a
	// project-scoped one.
	if want := "/api/0/organizations/acme/issues/"; stub.gotIssuesPath != want {
		t.Errorf("issues path = %q, want %q (org-level)", stub.gotIssuesPath, want)
	}
	if len(stub.gotEventPaths) != 1 {
		t.Fatalf("got %d event fetches, want 1", len(stub.gotEventPaths))
	}
	if want := "/api/0/organizations/acme/issues/123/events/latest/"; stub.gotEventPaths[0] != want {
		t.Errorf("event path = %q, want %q", stub.gotEventPaths[0], want)
	}
}

func TestListIssues_ToleratesLatestEventFailure(t *testing.T) {
	stub := &stubSentry{issuesBody: oneIssueBody, eventStatus: http.StatusInternalServerError}
	c := newStubClient(t, stub)

	issues, err := c.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatalf("ListIssues should tolerate event failure, got error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	// The issue is still returned, just without the stacktrace section.
	if strings.Contains(issues[0].GetDescription(), "### Latest stacktrace") {
		t.Errorf("stacktrace section should be omitted when the event fetch fails:\n%s", issues[0].GetDescription())
	}
	// Non-stacktrace metadata is still present.
	if !strings.Contains(issues[0].GetDescription(), "**Culprit:**") {
		t.Errorf("Description should still carry metadata:\n%s", issues[0].GetDescription())
	}
}

func TestListIssues_MapsHTTPErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, "event:read"},
		{"forbidden", http.StatusForbidden, "event:read"},
		{"not found", http.StatusNotFound, "organization slug"},
		{"rate limited", http.StatusTooManyRequests, "rate-limited"},
		{"server error", http.StatusBadGateway, "status 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubSentry{issuesStatus: tc.status, issuesBody: `{"detail":"nope"}`}
			c := newStubClient(t, stub)

			_, err := c.ListIssues(context.Background(), "")
			if err == nil {
				t.Fatalf("expected error for status %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRenderMarkdown_Golden(t *testing.T) {
	issue := sentryIssue{
		ID:        "123",
		ShortID:   "WEB-1A",
		Title:     "TypeError: undefined is not a function",
		Culprit:   "app/main.js in handleClick",
		Permalink: "https://acme.sentry.io/issues/123/",
		Level:     "error",
		Status:    "unresolved",
		Count:     "42",
		UserCount: 7,
		FirstSeen: "2026-06-01T00:00:00Z",
		LastSeen:  "2026-06-12T00:00:00Z",
	}
	issue.Project.Slug = "bossanova-web"
	// Build the event via JSON so the frame/tag shapes match the wire format.
	parsed := mustEvent(t, eventBody)

	want := "**Project:** bossanova-web   **Culprit:** app/main.js in handleClick   **Level:** error   **Events:** 42   **Users:** 7\n" +
		"**First seen:** 2026-06-01T00:00:00Z   **Last seen:** 2026-06-12T00:00:00Z\n" +
		"**Permalink:** https://acme.sentry.io/issues/123/\n" +
		"\n### Latest stacktrace\n" +
		"```\n" +
		"app/inner.js:inner:20\n" +
		"app/outer.js:outer:10\n" +
		"```\n" +
		"\n### Tags\n" +
		"- environment: production\n" +
		"- release: 1.2.3"

	if got := renderMarkdown(issue, parsed); got != want {
		t.Errorf("renderMarkdown mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

const twoIssuesBody = `[
  {"id": "1", "shortId": "WEB-1A", "title": "first", "permalink": "https://s/1/", "status": "unresolved", "culprit": "a"},
  {"id": "2", "shortId": "WEB-2B", "title": "second", "permalink": "https://s/2/", "status": "unresolved", "culprit": "b"}
]`

func TestListIssues_PreservesOrderWithConcurrentEnrichment(t *testing.T) {
	stub := &stubSentry{issuesBody: twoIssuesBody, eventBody: eventBody}
	c := newStubClient(t, stub)

	issues, err := c.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatalf("ListIssues error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	// Concurrent enrichment must not reorder the list.
	if issues[0].GetExternalId() != "WEB-1A" || issues[1].GetExternalId() != "WEB-2B" {
		t.Errorf("order not preserved: %q, %q", issues[0].GetExternalId(), issues[1].GetExternalId())
	}
	for _, is := range issues {
		if !strings.Contains(is.GetDescription(), "### Latest stacktrace") {
			t.Errorf("issue %s missing stacktrace enrichment", is.GetExternalId())
		}
	}
}

// TestListIssues_ReturnsDespiteStalledEnrichment verifies a hung latest-event
// fetch cannot block the issue list past the enrichment budget. With a
// background (deadline-free) caller context, the only thing that unblocks the
// call is the client's own budget — without it this test would hang until the
// go-test timeout.
func TestListIssues_ReturnsDespiteStalledEnrichment(t *testing.T) {
	stub := &stubSentry{issuesBody: twoIssuesBody, eventHang: true}
	c := newStubClient(t, stub)
	c.enrichBudget = 50 * time.Millisecond

	issues, err := c.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatalf("ListIssues should return despite stalled enrichment, got error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	// Base list data survives; only the (timed-out) stacktrace is missing.
	if issues[0].GetExternalId() != "WEB-1A" || issues[1].GetExternalId() != "WEB-2B" {
		t.Errorf("order not preserved: %q, %q", issues[0].GetExternalId(), issues[1].GetExternalId())
	}
	for _, is := range issues {
		if strings.Contains(is.GetDescription(), "### Latest stacktrace") {
			t.Errorf("issue %s should have no stacktrace when enrichment stalls:\n%s", is.GetExternalId(), is.GetDescription())
		}
		if !strings.Contains(is.GetDescription(), "**Culprit:**") {
			t.Errorf("issue %s should still carry base metadata:\n%s", is.GetExternalId(), is.GetDescription())
		}
	}
}

func TestFormatFrame_SkipsEmptyAndFormats(t *testing.T) {
	if got := formatFrame(sentryFrame{LineNo: 42}); got != "" {
		t.Errorf("frame with only a line number = %q, want empty (skipped)", got)
	}
	if got := formatFrame(sentryFrame{Function: "fn", LineNo: 7}); got != "fn:7" {
		t.Errorf("function-only frame = %q, want fn:7", got)
	}
	if got := formatFrame(sentryFrame{Filename: "a.js", Function: "fn", LineNo: 3}); got != "a.js:fn:3" {
		t.Errorf("full frame = %q, want a.js:fn:3", got)
	}
}

// frameFns returns the Function field of each frame, for order assertions.
func frameFns(frames []sentryFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Function
	}
	return out
}

func TestExtractFrames(t *testing.T) {
	cases := []struct {
		name  string
		event *sentryEvent
		want  []string // expected Function names, crash-site (innermost) first
	}{
		{
			name:  "nil event",
			event: nil,
			want:  nil,
		},
		{
			name:  "no stacktrace entries",
			event: mustEvent(t, `{"entries": [{"type": "message", "data": {}}]}`),
			want:  nil,
		},
		{
			name: "malformed entry data is skipped",
			event: mustEvent(t, `{"entries": [
			  {"type": "exception", "data": "not-an-object"}
			]}`),
			want: nil,
		},
		{
			name: "exception frames reversed to crash-site first",
			event: mustEvent(t, `{"entries": [
			  {"type": "exception", "data": {"values": [{"stacktrace": {"frames": [
			    {"function": "outer"},
			    {"function": "inner"}
			  ]}}]}}
			]}`),
			want: []string{"inner", "outer"},
		},
		{
			name: "stacktrace entry type is handled",
			event: mustEvent(t, `{"entries": [
			  {"type": "stacktrace", "data": {"frames": [
			    {"function": "a"},
			    {"function": "b"}
			  ]}}
			]}`),
			want: []string{"b", "a"},
		},
		{
			name: "frames from multiple entries are combined",
			event: mustEvent(t, `{"entries": [
			  {"type": "exception", "data": {"values": [{"stacktrace": {"frames": [
			    {"function": "ex"}
			  ]}}]}},
			  {"type": "stacktrace", "data": {"frames": [
			    {"function": "st"}
			  ]}}
			]}`),
			want: []string{"st", "ex"},
		},
		{
			name: "keeps innermost maxStackFrames then reverses",
			event: mustEvent(t, `{"entries": [
			  {"type": "exception", "data": {"values": [{"stacktrace": {"frames": [
			    {"function": "f0"},
			    {"function": "f1"},
			    {"function": "f2"},
			    {"function": "f3"},
			    {"function": "f4"},
			    {"function": "f5"},
			    {"function": "f6"}
			  ]}}]}}
			]}`),
			// 7 frames outer->inner; keep innermost 5 (f2..f6), reverse so the
			// crash site (f6) leads.
			want: []string{"f6", "f5", "f4", "f3", "f2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := frameFns(extractFrames(tc.event))
			if len(got) != len(tc.want) {
				t.Fatalf("extractFrames functions = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("extractFrames functions = %v, want %v", got, tc.want)
				}
			}
			if len(tc.want) > maxStackFrames {
				t.Fatalf("test bug: want exceeds maxStackFrames (%d)", maxStackFrames)
			}
		})
	}
}

func TestExtractTags(t *testing.T) {
	cases := []struct {
		name  string
		event *sentryEvent
		want  [][2]string
	}{
		{
			name:  "nil event",
			event: nil,
			want:  nil,
		},
		{
			name: "returns useful keys in display order, dropping unknown and empty",
			event: mustEvent(t, `{"tags": [
			  {"key": "release", "value": "1.2.3"},
			  {"key": "irrelevant", "value": "drop-me"},
			  {"key": "environment", "value": "production"},
			  {"key": "server_name", "value": ""}
			]}`),
			// usefulTagKeys order is environment, release, server_name, transaction;
			// server_name is empty (skipped), transaction is absent, irrelevant is
			// not a useful key.
			want: [][2]string{
				{"environment", "production"},
				{"release", "1.2.3"},
			},
		},
		{
			name: "duplicate keys collapse to the last value",
			event: mustEvent(t, `{"tags": [
			  {"key": "environment", "value": "staging"},
			  {"key": "environment", "value": "production"}
			]}`),
			want: [][2]string{{"environment", "production"}},
		},
		{
			name:  "no useful tags yields nil",
			event: mustEvent(t, `{"tags": [{"key": "irrelevant", "value": "x"}]}`),
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractTags(tc.event)
			if len(got) != len(tc.want) {
				t.Fatalf("extractTags = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("extractTags = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestBranchSlug(t *testing.T) {
	cases := map[string]string{
		"WEB-1A":     "fix/web-1a",
		"my_proj-2b": "fix/my-proj-2b",
		"  PROJ-9  ": "fix/proj-9",
		"":           "",
		"weird id!!": "fix/weird-id",
	}
	for in, want := range cases {
		if got := branchSlug(in); got != want {
			t.Errorf("branchSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
