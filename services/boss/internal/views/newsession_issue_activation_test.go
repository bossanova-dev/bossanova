package views

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func newPendingIssueActivationModel(t *testing.T, issues []*pb.TrackerIssue, query string) NewSessionModel {
	t.Helper()
	sc := &stubClient{
		repos:         linearRepo(),
		trackerIssues: issues,
		created:       &pb.Session{Id: "session-1"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, issuesMsg{issues: issues})
	m = sendKey(t, m, '/')
	m = sendMsg(t, m, pasteText(query))
	m = sendSpecialKey(t, m, tea.KeyEnter)
	return m
}

func TestNewSession_IssueFilterEnterDefersUntilFreshResults(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "empty stale rows", query: "BOS-1094"},
		{name: "non-empty stale rows", query: "old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{
				{ExternalId: "OLD-1", Title: "old result"},
			}, tt.query)
			if m.phase != newSessionPhaseIssueSelect {
				t.Fatalf("phase = %d after Enter, want issue select while search is pending", m.phase)
			}
			if !m.pendingIssueActivate {
				t.Fatal("pendingIssueActivate = false after Enter during search")
			}
			if m.selectedIssue != nil {
				t.Fatalf("selectedIssue = %v before fresh results land, want nil", m.selectedIssue)
			}

			fresh := &pb.TrackerIssue{ExternalId: "BOS-1094", Title: tt.query + " fresh result"}
			m = sendMsg(t, m, issuesMsg{issues: []*pb.TrackerIssue{fresh}, seq: m.issueSearchSeq, query: tt.query})
			if m.phase != newSessionPhaseCreating {
				t.Fatalf("phase = %d after fresh result, want creating", m.phase)
			}
			if m.selectedIssue != fresh {
				t.Fatalf("selectedIssue = %v, want fresh result %v", m.selectedIssue, fresh)
			}
			if m.pendingIssueActivate {
				t.Fatal("pendingIssueActivate remains set after activation")
			}
		})
	}
}

func TestNewSession_PendingIssueActivationSettledWithoutSelection(t *testing.T) {
	tests := []struct {
		name string
		msg  issuesMsg
	}{
		{name: "empty result", msg: issuesMsg{query: "missing"}},
		{name: "error", msg: issuesMsg{query: "missing", err: fmt.Errorf("search failed")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{{ExternalId: "OLD-1", Title: "old result"}}, "missing")
			tt.msg.seq = m.issueSearchSeq
			m = sendMsg(t, m, tt.msg)
			if m.pendingIssueActivate {
				t.Fatal("pendingIssueActivate remains set after settled result")
			}
			if m.phase != newSessionPhaseIssueSelect {
				t.Fatalf("phase = %d, want issue select", m.phase)
			}
			if m.selectedIssue != nil {
				t.Fatalf("selectedIssue = %v, want nil", m.selectedIssue)
			}
			if tt.msg.err != nil && m.err == nil {
				t.Fatal("search error was not surfaced")
			}
		})
	}
}

func TestNewSession_PendingIssueActivationDisarmsOnIntentChange(t *testing.T) {
	fresh := issuesMsg{issues: []*pb.TrackerIssue{{ExternalId: "BOS-1094", Title: "fresh result"}}, query: "missing"}
	tests := []struct {
		name   string
		change func(t *testing.T, m NewSessionModel) NewSessionModel
	}{
		{name: "escape filter", change: func(t *testing.T, m NewSessionModel) NewSessionModel {
			return sendSpecialKey(t, m, tea.KeyEscape)
		}},
		{name: "escape picker", change: func(t *testing.T, m NewSessionModel) NewSessionModel {
			m.issueFilter.Reset()
			return sendSpecialKey(t, m, tea.KeyEscape)
		}},
		{name: "query change", change: func(t *testing.T, m NewSessionModel) NewSessionModel {
			return sendKey(t, m, 'x')
		}},
		{name: "cursor move", change: func(t *testing.T, m NewSessionModel) NewSessionModel {
			return sendSpecialKey(t, m, tea.KeyDown)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{{ExternalId: "OLD-1", Title: "old result"}}, "missing")
			m = tt.change(t, m)
			if m.pendingIssueActivate {
				t.Fatal("pendingIssueActivate remains set after intent change")
			}
			fresh.seq = m.issueSearchSeq
			m = sendMsg(t, m, fresh)
			if m.phase == newSessionPhaseCreating {
				t.Fatal("fresh result started a session after activation was disarmed")
			}
		})
	}
}

func TestNewSession_StaleIssuesMessagePreservesPendingActivation(t *testing.T) {
	m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{{ExternalId: "OLD-1", Title: "old result"}}, "missing")
	m = sendMsg(t, m, issuesMsg{
		issues: []*pb.TrackerIssue{{ExternalId: "STALE-1", Title: "stale result"}},
		seq:    m.issueSearchSeq - 1,
		query:  "old query",
	})
	if !m.pendingIssueActivate {
		t.Fatal("stale issuesMsg cleared pendingIssueActivate")
	}
	if m.phase != newSessionPhaseIssueSelect {
		t.Fatalf("phase = %d after stale issuesMsg, want issue select", m.phase)
	}
}

func TestNewSession_PendingIssueActivationRendersStatus(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "empty placeholder", query: "missing"},
		{name: "non-empty suffix", query: "old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{{ExternalId: "OLD-1", Title: "old result"}}, tt.query)
			view := m.View().Content
			if !strings.Contains(view, "starting when results land…") {
				t.Fatalf("View() = %q, want pending activation status", view)
			}
			if strings.Contains(view, "searching…") {
				t.Fatalf("View() = %q, want pending status to replace searching status", view)
			}
			if tt.query == "missing" && !strings.Contains(view, "no matches") {
				t.Fatalf("View() = %q, want empty-result context alongside pending status", view)
			}
		})
	}
}

func TestNewSession_CreatingIssueRendersSelectedTitle(t *testing.T) {
	m := NewNewSessionModel(&stubClient{repos: linearRepo()}, context.Background())
	m.phase = newSessionPhaseCreating
	m.selectedIssue = &pb.TrackerIssue{ExternalId: "BOS-1094", Title: "BOS-1094 deferred activation result"}

	view := m.View().Content
	if !strings.Contains(view, m.selectedIssue.Title) {
		t.Fatalf("View() = %q, want selected issue title %q", view, m.selectedIssue.Title)
	}
}

func TestNewSession_PRFilterEnterSelectsHighlightedInOnePress(t *testing.T) {
	sc := &stubClient{repos: oneRepo(), created: &pb.Session{Id: "session-1"}}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m.selectedType = sessionTypeExistingPR
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, prsMsg{prs: []*pb.PRSummary{
		{Number: 7, Title: "Fix login flow", HeadBranch: "fix-login"},
		{Number: 8, Title: "Add dark mode", HeadBranch: "dark-mode"},
	}})
	m = sendKey(t, m, '/')
	m = sendMsg(t, m, pasteText("dark mode"))
	m = sendSpecialKey(t, m, tea.KeyEnter)
	if m.phase != newSessionPhaseCreating {
		t.Fatalf("phase = %d after one Enter, want creating", m.phase)
	}
	if m.textEntryActive() {
		t.Fatal("textEntryActive() = true after PR activation moved to creating")
	}
}
