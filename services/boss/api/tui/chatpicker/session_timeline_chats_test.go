package chatpicker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPickerTimelineFetchWithoutAttention_ChatPicker_AC212801(t *testing.T) {
	c := &timelineClient{steps: []*pb.TimelineStep{{Title: "build"}}}
	m := newTestModel(c, context.Background(), "s", "")
	s := &pb.Session{Id: "s"}
	model, cmd := m.Update(SessionMsg{Session: s})
	m = applyTimelineCmd(model.(Model), cmd)
	if c.calls != 1 || len(m.timeline) != 1 {
		t.Fatalf("open without attention: calls=%d steps=%d", c.calls, len(m.timeline))
	}
	model, cmd = m.Update(RefreshMsg{Session: s})
	m = applyTimelineCmd(model.(Model), cmd)
	if c.calls != 1 {
		t.Fatalf("unchanged chat count fetched timeline: calls=%d", c.calls)
	}
	c.steps = append(c.steps, &pb.TimelineStep{Title: "verify"})
	model, cmd = m.Update(ChatsListedMsg{Chats: []*pb.AgentChat{{AgentSessionId: "verify", Title: "verify"}}})
	m = applyTimelineCmd(model.(Model), cmd)
	if c.calls != 2 || len(m.timeline) != 2 || m.timeline[1].GetTitle() != "verify" {
		t.Fatalf("new chat not fetched: calls=%d steps=%v", c.calls, m.timeline)
	}
}

func TestChatPickerTimelineRefreshesRenamedChat_ChatPicker_AC212801(t *testing.T) {
	for _, source := range []string{"inline", "poll"} {
		t.Run(source, func(t *testing.T) {
			c := &timelineClient{steps: []*pb.TimelineStep{{Title: "repair: old"}}}
			m := newTestModel(c, context.Background(), "s", "")
			m.session = &pb.Session{Id: "s"}
			m.chats = []*pb.AgentChat{{AgentSessionId: "repair", Title: "repair: old"}}
			cmd := m.syncTimeline()
			stale := cmd()
			initial, _ := m.Update(stale)
			m = initial.(Model)
			c.steps = []*pb.TimelineStep{{Title: "repair: new"}}
			if source == "inline" {
				m.chats[0].Title = "repair: new"
				updated, fetch := m.Update(chatRenamedMsg{agentSessionID: "repair"})
				m = applyTimelineCmd(updated.(Model), fetch)
			} else {
				updated, fetch := m.Update(ChatsListedMsg{Chats: []*pb.AgentChat{{AgentSessionId: "repair", Title: "repair: new"}}})
				m = applyTimelineCmd(updated.(Model), fetch)
			}
			updated, _ := m.Update(stale)
			m = updated.(Model)
			if c.calls != 2 || len(m.timeline) != 1 || m.timeline[0].GetTitle() != "repair: new" {
				t.Fatalf("rename did not replace cached timeline: calls=%d steps=%v", c.calls, m.timeline)
			}
			if cmd := m.syncTimeline(); cmd != nil {
				t.Fatal("unchanged title refetched timeline")
			}
		})
	}
}

// TestChatPickerTimelineShowsRotationsWithoutAttention: a session with nothing
// waiting merges daemon history and rotations, oldest first, with a failed
// switch marked failed.
func TestChatPickerTimelineShowsRotationsWithoutAttention_ChatPicker_AC212801(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, 40)
	m.timeline = nil
	m.session.RotationEvents = []*pb.RotationEvent{
		{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_FAILED, ToAccount: "b@x", CreatedAt: timestamppb.New(time.Now().Add(-time.Minute))},
		{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, FromAccount: "a@x", ToAccount: "c@x", CreatedAt: timestamppb.New(time.Now().Add(-time.Hour))},
	}
	rows := timelineRowsOf(m)
	if len(rows) != 2 || !strings.Contains(rows[0], "✓ Account rotation  a@x switched to c@x") || !strings.Contains(rows[1], "✗ Account rotation  switch to b@x failed") {
		t.Fatalf("rotation-only timeline:\n%s", strings.Join(rows, "\n"))
	}
	start := time.Now().Add(-2 * time.Hour)
	m.timeline = nil
	for i, title := range []string{"build", "verify", "repair: checks", "verify 2"} {
		m.timeline = append(m.timeline, &pb.TimelineStep{At: timestamppb.New(start.Add(time.Duration(i) * 20 * time.Minute)), Title: title, State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
	}
	m.session.RotationEvents = m.session.RotationEvents[1:]
	steps := m.timelineSteps()
	var titles []string
	for _, step := range steps {
		titles = append(titles, step.GetTitle())
	}
	if want := []string{"build", "verify", "repair: checks", rotationStepTitle, "verify 2"}; !slices.Equal(titles, want) {
		t.Fatalf("merged timeline titles = %v, want %v", titles, want)
	}
	block := stripANSI(m.timelineBlock())
	for _, title := range titles {
		if !strings.Contains(block, title) {
			t.Errorf("timeline omits %q:\n%s", title, block)
		}
	}
	if screen := stripANSI(m.View().Content); strings.Contains(screen, "Rotation history") {
		t.Fatalf("the separate rotation block still renders:\n%s", screen)
	}
}

func TestChatPicker_RefreshRefetchesWebLinkWhenPRNumberAppears(t *testing.T) {
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1"},
		repos: []*pb.Repo{
			{Id: "repo-1", OriginUrl: "git@github.com:owner/repo.git"},
		},
	}
	m := newTestModel(stub, context.Background(), "session-1", "")
	m.session = stub.session

	// Initial fetch happens before a PR exists: caches the plain repo URL.
	updated, _ := m.Update(m.fetchRepoWebLink()())
	m = updated.(Model)
	if m.repoWebLink.url != "https://github.com/owner/repo" {
		t.Fatalf("initial repoWebLink.url = %q, want plain repo URL", m.repoWebLink.url)
	}

	// Polling later discovers the PR number; the refresh must re-fetch the link.
	prNumber := int32(42)
	refreshed := &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber}
	updated, cmd := m.Update(RefreshMsg{Session: refreshed})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected refresh with a new PR number to re-fetch the repo web link")
	}
	// During the async refetch the stale link must be cleared so the [g]ithub
	// action hides rather than opening the old repo URL.
	if m.repoWebLink.url != "" {
		t.Fatalf("repoWebLink.url = %q, want cleared during async refetch", m.repoWebLink.url)
	}
	if m.canOpenGitHub() {
		t.Fatal("canOpenGitHub must be false while the PR web link is being refetched")
	}

	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(Model)
	}
	if m.repoWebLink.url != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("repoWebLink.url = %q, want PR URL after PR number appears", m.repoWebLink.url)
	}
	if !m.canOpenGitHub() {
		t.Fatal("canOpenGitHub must be true once the PR web link is installed")
	}
}

func TestChatPicker_RefreshDoesNotRefetchWebLinkWhenPRNumberUnchanged(t *testing.T) {
	prNumber := int32(42)
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber},
	}
	m := newTestModel(stub, context.Background(), "session-1", "")
	m.session = stub.session

	refreshed := &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber}
	_, cmd := m.Update(RefreshMsg{Session: refreshed})
	for _, msg := range drainCmd(cmd) {
		if _, ok := msg.(repoWebLinkMsg); ok {
			t.Fatal("expected no web-link re-fetch when the PR number is unchanged")
		}
	}
}
