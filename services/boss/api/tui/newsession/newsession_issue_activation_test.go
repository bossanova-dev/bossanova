package newsession

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func newPendingIssueActivationModel(t *testing.T, issues []*pb.TrackerIssue, query string) Model {
	t.Helper()
	sc := &stubClient{
		repos:         linearRepo(),
		trackerIssues: issues,
		created:       &pb.Session{Id: "session-1"},
	}
	m := NewModel(sc, context.Background())
	m = sendMsg(t, m, ReposMsg{Repos: sc.repos})
	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, IssuesMsg{Issues: issues})
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
			m = sendMsg(t, m, IssuesMsg{Issues: []*pb.TrackerIssue{fresh}, Seq: m.issueSearchSeq, Query: tt.query})
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
		msg  IssuesMsg
	}{
		{name: "empty result", msg: IssuesMsg{Query: "missing"}},
		{name: "error", msg: IssuesMsg{Query: "missing", Err: fmt.Errorf("search failed")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{{ExternalId: "OLD-1", Title: "old result"}}, "missing")
			tt.msg.Seq = m.issueSearchSeq
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
			if tt.msg.Err != nil && m.err == nil {
				t.Fatal("search error was not surfaced")
			}
		})
	}
}

func TestNewSession_PendingIssueActivationDisarmsOnIntentChange(t *testing.T) {
	fresh := IssuesMsg{Issues: []*pb.TrackerIssue{{ExternalId: "BOS-1094", Title: "fresh result"}}, Query: "missing"}
	tests := []struct {
		name   string
		change func(t *testing.T, m Model) Model
	}{
		{name: "escape filter", change: func(t *testing.T, m Model) Model {
			return sendSpecialKey(t, m, tea.KeyEscape)
		}},
		{name: "escape picker", change: func(t *testing.T, m Model) Model {
			m.issueFilter.Reset()
			return sendSpecialKey(t, m, tea.KeyEscape)
		}},
		{name: "query change", change: func(t *testing.T, m Model) Model {
			return sendKey(t, m, 'x')
		}},
		{name: "cursor move", change: func(t *testing.T, m Model) Model {
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
			fresh.Seq = m.issueSearchSeq
			m = sendMsg(t, m, fresh)
			if m.phase == newSessionPhaseCreating {
				t.Fatal("fresh result started a session after activation was disarmed")
			}
		})
	}
}

func TestNewSession_StaleIssuesMessagePreservesPendingActivation(t *testing.T) {
	m := newPendingIssueActivationModel(t, []*pb.TrackerIssue{{ExternalId: "OLD-1", Title: "old result"}}, "missing")
	m = sendMsg(t, m, IssuesMsg{
		Issues: []*pb.TrackerIssue{{ExternalId: "STALE-1", Title: "stale result"}},
		Seq:    m.issueSearchSeq - 1,
		Query:  "old query",
	})
	if !m.pendingIssueActivate {
		t.Fatal("stale IssuesMsg cleared pendingIssueActivate")
	}
	if m.phase != newSessionPhaseIssueSelect {
		t.Fatalf("phase = %d after stale IssuesMsg, want issue select", m.phase)
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
	m := NewModel(&stubClient{repos: linearRepo()}, context.Background())
	m.phase = newSessionPhaseCreating
	m.selectedIssue = &pb.TrackerIssue{ExternalId: "BOS-1094", Title: "BOS-1094 deferred activation result"}

	view := m.View().Content
	if !strings.Contains(view, m.selectedIssue.Title) {
		t.Fatalf("View() = %q, want selected issue title %q", view, m.selectedIssue.Title)
	}
}

func TestNewSession_PRFilterEnterSelectsHighlightedInOnePress(t *testing.T) {
	sc := &stubClient{repos: oneRepo(), created: &pb.Session{Id: "session-1"}}
	m := NewModel(sc, context.Background())
	m = sendMsg(t, m, ReposMsg{Repos: sc.repos})
	m.selectedType = sessionTypeExistingPR
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, PrsMsg{PRs: []*pb.PRSummary{
		{Number: 7, Title: "Fix login flow", HeadBranch: "fix-login"},
		{Number: 8, Title: "Add dark mode", HeadBranch: "dark-mode"},
	}})
	m = sendKey(t, m, '/')
	m = sendMsg(t, m, pasteText("dark mode"))
	m = sendSpecialKey(t, m, tea.KeyEnter)
	if m.phase != newSessionPhaseCreating {
		t.Fatalf("phase = %d after one Enter, want creating", m.phase)
	}
	if m.TextEntryActive() {
		t.Fatal("TextEntryActive() = true after PR activation moved to creating")
	}
}
