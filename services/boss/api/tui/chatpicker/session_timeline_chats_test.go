package chatpicker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"

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

// labelTimelineSteps is a dated two-step history whose first step is the
// build chat's label.
func labelTimelineSteps(label string) []*pb.TimelineStep {
	at := time.Now().Add(-time.Hour)
	return []*pb.TimelineStep{
		{At: timestamppb.New(at), Title: label, State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: timestamppb.New(at.Add(20 * time.Minute)), Title: "verify", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}
}

// labelTimelineChats is one listing of the build chat titled title, built
// fresh each call as a poll would deliver it.
func labelTimelineChats(title string) ChatsListedMsg {
	return ChatsListedMsg{
		Chats:          []*pb.AgentChat{{SessionId: "s", AgentSessionId: "build", Title: title, CreatedAt: timestamppb.New(time.Unix(100, 0))}},
		DaemonStatuses: map[string]string{"build": ui.StatusWorking},
	}
}

// labelTimelinePicker opens a sized picker on session s with the build chat
// listed and its timeline fetched.
func labelTimelinePicker(t *testing.T, c *timelineClient) Model {
	t.Helper()
	c.steps = labelTimelineSteps("build: old")
	m := newTestModel(c, context.Background(), "s", "")
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model, cmd := model.(Model).Update(SessionMsg{Session: &pb.Session{Id: "s", Title: "Fix login", PrDisplayHeadSha: "abc"}})
	m = applyTimelineCmd(model.(Model), cmd)
	model, cmd = m.Update(labelTimelineChats("build: old"))
	m = applyTimelineCmd(model.(Model), cmd)
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: old") {
		t.Fatalf("fixture timeline missing the build label:\n%s", block)
	}
	return m
}

// renameThroughKeys renames the selected chat to title the way an operator
// does: r, clear the prefilled title, type, enter. It returns the model and the
// rename command enter dispatched, not yet run.
func renameThroughKeys(t *testing.T, m Model, title string) (Model, tea.Cmd) {
	t.Helper()
	model, _ := m.Update(keyPress('r'))
	m = model.(Model)
	if !m.renaming {
		t.Fatal("r did not open the rename prompt")
	}
	for range m.renameInput.Value() {
		model, _ = m.Update(specialKeyPress(tea.KeyBackspace))
		m = model.(Model)
	}
	m = typeIntoPicker(m, title)
	model, cmd := m.Update(specialKeyPress(tea.KeyEnter))
	return model.(Model), cmd
}

func TestChatPickerTimelineInlineRenameRelabelsWithoutCollapsing_ChatPicker_AC214701(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	before := c.calls
	m, rename := renameThroughKeys(t, m, "build: renamed")
	if calls := c.titleUpdateCalls(); len(calls) != 0 || c.calls != before {
		t.Fatalf("enter ran RPCs before the rename command: renames=%d fetches=%d", len(calls), c.calls-before)
	}
	renamed := rename()
	if calls := c.titleUpdateCalls(); len(calls) != 1 || calls[0].title != "build: renamed" {
		t.Fatalf("rename RPC = %v", calls)
	}
	c.steps = labelTimelineSteps("build: renamed")
	model, fetch := m.Update(renamed)
	m = model.(Model)
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: old") || !strings.Contains(block, "verify") {
		t.Fatalf("timeline collapsed while the relabelled timeline was in flight:\n%s", block)
	}
	m = applyTimelineCmd(m, fetch)
	if c.calls != before+1 {
		t.Fatalf("rename fetched the timeline %d times, want 1", c.calls-before)
	}
	block := stripANSI(m.timelineBlock())
	if !strings.Contains(block, "build: renamed") || strings.Contains(block, "build: old") {
		t.Fatalf("timeline kept the old label after the rename:\n%s", block)
	}
	if view := stripANSI(m.View().Content); !strings.Contains(view, "build: renamed") || strings.Contains(view, "build: old") {
		t.Fatalf("screen kept the old label after the rename:\n%s", view)
	}
	if cmd := m.syncTimeline(); cmd != nil {
		t.Fatal("unchanged labels refetched the timeline")
	}
}

func TestChatPickerTimelineFailedRenameKeepsLabel_ChatPicker_AC214701(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	c.titleErr = errors.New("daemon refused")
	before := c.calls
	m, rename := renameThroughKeys(t, m, "build: renamed")
	c.steps = labelTimelineSteps("build: renamed")
	model, fetch := m.Update(rename())
	m = applyTimelineCmd(model.(Model), fetch)
	if c.calls != before {
		t.Fatalf("failed rename fetched the timeline %d times", c.calls-before)
	}
	if got := m.chats[0].GetTitle(); got != "build: old" {
		t.Fatalf("row title = %q after a failed rename, want it restored", got)
	}
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: old") || strings.Contains(block, "build: renamed") {
		t.Fatalf("failed rename changed the timeline label:\n%s", block)
	}
	if cmd := m.syncTimeline(); cmd != nil {
		t.Fatal("restored title refetched the timeline")
	}
}

func TestChatPickerTimelineLabelOnlyRefetchKeepsSteps_ChatPicker_AC214701(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	stale := sessionTimelineMsg{key: m.timelineKey, steps: labelTimelineSteps("build: stale")}
	c.steps = labelTimelineSteps("build: new")
	model, fetch := m.Update(labelTimelineChats("build: new"))
	m = model.(Model)
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: old") || !strings.Contains(block, "verify") {
		t.Fatalf("label-only refetch cleared the shown steps:\n%s", block)
	}
	model, _ = m.Update(stale)
	m = model.(Model)
	if block := stripANSI(m.timelineBlock()); strings.Contains(block, "build: stale") {
		t.Fatalf("in-flight timeline accepted the previous key's response:\n%s", block)
	}
	m = applyTimelineCmd(m, fetch)
	model, _ = m.Update(stale)
	m = model.(Model)
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: new") || strings.Contains(block, "build: stale") {
		t.Fatalf("late response for the previous key replaced the relabelled timeline:\n%s", block)
	}
	model, _ = m.Update(RefreshMsg{Session: &pb.Session{Id: "s", Title: "Fix login", PrDisplayHeadSha: "def"}})
	if steps := model.(Model).timeline; steps != nil {
		t.Fatalf("head change kept the previous head's steps: %v", steps)
	}
}

// pickerPollCmd drives the actual tick handler, leaving the scheduled next tick
// unexecuted. The caller may delay the returned RPC command to exercise races.
func pickerPollCmd(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	model, cmd := m.Update(tickMsg{})
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatal("tick did not dispatch refresh and next tick")
	}
	return model.(Model), batch[0]
}

func TestChatPickerTimelinePollPicksUpExternalRename_ChatPicker_AC214702_AC229301(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	c.session = m.session
	before := c.calls
	c.steps = labelTimelineSteps("build: from cli")
	for range 2 {
		c.chats = labelTimelineChats("build: from cli").Chats
		var poll tea.Cmd
		m, poll = pickerPollCmd(t, m)
		model, fetch := m.Update(poll())
		m = model.(Model)
		if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "verify") {
			t.Fatalf("poll collapsed the shown timeline: %s", block)
		}
		m = applyTimelineCmd(m, fetch)
		if m.selectedChat().GetAgentSessionId() != "build" || !strings.Contains(stripANSI(m.table.View()), "build: from cli") {
			t.Fatal("poll lost the selection or kept the old row title")
		}
	}
	if c.calls != before+1 {
		t.Fatalf("external rename fetched %d times over two polls, want 1", c.calls-before)
	}
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: from cli") || strings.Contains(block, "build: old") {
		t.Fatalf("timeline kept the old label after the poll:\n%s", block)
	}
}

func TestChatPickerPollMembershipKeepsSelection_ChatPicker_AC212801_AC229302(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	c.session = m.session
	newer := &pb.AgentChat{AgentSessionId: "new", Title: "automation", CreatedAt: timestamppb.New(time.Unix(200, 0))}
	older := &pb.AgentChat{AgentSessionId: "older", Title: "earlier", CreatedAt: timestamppb.New(time.Unix(50, 0))}
	poll := func(chats []*pb.AgentChat) {
		c.chats = chats
		var cmd tea.Cmd
		m, cmd = pickerPollCmd(t, m)
		model, fetch := m.Update(cmd())
		m = applyTimelineCmd(model.(Model), fetch)
	}
	m.highlightID = "new" // A poll must not retarget to the caller's highlight.
	c.steps = append(labelTimelineSteps("build: old"), &pb.TimelineStep{Title: "automation"})
	poll(append(labelTimelineChats("build: old").Chats, newer, older))
	if len(m.chats) != 3 || m.chats[0].GetAgentSessionId() != "new" || m.selectedChat().GetAgentSessionId() != "build" {
		t.Fatalf("new chat moved the cursor or was not sorted: selected=%s chats=%v", m.selectedChat().GetAgentSessionId(), m.chats)
	}
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "automation") {
		t.Fatalf("new chat missing from timeline: %s", block)
	}
	poll([]*pb.AgentChat{older, newer})
	if len(m.chats) != 2 || m.selectedChat() == nil || m.table.Cursor() >= len(m.chatRows) {
		t.Fatal("selected external deletion left the cursor outside the list")
	}
	poll([]*pb.AgentChat{newer})
	poll([]*pb.AgentChat{})
	if len(m.chats) != 0 || len(m.table.Rows()) != 0 {
		t.Fatal("last external deletion left a row on screen")
	}
}

func TestChatPickerPollKeepsSelectedBuildVisible_ChatPicker_AC229302(t *testing.T) {
	for _, highlight := range []string{"", "new"} {
		t.Run("highlight="+highlight, func(t *testing.T) {
			c := &timelineClient{}
			m := labelTimelinePicker(t, c)
			c.session = m.session
			selected := m.chats[0]
			selected.Role = pb.ChatRole_CHAT_ROLE_BUILD
			older := &pb.AgentChat{AgentSessionId: "older", Title: "earlier build", Role: pb.ChatRole_CHAT_ROLE_BUILD, CreatedAt: timestamppb.New(time.Unix(50, 0))}
			newer := &pb.AgentChat{AgentSessionId: "new", Title: "new build", Role: pb.ChatRole_CHAT_ROLE_BUILD, CreatedAt: timestamppb.New(time.Unix(200, 0))}
			m.chats = append(m.chats, older)
			m.daemonStatuses = map[string]string{}
			m.highlightID = highlight
			m.buildTableRows()
			if m.SelectedChatID() != selected.GetAgentSessionId() || len(m.table.Rows()) != 2 {
				t.Fatal("fixture did not select the newer of two idle builds")
			}
			c.chats = []*pb.AgentChat{newer, selected, older}
			for range 2 {
				var poll tea.Cmd
				m, poll = pickerPollCmd(t, m)
				model, fetch := m.Update(poll())
				m = applyTimelineCmd(model.(Model), fetch)
				m.buildTableRows()
				if m.SelectedChatID() != selected.GetAgentSessionId() {
					t.Fatalf("new build hid the selected build: selected=%q", m.SelectedChatID())
				}
			}
		})
	}
}

func TestChatPickerPollCannotRevertInlineRename_ChatPicker_AC214701_AC229303(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	c.session = m.session
	c.chats = labelTimelineChats("build: old").Chats
	m, beforeRename := pickerPollCmd(t, m)
	m, rename := renameThroughKeys(t, m, "build: renamed")
	staleBeforeRename := beforeRename()
	m, duringRename := pickerPollCmd(t, m)
	staleDuringRename := duringRename()
	for _, stale := range []tea.Msg{staleBeforeRename, staleDuringRename} {
		model, fetch := m.Update(stale)
		m = applyTimelineCmd(model.(Model), fetch)
		if m.chats[0].GetTitle() != "build: renamed" {
			t.Fatal("in-flight poll reverted the optimistic title")
		}
	}
	c.steps = labelTimelineSteps("build: renamed")
	model, fetch := m.Update(rename())
	m = applyTimelineCmd(model.(Model), fetch)
	for _, stale := range []tea.Msg{staleBeforeRename, staleDuringRename} {
		model, fetch = m.Update(stale)
		m = applyTimelineCmd(model.(Model), fetch)
		if m.chats[0].GetTitle() != "build: renamed" || !strings.Contains(stripANSI(m.timelineBlock()), "build: renamed") {
			t.Fatal("late poll reverted the settled rename or timeline label")
		}
	}
	c.chats = labelTimelineChats("build: later cli").Chats
	c.steps = labelTimelineSteps("build: later cli")
	m, fresh := pickerPollCmd(t, m)
	model, fetch = m.Update(fresh())
	m = applyTimelineCmd(model.(Model), fetch)
	if m.chats[0].GetTitle() != "build: later cli" || !strings.Contains(stripANSI(m.timelineBlock()), "build: later cli") {
		t.Fatal("fresh external rename did not replace the settled inline title")
	}
}

func TestChatPickerPollFailedReadAndOpenLoad(t *testing.T) {
	for _, loading := range []bool{false, true} {
		t.Run(fmt.Sprint("loading=", loading), func(t *testing.T) {
			c := &timelineClient{}
			m := labelTimelinePicker(t, c)
			m.loading = loading
			if loading {
				c.chats = labelTimelineChats("should not land").Chats
			}
			c.session = &pb.Session{Id: "s", Title: "updated session", PrDisplayHeadSha: "abc"}
			c.statuses = []*pb.ChatStatusEntry{{AgentSessionId: "build", Status: pb.ChatStatus_CHAT_STATUS_IDLE}}
			cursor := m.table.Cursor()
			m, poll := pickerPollCmd(t, m)
			model, _ := m.Update(poll())
			m = model.(Model)
			if m.session.GetTitle() != "updated session" || m.daemonStatuses["build"] != ui.StatusIdle {
				t.Fatal("chat read suppressed session or status refresh")
			}
			if m.chats[0].GetTitle() != "build: old" || m.table.Cursor() != cursor || m.err != nil || m.loading != loading {
				t.Fatal("failed read or loading poll changed chat list, cursor, error or loading state")
			}
		})
	}
}

func TestChatPickerPollCannotResurrectDeletedChat(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	c.session = m.session
	c.chats = labelTimelineChats("build: old").Chats
	m.deletingAgentSessionID = "build"
	m, poll := pickerPollCmd(t, m)
	stale := poll()
	model, _ := m.Update(stale)
	m = model.(Model)
	if len(m.chats) != 1 || m.deletingAgentSessionID != "build" {
		t.Fatal("poll interfered with pending deletion")
	}
	model, _ = m.Update(chatDeletedMsg{agentSessionID: "build"})
	m = model.(Model)
	model, _ = m.Update(stale)
	m = model.(Model)
	if len(m.chats) != 0 || len(m.table.Rows()) != 0 {
		t.Fatal("late poll resurrected the deleted chat")
	}
}

func TestChatPickerTimelineSessionTitleChangeRefetchesOnce_ChatPicker_AC214702(t *testing.T) {
	c := &timelineClient{}
	m := labelTimelinePicker(t, c)
	before := c.calls
	refresh := func(title string) {
		model, cmd := m.Update(RefreshMsg{Session: &pb.Session{Id: "s", Title: title, PrDisplayHeadSha: "abc"}})
		m = applyTimelineCmd(model.(Model), cmd)
	}
	refresh("Fix login")
	if c.calls != before {
		t.Fatalf("unchanged session title fetched the timeline %d times", c.calls-before)
	}
	c.steps = labelTimelineSteps("build: relabelled")
	refresh("Fix login flow")
	refresh("Fix login flow")
	if c.calls != before+1 {
		t.Fatalf("session title change fetched %d times over two refreshes, want 1", c.calls-before)
	}
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "build: relabelled") {
		t.Fatalf("timeline kept the old label after the session title changed:\n%s", block)
	}
}
