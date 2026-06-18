package views

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sentryRepo returns a repo configured with both Sentry credentials, plus an
// optional Linear key.
func sentryRepo(linearKey string) *pb.Repo {
	return &pb.Repo{
		Id:                "repo-1",
		DisplayName:       "alpha",
		LocalPath:         "/path/alpha",
		DefaultBaseBranch: "main",
		LinearApiKey:      linearKey,
		SentryApiKey:      "sntrys_abc123",
		SentryOrg:         "acme",
	}
}

func TestNewSession_SentryOptionHiddenWithoutFullConfig(t *testing.T) {
	cases := map[string]*pb.Repo{
		"no sentry config": {Id: "repo-1", DisplayName: "a", LocalPath: "/p", DefaultBaseBranch: "main"},
		"missing org": {
			Id: "repo-1", DisplayName: "a", LocalPath: "/p", DefaultBaseBranch: "main",
			SentryApiKey: "sntrys_abc",
		},
		"missing key": {
			Id: "repo-1", DisplayName: "a", LocalPath: "/p", DefaultBaseBranch: "main",
			SentryOrg: "acme",
		},
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			sc := &stubClient{repos: []*pb.Repo{repo}}
			m := NewNewSessionModel(sc, context.Background())
			m = sendMsg(t, m, reposMsg{repos: sc.repos})

			for _, opt := range m.buildSessionTypeOptions() {
				if opt.typ == sessionTypeSentryIssue {
					t.Fatal("Sentry option should be hidden unless both creds are set")
				}
			}
		})
	}
}

func TestNewSession_SentryOptionShownDirectlyAfterLinear(t *testing.T) {
	sc := &stubClient{repos: []*pb.Repo{sentryRepo("lin_api_abc123")}}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	opts := m.buildSessionTypeOptions()

	linearIdx, sentryIdx := -1, -1
	for i, opt := range opts {
		switch opt.typ {
		case sessionTypeLinearTicket:
			linearIdx = i
		case sessionTypeSentryIssue:
			sentryIdx = i
			if opt.label != "Fix a Sentry issue" {
				t.Errorf("Sentry option label = %q, want %q", opt.label, "Fix a Sentry issue")
			}
		default:
			// other session types are irrelevant to ordering here
		}
	}
	if linearIdx < 0 || sentryIdx < 0 {
		t.Fatalf("expected both Linear (%d) and Sentry (%d) options present", linearIdx, sentryIdx)
	}
	if sentryIdx != linearIdx+1 {
		t.Errorf("Sentry option at %d, want directly after Linear (%d)", sentryIdx, linearIdx)
	}
}

func TestNewSession_SentryOptionShownWithoutLinear(t *testing.T) {
	sc := &stubClient{repos: []*pb.Repo{sentryRepo("")}}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	opts := m.buildSessionTypeOptions()
	var foundSentry, foundLinear bool
	for _, opt := range opts {
		if opt.typ == sessionTypeSentryIssue {
			foundSentry = true
		}
		if opt.typ == sessionTypeLinearTicket {
			foundLinear = true
		}
	}
	if !foundSentry {
		t.Error("Sentry option should be shown when all Sentry creds are set")
	}
	if foundLinear {
		t.Error("Linear option should be hidden without a Linear key")
	}
}

func TestNewSession_SentryAdvanceUsesSentrySource(t *testing.T) {
	sc := &stubClient{
		repos:         []*pb.Repo{sentryRepo("")},
		trackerIssues: []*pb.TrackerIssue{{ExternalId: "WEB-1A", Title: "boom"}},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m.selectedType = sessionTypeSentryIssue

	_, cmd := m.advanceFromTypeSelect()
	if cmd == nil {
		t.Fatal("advanceFromTypeSelect returned no fetch command")
	}
	cmd() // triggers the ListTrackerIssues call on the stub
	if sc.trackerLastSource != "sentry" {
		t.Errorf("fetch source = %q, want sentry", sc.trackerLastSource)
	}
}

func TestNewSession_SentryCreatesSessionWithMarkdownPlan(t *testing.T) {
	sc := &stubClient{
		repos: []*pb.Repo{sentryRepo("")},
		trackerIssues: []*pb.TrackerIssue{
			{
				ExternalId:  "WEB-1A",
				Title:       "TypeError: undefined",
				Description: "**Culprit:** app/main.js   **Level:** error",
				State:       "unresolved",
				Url:         "https://acme.sentry.io/issues/123/",
				BranchName:  "fix/web-1a",
			},
		},
		created: &pb.Session{Id: "session-1"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m.selectedType = sessionTypeSentryIssue
	m.selectedIssue = sc.trackerIssues[0]

	cmd := m.startCreating()
	if cmd != nil {
		cmd()
	}
	req := sc.createReq
	if req == nil {
		t.Fatal("CreateSession was not called")
	}
	if req.Title != "[WEB-1A] TypeError: undefined" {
		t.Errorf("Title = %q, want [WEB-1A] TypeError: undefined", req.Title)
	}
	if req.GetTrackerId() != "WEB-1A" {
		t.Errorf("TrackerId = %q, want WEB-1A", req.GetTrackerId())
	}
	if req.GetTrackerUrl() != "https://acme.sentry.io/issues/123/" {
		t.Errorf("TrackerUrl = %q", req.GetTrackerUrl())
	}
	if req.GetBranchName() != "fix/web-1a" {
		t.Errorf("BranchName = %q, want fix/web-1a", req.GetBranchName())
	}
	// The plan is the self-formatted Sentry markdown.
	want := "Sentry issue:\n\n[WEB-1A] TypeError: undefined\n\n**Culprit:** app/main.js   **Level:** error\n\nhttps://acme.sentry.io/issues/123/\n"
	if req.Plan != want {
		t.Errorf("Plan mismatch:\n--- got ---\n%s\n--- want ---\n%s", req.Plan, want)
	}
}

func TestFormatSentryPrompt_Golden(t *testing.T) {
	issue := &pb.TrackerIssue{
		ExternalId:  "WEB-1A",
		Title:       "TypeError: undefined",
		Description: "**Culprit:** app/main.js\n\n### Latest stacktrace\n```\napp/a.js:fn:10\n```",
		Url:         "https://acme.sentry.io/issues/123/",
	}
	want := "Sentry issue:\n\n" +
		"[WEB-1A] TypeError: undefined\n\n" +
		"**Culprit:** app/main.js\n\n### Latest stacktrace\n```\napp/a.js:fn:10\n```\n\n" +
		"https://acme.sentry.io/issues/123/\n"
	if got := formatSentryPrompt(issue); got != want {
		t.Errorf("formatSentryPrompt mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestFormatSentryPrompt_NilSafe(t *testing.T) {
	if got := formatSentryPrompt(nil); got != "" {
		t.Errorf("formatSentryPrompt(nil) = %q, want empty", got)
	}
}

// When the displayed issues were already fetched with the current query, the
// tracker applied it server-side. Structured Sentry queries (e.g.
// "environment:production") match server-side but don't appear in the rendered
// columns, so the local filter must not re-hide them.
func TestApplyIssueFilter_TrustsServerFilteredResults(t *testing.T) {
	m := &NewSessionModel{
		issueFilter: newListFilter(),
		trackerIssues: []*pb.TrackerIssue{
			{ExternalId: "WEB-1A", Title: "boom", State: "unresolved", Description: "### Tags\n- environment: production"},
			{ExternalId: "WEB-2B", Title: "crash", State: "unresolved", Description: "no tags rendered"},
		},
	}
	m.issueFilter.input.SetValue("environment:production")
	m.issueSearchQuery = "environment:production" // same query the results were fetched with

	m.applyIssueFilter()

	if len(m.issuesFiltered) != 2 {
		t.Fatalf("server-filtered results should all be shown, got %d of 2", len(m.issuesFiltered))
	}
}

// Before the debounced server fetch returns, the local filter narrows the
// previously-fetched set. It should search the rendered description (tags,
// metadata), not just the ID/title/state columns.
func TestApplyIssueFilter_MatchesDescriptionContent(t *testing.T) {
	m := &NewSessionModel{
		issueFilter: newListFilter(),
		trackerIssues: []*pb.TrackerIssue{
			{ExternalId: "WEB-1A", Title: "boom", State: "unresolved", Description: "### Tags\n- release: 1.2.3"},
			{ExternalId: "WEB-2B", Title: "crash", State: "unresolved", Description: "nothing relevant"},
		},
	}
	// User typed ahead of the last fetch (issueSearchQuery differs), so the
	// local substring filter applies.
	m.issueFilter.input.SetValue("1.2.3")
	m.issueSearchQuery = ""

	m.applyIssueFilter()

	if len(m.issuesFiltered) != 1 || m.trackerIssues[m.issuesFiltered[0]].ExternalId != "WEB-1A" {
		t.Fatalf("description content should be searchable, got indices %v", m.issuesFiltered)
	}
}
