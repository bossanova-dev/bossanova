package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeLister is a stub issueLister returning canned issues (or an error),
// substituted via server.newLister to avoid hitting the real Sentry API.
type fakeLister struct {
	issues       []*bossanovav1.TrackerIssue
	err          error
	gotQuery     string
	gotToken     string
	gotOrg       string
	listerCalled bool
}

func (f *fakeLister) ListIssues(_ context.Context, query string) ([]*bossanovav1.TrackerIssue, error) {
	f.listerCalled = true
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	return f.issues, nil
}

// newTestServer wires a server with the supplied fake lister, bypassing the
// real Sentry client factory.
func newTestServer(lister *fakeLister) *server {
	s := newServer(zerolog.Nop())
	s.newLister = func(token, org string) issueLister {
		lister.gotToken = token
		lister.gotOrg = org
		return lister
	}
	return s
}

func TestGetInfo_ReportsSentryTaskSourceContract(t *testing.T) {
	s := newServer(zerolog.Nop())

	resp, err := s.GetInfo(context.Background(), &bossanovav1.TaskSourceServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo returned error: %v", err)
	}

	info := resp.GetInfo()
	if info == nil {
		t.Fatal("GetInfo returned nil info")
	}
	// The daemon routes ListTrackerIssues to this plugin by matching this exact
	// name; pin it so a rename can't silently break Sentry routing.
	if got := info.GetName(); got != "sentry" {
		t.Errorf("plugin name = %q, want %q", got, "sentry")
	}
	caps := info.GetCapabilities()
	if len(caps) != 1 || caps[0] != "task_source" {
		t.Errorf("capabilities = %v, want [task_source]", caps)
	}
}

func TestPollTasks_ReturnsEmptyForUserInitiatedPlugin(t *testing.T) {
	s := newServer(zerolog.Nop())

	resp, err := s.PollTasks(context.Background(), &bossanovav1.PollTasksRequest{})
	if err != nil {
		t.Fatalf("PollTasks returned error: %v", err)
	}
	if got := len(resp.GetTasks()); got != 0 {
		t.Errorf("PollTasks returned %d tasks, want 0", got)
	}
}

func TestUpdateTaskStatus_IsNoOpSuccess(t *testing.T) {
	s := newServer(zerolog.Nop())

	resp, err := s.UpdateTaskStatus(context.Background(), &bossanovav1.UpdateTaskStatusRequest{
		ExternalId: "PROJ-1",
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

func TestListAvailableIssues_RejectsMissingConfig(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{"all missing", map[string]string{}, "sentry_api_key"},
		{"missing org", map[string]string{"sentry_api_key": "t"}, "sentry_org"},
		{"missing key", map[string]string{"sentry_org": "o"}, "sentry_api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeLister{}
			s := newTestServer(lister)
			_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
				Config: tc.config,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error naming %q, got %v", tc.want, err)
			}
			if lister.listerCalled {
				t.Error("lister should not be invoked when config is incomplete")
			}
		})
	}
}

func TestListAvailableIssues_WrapsListError(t *testing.T) {
	lister := &fakeLister{err: errors.New("boom")}
	s := newTestServer(lister)

	_, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		Config: map[string]string{"sentry_api_key": "t", "sentry_org": "o"},
	})
	if err == nil || !strings.Contains(err.Error(), "fetch Sentry issues") {
		t.Fatalf("expected wrapped fetch error, got %v", err)
	}
}

func TestListAvailableIssues_ThreadsCredentialsAndQuery(t *testing.T) {
	lister := &fakeLister{issues: []*bossanovav1.TrackerIssue{
		{ExternalId: "PROJ-1", Title: "boom"},
	}}
	s := newTestServer(lister)

	resp, err := s.ListAvailableIssues(context.Background(), &bossanovav1.ListAvailableIssuesRequest{
		Config: map[string]string{
			"sentry_api_key": "secret",
			"sentry_org":     "acme",
		},
		Query: "boom",
	})
	if err != nil {
		t.Fatalf("ListAvailableIssues returned error: %v", err)
	}
	if lister.gotToken != "secret" || lister.gotOrg != "acme" {
		t.Errorf("credentials = (%q,%q), want (secret,acme)", lister.gotToken, lister.gotOrg)
	}
	if lister.gotQuery != "boom" {
		t.Errorf("query = %q, want boom", lister.gotQuery)
	}
	if got := len(resp.GetIssues()); got != 1 {
		t.Fatalf("got %d issues, want 1", got)
	}
	if resp.GetIssues()[0].GetExternalId() != "PROJ-1" {
		t.Errorf("ExternalId = %q, want PROJ-1", resp.GetIssues()[0].GetExternalId())
	}
}
