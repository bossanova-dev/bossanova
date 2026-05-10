package views

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// chatPickerStub is a BossClient that records WakeChat calls so the chat
// picker tests can assert on what the TUI dispatched. Other methods panic;
// the chat picker tests only drive the wake path directly via Update,
// they don't go through the lifecycle that needs ListChats / GetSession.
type chatPickerStub struct {
	mu            sync.Mutex
	wakeChatCalls []wakeChatCall
	wakeResp      *pb.WakeChatResponse
	wakeErr       error
}

type wakeChatCall struct {
	sessionID      string
	agentSessionID string
	forceFresh     bool
}

func (s *chatPickerStub) WakeChat(_ context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wakeChatCalls = append(s.wakeChatCalls, wakeChatCall{
		sessionID:      sessionID,
		agentSessionID: agentSessionID,
		forceFresh:     forceFresh,
	})
	if s.wakeErr != nil {
		return nil, s.wakeErr
	}
	if s.wakeResp != nil {
		return s.wakeResp, nil
	}
	return &pb.WakeChatResponse{Outcome: pb.WakeChatResponse_OUTCOME_RESUMED}, nil
}

func (s *chatPickerStub) wakeCalls() []wakeChatCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wakeChatCall, len(s.wakeChatCalls))
	copy(out, s.wakeChatCalls)
	return out
}

// GetChatStatuses must not panic — the model's refreshStatuses tick may
// call it; we just return nothing so the picker keeps the explicitly
// seeded statuses.
func (s *chatPickerStub) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return nil, nil
}

// GetSession must not panic — refreshStatuses calls it on every tick.
func (s *chatPickerStub) GetSession(context.Context, string) (*pb.Session, error) {
	return &pb.Session{Id: "session-1"}, nil
}

// Unused interface methods — panic if called unexpectedly.
func (s *chatPickerStub) Ping(context.Context) error { panic("unused") }
func (s *chatPickerStub) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) ValidateRepoPath(context.Context, string) (*pb.ValidateRepoPathResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) RegisterRepo(context.Context, *pb.RegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *chatPickerStub) CloneAndRegisterRepo(context.Context, *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *chatPickerStub) ListRepos(context.Context) ([]*pb.Repo, error) { panic("unused") }
func (s *chatPickerStub) RemoveRepo(context.Context, string) error      { panic("unused") }
func (s *chatPickerStub) UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *chatPickerStub) ListSessions(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
	panic("unused")
}
func (s *chatPickerStub) AttachSession(context.Context, string) (client.AttachStream, error) {
	panic("unused")
}
func (s *chatPickerStub) CreateSession(context.Context, *pb.CreateSessionRequest) (client.CreateSessionStream, error) {
	panic("unused")
}
func (s *chatPickerStub) StopSession(context.Context, string) (*pb.Session, error)   { panic("unused") }
func (s *chatPickerStub) PauseSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *chatPickerStub) ResumeSession(context.Context, string) (*pb.Session, error) { panic("unused") }
func (s *chatPickerStub) RetrySession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *chatPickerStub) CloseSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *chatPickerStub) MergeSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *chatPickerStub) RemoveSession(context.Context, string) error                { panic("unused") }
func (s *chatPickerStub) UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error) {
	panic("unused")
}
func (s *chatPickerStub) ArchiveSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *chatPickerStub) ResurrectSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *chatPickerStub) EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error) {
	panic("unused")
}
func (s *chatPickerStub) RecordChat(context.Context, string, string, string, string, bool) (*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *chatPickerStub) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *chatPickerStub) UpdateChatTitle(context.Context, string, string) error { panic("unused") }
func (s *chatPickerStub) DeleteChat(context.Context, string) error              { panic("unused") }
func (s *chatPickerStub) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	panic("unused")
}
func (s *chatPickerStub) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	panic("unused")
}
func (s *chatPickerStub) NotifyAuthChange(context.Context, string) error { return nil }
func (s *chatPickerStub) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	panic("unused")
}
func (s *chatPickerStub) ListTrackerIssues(context.Context, string, string) ([]*pb.TrackerIssue, error) {
	panic("unused")
}
func (s *chatPickerStub) CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *chatPickerStub) ListCronJobs(context.Context) ([]*pb.CronJob, error) { panic("unused") }
func (s *chatPickerStub) UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *chatPickerStub) DeleteCronJob(context.Context, string) error { panic("unused") }
func (s *chatPickerStub) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) RepairDoctor(context.Context) (*pb.RepairDoctorResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) ListCheckSnapshots(context.Context, string, int32) (*pb.ListCheckSnapshotsResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) ListAgents(context.Context) ([]client.AgentInfo, error) { return nil, nil }
func (s *chatPickerStub) ListPlugins(context.Context) ([]*pb.InstalledPlugin, error) {
	return nil, nil
}

// seedChatPicker returns a ChatPickerModel populated with a single chat at the
// given daemon status. Tests can press 'w' against the resulting model.
func seedChatPicker(c client.BossClient, status string) ChatPickerModel {
	m := NewChatPickerModel(c, context.Background(), "session-1", "")
	chat := &pb.ClaudeChat{
		SessionId:      "session-1",
		AgentSessionId: "agent-1",
		Title:          "Test chat",
		CreatedAt:      timestamppb.Now(),
	}
	statuses := map[string]string{}
	if status != "" {
		statuses["agent-1"] = status
	}
	updated, _ := m.Update(chatsListedMsg{
		chats:          []*pb.ClaudeChat{chat},
		daemonStatuses: statuses,
	})
	return updated.(ChatPickerModel)
}

func TestChatPicker_W_OnStoppedChat_FiresWake(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusStopped)

	updated, cmd := m.Update(keyPress('w'))
	m = updated.(ChatPickerModel)

	if cmd == nil {
		t.Fatal("expected a cmd from 'w' on stopped chat, got nil")
	}
	if m.statusMsg != "Waking..." {
		t.Errorf("statusMsg before resolve = %q, want %q", m.statusMsg, "Waking...")
	}

	// Execute the cmd; it should call WakeChat exactly once.
	_ = cmd()
	calls := stub.wakeCalls()
	if len(calls) != 1 {
		t.Fatalf("WakeChat called %d times, want 1", len(calls))
	}
	want := wakeChatCall{sessionID: "session-1", agentSessionID: "agent-1", forceFresh: false}
	if calls[0] != want {
		t.Errorf("WakeChat call = %+v, want %+v", calls[0], want)
	}
}

func TestChatPicker_W_OnLiveChat_NoOp(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)

	_, cmd := m.Update(keyPress('w'))

	if cmd != nil {
		// The cmd is a no-op view-state command at most. To prove the wake
		// didn't fire, just count calls.
		_ = cmd()
	}
	calls := stub.wakeCalls()
	if len(calls) != 0 {
		t.Fatalf("WakeChat called %d times for a working chat, want 0", len(calls))
	}
}

func TestChatPicker_WakeResultMsg_RendersOutcome(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusStopped)

	cases := []struct {
		name    string
		outcome pb.WakeChatResponse_Outcome
		want    string
	}{
		{"resumed", pb.WakeChatResponse_OUTCOME_RESUMED, "Resumed"},
		{"already-live", pb.WakeChatResponse_OUTCOME_ALREADY_LIVE, "Already live"},
		{"fresh-fallback", pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK, "Started fresh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, _ := m.Update(wakeResultMsg{
				agentSessionID: "agent-1",
				resp:           &pb.WakeChatResponse{Outcome: tc.outcome},
			})
			got := updated.(ChatPickerModel).statusMsg
			if got != tc.want {
				t.Errorf("statusMsg = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestChatPicker_RendersRepairChatTitle is the TUI smoke test for Task 6
// of the repair-chat-visibility spec. The daemon-side regression test
// (services/bossd/internal/plugin/repair_chat_visibility_test.go) pins
// that StartChatRun inserts a row titled "Repair: <session>" into
// agent_chats — the chat picker is what surfaces that row to the
// operator. This test guards against future regressions where
// repair-specific rendering accidentally diverges (eg. a code path that
// special-cases titles starting with "Repair:" and panics, or a column
// width calculation that mishandles the colon). One assertion: the
// rendered View() output contains the title, and View() doesn't panic.
//
// We deliberately don't assert on layout/spacing here — the chat
// picker's View() is exercised at the integration layer; this is a
// targeted "the title round-trips through render" guard.
func TestChatPicker_RendersRepairChatTitle(t *testing.T) {
	stub := &chatPickerStub{}
	const repairTitle = "Repair: broken session"
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	chat := &pb.ClaudeChat{
		SessionId:       "session-1",
		AgentSessionId:  "agent-repair-1",
		Title:           repairTitle,
		TmuxSessionName: "boss-repair-tmux-1",
		CreatedAt:       timestamppb.Now(),
	}
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{chat},
		daemonStatuses: map[string]string{
			"agent-repair-1": statusWorking,
		},
	})
	m = updated.(ChatPickerModel)

	// Set a viewport size so the table actually renders rows. Without
	// this, the model is in "loading"/zero-size mode and View output
	// degenerates to a placeholder.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	// View() must not panic on a Repair-prefixed title. If it ever does
	// (rune slicing, lipgloss styling, table column-width math), this
	// assertion fires. tea.View carries the rendered content as Content.
	rendered := m.View().Content
	if !strings.Contains(rendered, "Repair:") {
		t.Errorf("rendered chat picker missing %q in:\n%s", "Repair:", rendered)
	}
}

// TestChatPicker_NewChatShowsAgentPickerWithMultipleAgents verifies that
// pressing "n" with 2+ agents loaded enters the agent-select sub-phase
// instead of immediately switching to ViewAttach.
func TestChatPicker_NewChatShowsAgentPickerWithMultipleAgents(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(agentsMsg{agents: []client.AgentInfo{
		{Name: "claude"},
		{Name: "codex"},
	}})
	m = updated.(ChatPickerModel)

	updated, cmd := m.Update(keyPress('n'))
	got := updated.(ChatPickerModel)
	if !got.pickingAgent {
		t.Errorf("expected pickingAgent=true after pressing 'n' with 2 agents loaded")
	}
	if cmd != nil {
		t.Errorf("expected no cmd while entering picker, got %T", cmd)
	}
	if len(got.agentTable.Rows()) != 2 {
		t.Errorf("agentTable rows = %d, want 2", len(got.agentTable.Rows()))
	}
}

// TestChatPicker_NewChatSkipsAgentPickerWithSingleAgent verifies that
// the agent picker is skipped when only one agent runner is loaded —
// pressing "n" goes straight to ViewAttach with no agent override.
func TestChatPicker_NewChatSkipsAgentPickerWithSingleAgent(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(agentsMsg{agents: []client.AgentInfo{
		{Name: "claude"},
	}})
	m = updated.(ChatPickerModel)

	updated, cmd := m.Update(keyPress('n'))
	got := updated.(ChatPickerModel)
	if got.pickingAgent {
		t.Errorf("expected pickingAgent=false with a single agent loaded")
	}
	if cmd == nil {
		t.Fatal("expected a switchViewMsg cmd to be returned")
	}
	out := cmd()
	sw, ok := out.(switchViewMsg)
	if !ok {
		t.Fatalf("expected switchViewMsg, got %T", out)
	}
	if sw.view != ViewAttach {
		t.Errorf("switchViewMsg.view = %v, want ViewAttach", sw.view)
	}
	if sw.agentName != "" {
		t.Errorf("switchViewMsg.agentName = %q, want empty (single-agent skips override)", sw.agentName)
	}
}

// TestChatPicker_AgentPickerEnterEmitsOverride verifies that confirming
// the agent picker with Enter returns a switchViewMsg whose agentName
// matches the cursor's agent — the per-chat override pipeline.
func TestChatPicker_AgentPickerEnterEmitsOverride(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(agentsMsg{agents: []client.AgentInfo{
		{Name: "claude"},
		{Name: "codex"},
	}})
	m = updated.(ChatPickerModel)

	updated, _ = m.Update(keyPress('n'))
	m = updated.(ChatPickerModel)
	if !m.pickingAgent {
		t.Fatalf("setup: expected pickingAgent=true")
	}

	// Cursor defaults to row 0 ("claude"). Press enter.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := updated.(ChatPickerModel)
	if got.pickingAgent {
		t.Errorf("expected pickingAgent=false after enter")
	}
	if cmd == nil {
		t.Fatal("expected a switchViewMsg cmd from enter")
	}
	out := cmd()
	sw, ok := out.(switchViewMsg)
	if !ok {
		t.Fatalf("expected switchViewMsg, got %T", out)
	}
	if sw.agentName != "claude" {
		t.Errorf("switchViewMsg.agentName = %q, want %q", sw.agentName, "claude")
	}
}

// TestChatPicker_AgentPickerEscCancels verifies that Esc while in the
// agent picker returns to the main chat list with no view switch.
func TestChatPicker_AgentPickerEscCancels(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(agentsMsg{agents: []client.AgentInfo{
		{Name: "claude"},
		{Name: "codex"},
	}})
	m = updated.(ChatPickerModel)

	updated, _ = m.Update(keyPress('n'))
	m = updated.(ChatPickerModel)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := updated.(ChatPickerModel)
	if got.pickingAgent {
		t.Errorf("expected pickingAgent=false after esc")
	}
	if got.cancel {
		t.Errorf("esc inside agent picker must not cancel the chat picker itself")
	}
	if cmd != nil {
		t.Errorf("esc should not emit a cmd, got %T", cmd)
	}
}

func TestChatPicker_WakeResultMsg_ErrorSurfaced(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusStopped)

	updated, _ := m.Update(wakeResultMsg{
		agentSessionID: "agent-1",
		err:            errors.New("daemon down"),
	})
	got := updated.(ChatPickerModel).statusMsg
	want := "Wake failed: daemon down"
	if got != want {
		t.Errorf("statusMsg = %q, want %q", got, want)
	}
}
