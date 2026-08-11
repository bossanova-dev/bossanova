package views

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
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
	session       *pb.Session
	repos         []*pb.Repo

	// Switch-account canned data (BOS-171). accounts is returned by
	// ListAccounts; switchResp / switchErr drive SwitchSessionAccount; and
	// switchCalls records the requests the TUI dispatched.
	accounts    []*pb.Account
	switchResp  *pb.SwitchSessionAccountResponse
	switchErr   error
	switchCalls []*pb.SwitchSessionAccountRequest
}

type wakeChatCall struct {
	sessionID      string
	agentSessionID string
	forceFresh     bool
}

func (s *chatPickerStub) DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error) {
	return nil, nil
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
func (s *chatPickerStub) GetSession(context.Context, string, client.SessionReadOptions) (*pb.Session, error) {
	if s.session != nil {
		return s.session, nil
	}
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
func (s *chatPickerStub) ListRepos(context.Context) ([]*pb.Repo, error) { return s.repos, nil }
func (s *chatPickerStub) RemoveRepo(context.Context, string) error      { panic("unused") }
func (s *chatPickerStub) UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *chatPickerStub) ListSessions(context.Context, *pb.ListSessionsRequest, client.SessionReadOptions) ([]*pb.Session, error) {
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
func (s *chatPickerStub) MergeSession(context.Context, string) (*pb.Session, string, error) {
	panic("unused")
}
func (s *chatPickerStub) RemoveSession(context.Context, string) error { panic("unused") }
func (s *chatPickerStub) UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error) {
	panic("unused")
}
func (s *chatPickerStub) LinkSessionPR(context.Context, string, string) (*pb.Session, error) {
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
func (s *chatPickerStub) GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) SendChatMessage(context.Context, *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) NotifyAuthChange(context.Context, string) error { return nil }
func (s *chatPickerStub) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	panic("unused")
}
func (s *chatPickerStub) ListTrackerIssues(context.Context, string, string, string) ([]*pb.TrackerIssue, error) {
	panic("unused")
}
func (s *chatPickerStub) CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *chatPickerStub) GetCronJob(context.Context, string) (*pb.CronJob, error) { panic("unused") }
func (s *chatPickerStub) ListCronJobs(context.Context, string) ([]*pb.CronJob, error) {
	panic("unused")
}
func (s *chatPickerStub) UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *chatPickerStub) DeleteCronJob(context.Context, string) error { panic("unused") }
func (s *chatPickerStub) CreateGithubCallback(context.Context, *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
	panic("unused")
}
func (s *chatPickerStub) ListGithubCallbacks(context.Context, *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	panic("unused")
}
func (s *chatPickerStub) DeleteGithubCallback(context.Context, string, string) error { panic("unused") }
func (s *chatPickerStub) CreateNote(context.Context, *pb.CreateNoteRequest) (*pb.Note, error) {
	panic("unused")
}
func (s *chatPickerStub) GetNote(context.Context, string, string) (*pb.Note, error) { panic("unused") }
func (s *chatPickerStub) ListNotes(context.Context, *pb.ListNotesRequest) ([]*pb.Note, error) {
	panic("unused")
}
func (s *chatPickerStub) UpdateNote(context.Context, string, *pb.UpdateNoteRequest) (*pb.Note, error) {
	panic("unused")
}
func (s *chatPickerStub) DeleteNote(context.Context, string, string) error { panic("unused") }
func (s *chatPickerStub) SendBroadcast(context.Context, *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) ListBroadcasts(context.Context, *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error) {
	panic("unused")
}
func (s *chatPickerStub) DeleteBroadcast(context.Context, string) error { panic("unused") }
func (s *chatPickerStub) CreateBroadcastSubscription(context.Context, *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
	panic("unused")
}
func (s *chatPickerStub) ListBroadcastSubscriptions(context.Context, *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error) {
	panic("unused")
}
func (s *chatPickerStub) DeleteBroadcastSubscription(context.Context, string) error { panic("unused") }
func (s *chatPickerStub) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) ListAccounts(context.Context, string, bool) ([]*pb.Account, error) {
	return s.accounts, nil
}
func (s *chatPickerStub) AddAccount(context.Context, *pb.AddAccountRequest) (*pb.Account, error) {
	panic("unused")
}
func (s *chatPickerStub) UpdateAccount(context.Context, *pb.UpdateAccountRequest) (*pb.Account, error) {
	panic("unused")
}
func (s *chatPickerStub) RemoveAccount(context.Context, string) error { panic("unused") }
func (s *chatPickerStub) TestAccount(context.Context, string) (*pb.TestAccountResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) RepairDoctor(context.Context) (*pb.RepairDoctorResponse, error) {
	panic("unused")
}

func (s *chatPickerStub) StartRepairWorkflow(context.Context) (*pb.StartRepairWorkflowResponse, error) {
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

func TestChatPickerBuildTableRows_ShowsAgentAfterChatWhenMultipleAgentsEnabled(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.agents = []client.AgentInfo{{Name: "claude"}, {Name: "codex"}}
	now := timestamppb.Now()
	m.chats = []*pb.ClaudeChat{
		{
			SessionId:      "session-1",
			AgentSessionId: "agent-1",
			Title:          "Claude chat",
			AgentName:      "claude",
			CreatedAt:      now,
		},
		{
			SessionId:      "session-1",
			AgentSessionId: "agent-2",
			Title:          "Codex chat",
			AgentName:      "codex",
			CreatedAt:      now,
		},
	}

	m.buildTableRows()

	rows := m.table.Rows()
	if got := rows[0][2]; got != "claude" {
		t.Fatalf("chat row AGENT column = %q, want claude", got)
	}
	if got := rows[1][2]; got != "codex" {
		t.Fatalf("chat row AGENT column = %q, want codex", got)
	}
}

// responsiveChatPickerModel builds a board whose long title makes the
// responsive tiers exercise the chat picker's declared-column policy.
func responsiveChatPickerModel(width int, multipleAgents bool) ChatPickerModel {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.width = width
	m.agents = []client.AgentInfo{{Name: "claude"}}
	if multipleAgents {
		m.agents = append(m.agents, client.AgentInfo{Name: "codex"})
	}
	now := timestamppb.Now()
	m.chats = []*pb.ClaudeChat{{
		SessionId:      "session-1",
		AgentSessionId: "agent-1",
		AgentName:      "claude-enter",
		Title:          "Make the chat picker fit a narrow terminal without losing its useful status information",
		CreatedAt:      now,
	}}
	m.daemonStatuses = map[string]string{"agent-1": statusWorking}
	m.buildTableRows()
	return m
}

func chatPickerColumnTitles(cols []table.Column) []string {
	titles := make([]string, len(cols))
	for i, col := range cols {
		titles[i] = col.Title
	}
	return titles
}

// TestChatPickerResponsiveColumns proves the chat board keeps its identity
// columns, sheds the least useful metadata first, and never leaves rows out of
// sync with the fitted headers.
func TestChatPickerResponsiveColumns(t *testing.T) {
	tests := []struct {
		width  int
		single []string
		multi  []string
	}{
		{60, []string{" ", "CHAT"}, []string{" ", "CHAT"}},
		{72, []string{" ", "CHAT"}, []string{" ", "CHAT"}},
		{80, []string{" ", "CHAT", "STATUS"}, []string{" ", "CHAT", "STATUS"}},
		{100, []string{" ", "CHAT", "CREATED", "ACTIVE", "STATUS"}, []string{" ", "CHAT", "ACTIVE", "STATUS"}},
		{140, []string{" ", "CHAT", "CREATED", "ACTIVE", "STATUS"}, []string{" ", "CHAT", "AGENT", "CREATED", "ACTIVE", "STATUS"}},
	}

	for _, multipleAgents := range []bool{false, true} {
		branch := "single agent"
		if multipleAgents {
			branch = "multiple agents"
		}
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/%d_columns", branch, tt.width), func(t *testing.T) {
				m := responsiveChatPickerModel(tt.width, multipleAgents)
				want := tt.single
				if multipleAgents {
					want = tt.multi
				}
				cols := m.table.Columns()
				if got := chatPickerColumnTitles(cols); !reflect.DeepEqual(got, want) {
					t.Fatalf("column titles = %v, want %v", got, want)
				}
				if got, avail := columnsWidth(cols), fitAvailWidth(m.width, chatPickerBlockPadding); got > avail {
					t.Errorf("columnsWidth = %d, want <= fitted table width %d", got, avail)
				}
				if !slices.Contains(chatPickerColumnTitles(cols), "CHAT") {
					t.Fatal("CHAT column was dropped")
				}
				for rowIndex, row := range m.table.Rows() {
					if len(row) != len(cols) {
						t.Errorf("row %d has %d cells, want one per fitted column (%d)", rowIndex, len(row), len(cols))
					}
				}
			})
		}
	}

	for _, multipleAgents := range []bool{false, true} {
		full := responsiveChatPickerModel(140, multipleAgents)
		unfitted := responsiveChatPickerModel(0, multipleAgents)
		if got, want := full.table.Columns(), unfitted.table.Columns(); !reflect.DeepEqual(got, want) {
			t.Errorf("140-column headers = %+v, want byte-identical unfitted headers %+v", got, want)
		}
	}
}

func TestChatPickerResponsiveTableKeepsProseAndHTTPReservationsAt72Columns(t *testing.T) {
	m := responsiveChatPickerModel(72, true)
	if got, tableWidth := m.blockWrapWidth(), columnsWidth(m.table.Columns()); got > tableWidth {
		t.Errorf("blockWrapWidth() = %d, want <= fitted table width %d", got, tableWidth)
	}
	// The prose inset, not the table's (BOS-718): these blocks are drawn inside
	// chatPickerProseBlock, so the room they actually have is what
	// chatPickerProsePadding leaves. Against the table constant this bound is
	// two columns looser than the invariant it claims and would not catch the
	// clamp regressing.
	if got, avail := m.blockWrapWidth(), m.width-chatPickerProsePadding*2; got > avail {
		t.Errorf("blockWrapWidth() = %d, want <= terminal content width %d", got, avail)
	}
	m.session = &pb.Session{Id: "session-1", HttpEndpoints: manyEndpoints(12)}
	if got := m.httpLineHeight(); got != 1 {
		t.Errorf("httpLineHeight() = %d, want 1 at 72 columns", got)
	}
}

func TestChatPickerAgentTableFitsAtResponsiveWidths(t *testing.T) {
	for _, width := range []int{60, 72, 80, 100, 140} {
		t.Run(fmt.Sprintf("%d_columns", width), func(t *testing.T) {
			m := responsiveChatPickerModel(width, true)
			m.buildAgentTable()
			cols := m.agentTable.Columns()
			if got, avail := columnsWidth(cols), fitAvailWidth(m.width, chatPickerBlockPadding); got > avail {
				t.Errorf("agent columnsWidth = %d, want <= %d", got, avail)
			}
			for rowIndex, row := range m.agentTable.Rows() {
				if len(row) != len(cols) {
					t.Errorf("agent row %d has %d cells, want %d", rowIndex, len(row), len(cols))
				}
			}
		})
	}
}

// TestChatPickerResizeRefitsResponsiveTables ensures an already-open picker
// rebuilds its fitted columns when the terminal narrows, rather than waiting
// for a later poll or spinner tick to correct the stale wide table.
func TestChatPickerResizeRefitsResponsiveTables(t *testing.T) {
	assertFitted := func(t *testing.T, cols []table.Column, rows []table.Row, width int) {
		t.Helper()
		if got := columnsWidth(cols); got > width {
			t.Errorf("columnsWidth = %d after resize, want <= terminal width %d", got, width)
		}
		for rowIndex, row := range rows {
			if len(row) != len(cols) {
				t.Errorf("row %d has %d cells after resize, want %d", rowIndex, len(row), len(cols))
			}
		}
	}

	tests := []struct {
		name  string
		build func() ChatPickerModel
		cols  func(ChatPickerModel) []table.Column
		rows  func(ChatPickerModel) []table.Row
	}{
		{
			name: "chat table",
			build: func() ChatPickerModel {
				return responsiveChatPickerModel(140, true)
			},
			cols: func(m ChatPickerModel) []table.Column { return m.table.Columns() },
			rows: func(m ChatPickerModel) []table.Row { return m.table.Rows() },
		},
		{
			name: "agent picker",
			build: func() ChatPickerModel {
				m := responsiveChatPickerModel(140, true)
				m.pickingAgent = true
				m.buildAgentTable()
				return m
			},
			cols: func(m ChatPickerModel) []table.Column { return m.agentTable.Columns() },
			rows: func(m ChatPickerModel) []table.Row { return m.agentTable.Rows() },
		},
		{
			name: "switch-account picker",
			build: func() ChatPickerModel {
				m := responsiveChatPickerModel(140, true)
				m.pickingAccount = true
				m.switchAccounts = []*pb.Account{{
					Id:       "acct-1",
					Label:    "Long-running production account with a descriptive operator label",
					Provider: "claude-enter",
					Status:   "active",
					Health:   "healthy",
				}}
				m.buildSwitchAccountTable()
				return m
			},
			cols: func(m ChatPickerModel) []table.Column { return m.switchAccountTable.Columns() },
			rows: func(m ChatPickerModel) []table.Row { return m.switchAccountTable.Rows() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.build()
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
			m = updated.(ChatPickerModel)
			assertFitted(t, tt.cols(m), tt.rows(m), m.width)
		})
	}
}

// TestChatPickerResizePreservesActiveOverlayCursor keeps a live overlay
// selection stable while refitting it for a narrower terminal. Initial-open
// defaults remain owned by each builder; this only applies after navigation.
func TestChatPickerResizePreservesActiveOverlayCursor(t *testing.T) {
	t.Run("agent picker", func(t *testing.T) {
		m := responsiveChatPickerModel(140, true)
		m.pickingAgent = true
		m.buildAgentTable()
		updated, _ := m.Update(keyPress('j'))
		m = updated.(ChatPickerModel)
		wantCursor := m.agentTable.Cursor()

		updated, _ = m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
		m = updated.(ChatPickerModel)
		if got := m.agentTable.Cursor(); got != wantCursor {
			t.Errorf("agent cursor after resize = %d, want %d", got, wantCursor)
		}
		if got := columnsWidth(m.agentTable.Columns()); got > m.width {
			t.Errorf("agent columnsWidth = %d after resize, want <= terminal width %d", got, m.width)
		}
		for rowIndex, row := range m.agentTable.Rows() {
			if len(row) != len(m.agentTable.Columns()) {
				t.Errorf("agent row %d has %d cells after resize, want %d", rowIndex, len(row), len(m.agentTable.Columns()))
			}
		}
	})

	t.Run("switch-account picker", func(t *testing.T) {
		m := responsiveChatPickerModel(140, true)
		m.pickingAccount = true
		m.switchAccounts = []*pb.Account{
			{Id: "acct-1", Label: "Long-running production account with a descriptive operator label", Provider: "claude-enter", Status: "active", Health: "healthy"},
			{Id: "acct-2", Label: "Backup account", Provider: "codex", Status: "active", Health: "healthy"},
		}
		m.buildSwitchAccountTable()
		updated, _ := m.Update(keyPress('j'))
		m = updated.(ChatPickerModel)
		wantCursor := m.switchAccountTable.Cursor()

		updated, _ = m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
		m = updated.(ChatPickerModel)
		if got := m.switchAccountTable.Cursor(); got != wantCursor {
			t.Errorf("switch-account cursor after resize = %d, want %d", got, wantCursor)
		}
		if got := columnsWidth(m.switchAccountTable.Columns()); got > m.width {
			t.Errorf("switch-account columnsWidth = %d after resize, want <= terminal width %d", got, m.width)
		}
		for rowIndex, row := range m.switchAccountTable.Rows() {
			if len(row) != len(m.switchAccountTable.Columns()) {
				t.Errorf("switch-account row %d has %d cells after resize, want %d", rowIndex, len(row), len(m.switchAccountTable.Columns()))
			}
		}
	})
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
		reason  string
		want    string
	}{
		{"resumed", pb.WakeChatResponse_OUTCOME_RESUMED, "", "Resumed"},
		{"already-live", pb.WakeChatResponse_OUTCOME_ALREADY_LIVE, "", "Already live"},
		{"fresh-fallback", pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK, "", "Started fresh"},
		{"fresh-fallback-transcript", pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK, "transcript_missing", "Started fresh: transcript missing"},
		{"fresh-fallback-discovery", pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK, "provider_id_discovery_timeout", "Started fresh: provider session is still being discovered"},
		{"fresh-fallback-legacy-ambiguous", pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK, "legacy_provider_id_discovery_ambiguous", "Started fresh: legacy backfill matched multiple provider sessions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, _ := m.Update(wakeResultMsg{
				agentSessionID: "agent-1",
				resp:           &pb.WakeChatResponse{Outcome: tc.outcome, Reason: tc.reason},
			})
			got := updated.(ChatPickerModel).statusMsg
			if got != tc.want {
				t.Errorf("statusMsg = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChatPicker_CapturesOpenTelemetryOnNewTabSuccess(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.SetTelemetry(rec)

	updated, _ := m.Update(newTabResultMsg{})
	m = updated.(ChatPickerModel)

	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want 1", len(rec.events))
	}
	if rec.events[0] != telemetry.EventChatAttached {
		t.Fatalf("event = %q, want %q", rec.events[0], telemetry.EventChatAttached)
	}
	if got := rec.props[0]["action"]; got != "open" {
		t.Fatalf("action = %v, want open", got)
	}
	assertNoSensitiveTelemetryProps(t, rec.props[0])
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

// TestChatPicker_SurfacesLimitedProviderLine verifies the BOS-167 provider line:
// when a chat's daemon status is limited, the chat picker names the limited
// provider above the chat list so the operator sees which agent hit its cap.
func TestChatPicker_SurfacesLimitedProviderLine(t *testing.T) {
	stub := &chatPickerStub{}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{{
			SessionId:      "session-1",
			AgentSessionId: "agent-1",
			Title:          "A chat",
			AgentName:      "claude",
			CreatedAt:      timestamppb.Now(),
		}},
		daemonStatuses: map[string]string{"agent-1": statusLimited},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{Id: "session-1"}

	rendered := stripANSI(m.View().Content)
	if !strings.Contains(rendered, "usage-limited") || !strings.Contains(rendered, "claude") {
		t.Errorf("chat picker missing limited-provider line naming claude in:\n%s", rendered)
	}
}

// TestChatPicker_LimitedProviderLine unit-tests the helper: only limited chats
// contribute, provider names are de-duplicated, and non-limited providers are
// omitted.
func TestChatPicker_LimitedProviderLine(t *testing.T) {
	m := ChatPickerModel{
		chats: []*pb.ClaudeChat{
			{AgentSessionId: "a", AgentName: "claude"},
			{AgentSessionId: "b", AgentName: "codex"},
			{AgentSessionId: "c", AgentName: "claude"},
		},
		daemonStatuses: map[string]string{
			"a": statusLimited,
			"b": statusWorking,
			"c": statusLimited,
		},
	}
	got := m.limitedProviderLine()
	if !strings.Contains(got, "claude") {
		t.Errorf("expected limited provider claude in %q", got)
	}
	if strings.Contains(got, "codex") {
		t.Errorf("codex is not limited and must not appear in %q", got)
	}
	if n := strings.Count(got, "claude"); n != 1 {
		t.Errorf("expected claude named once (dedup), got %d in %q", n, got)
	}
}

// TestChatPicker_LimitedProviderLine_EmptyWhenNoneLimited confirms the helper
// stays silent (and callers skip the line) when no chat is limited.
func TestChatPicker_LimitedProviderLine_EmptyWhenNoneLimited(t *testing.T) {
	m := ChatPickerModel{
		chats:          []*pb.ClaudeChat{{AgentSessionId: "a", AgentName: "claude"}},
		daemonStatuses: map[string]string{"a": statusWorking},
	}
	if got := m.limitedProviderLine(); got != "" {
		t.Errorf("expected empty limited-provider line, got %q", got)
	}
}

// renderChatPickerWith builds a loaded chat-picker for the given session and
// returns its rendered main-view content. The session warning block (BOS-86)
// is surfaced below the header and above the chat list.
func renderChatPickerWith(t *testing.T, session *pb.Session) string {
	t.Helper()
	stub := &chatPickerStub{}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{{
			SessionId:      "session-1",
			AgentSessionId: "agent-1",
			Title:          "A chat",
			CreatedAt:      timestamppb.Now(),
		}},
		daemonStatuses: map[string]string{"agent-1": statusWorking},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = session
	return m.View().Content
}

func TestChatPicker_SurfacesSessionWarningAboveChatList(t *testing.T) {
	const warn = "finalize failed (pr_failed): worktree has uncommitted changes"
	rendered := renderChatPickerWith(t, &pb.Session{
		Id: "session-1",
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Summary:        warn,
		},
	})
	// Reflowed before comparing: since BOS-532 the warning block wraps at
	// blockWrapWidth() (the table's content width, floored at
	// minStatusWrapWidth) rather than at the terminal width, so this 61-column
	// warning wraps across two rows on the narrow fixture table. The assertion
	// is still that the *full* warning is on screen — reflowStatusBlock only
	// removes the wrap point, so a truncated warning still fails.
	if !strings.Contains(reflowStatusBlock(rendered), reflowStatusBlock(warn)) {
		t.Errorf("view-session screen missing full warning %q in:\n%s", warn, rendered)
	}
}

func TestChatPicker_NoWarningBlockForCleanSession(t *testing.T) {
	rendered := renderChatPickerWith(t, &pb.Session{Id: "session-1", Title: "clean"})
	if strings.Contains(rendered, "finalize failed") || strings.Contains(rendered, "repair failed") {
		t.Errorf("view-session screen rendered a warning block for a clean session:\n%s", rendered)
	}
}

// TestChatPicker_RendersRotationHistoryBelowActionBar guards the BOS-432
// placement: the rotation-history block moved to the very bottom of the
// chat-picker view, so its detail line must render AFTER the "[esc] back"
// action bar (not above the chat list as it did before). A regression that
// restored the block to the top would place the detail before the action bar
// and fail here. The scenario proof captures the same ordering visually; this
// is the code-level regression guard.
func TestChatPicker_RendersRotationHistoryBelowActionBar(t *testing.T) {
	const detail = "stale failover-proxy port: pane baked 52106 (BOS-409)"
	rendered := stripANSI(renderChatPickerWith(t, &pb.Session{
		Id:    "session-1",
		Title: "rotating",
		RotationEvents: []*pb.RotationEvent{{
			Id:        "rot-1",
			Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
			Detail:    detail,
			CreatedAt: timestamppb.Now(),
		}},
	}))
	actionIdx := strings.Index(rendered, "[esc] back")
	if actionIdx == -1 {
		t.Fatalf("chat-picker view missing the [esc] back action bar:\n%s", rendered)
	}
	detailIdx := strings.Index(rendered, detail)
	if detailIdx == -1 {
		t.Fatalf("chat-picker view missing the rotation detail %q:\n%s", detail, rendered)
	}
	if detailIdx < actionIdx {
		t.Errorf("rotation history rendered above the action bar (detail idx %d < action idx %d); BOS-432 requires it below:\n%s", detailIdx, actionIdx, rendered)
	}

	// Exactly one blank line separates the action-bar line from the rotation
	// block (BOS-432 acceptance criterion: "one blank line above"). The action
	// bar's Padding(actionBarPadY, 2) bottom-pad already supplies that single
	// blank line, so View writes only a "\n"; a regression back to "\n\n" would
	// render two blank lines here.
	lines := strings.Split(trimLineRightSpace(rendered), "\n")
	actionLine := -1
	for i, ln := range lines {
		if strings.Contains(ln, "[esc] back") {
			actionLine = i
			break
		}
	}
	if actionLine == -1 {
		t.Fatalf("could not locate the action-bar line in:\n%s", rendered)
	}
	blanks := 0
	for i := actionLine + 1; i < len(lines) && lines[i] == ""; i++ {
		blanks++
	}
	if blanks != 1 {
		t.Errorf("expected exactly one blank line between the action bar and the rotation block, got %d:\n%s", blanks, rendered)
	}
}

// TestChatPicker_SingleBlankLineAroundSessionWarning pins the spacing around
// the finalize/repair warning block (BOS-122). The header banner already
// renders a single blank line below the worktree-path line, so the chat
// picker must NOT add a second one above the warning: its content must not
// begin with a blank line. Below the warning there must remain exactly one
// blank line before the chat table (must not regress).
func TestChatPicker_SingleBlankLineAroundSessionWarning(t *testing.T) {
	const warn = "finalize failed (pr_failed): worktree has uncommitted changes"
	rendered := renderChatPickerWith(t, &pb.Session{
		Id: "session-1",
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Summary:        warn,
		},
	})

	// No blank line above the warning block: the banner owns the single blank
	// line above, so the chat picker's own content must not lead with one.
	if strings.HasPrefix(rendered, "\n") {
		t.Errorf("chat picker rendered a leading blank line above the warning block:\n%q", rendered)
	}

	// Exactly one blank line below the warning block before the chat table.
	// Locate the warning block, then advance past all of its (possibly
	// wrapped) lines so the gap assertion holds regardless of how many lines
	// the warning renders to.
	lines := strings.Split(trimLineRightSpace(stripANSI(rendered)), "\n")
	warnIdx := -1
	for i, ln := range lines {
		if strings.Contains(ln, "finalize failed") {
			warnIdx = i
			break
		}
	}
	if warnIdx == -1 {
		t.Fatalf("warning text %q not found in:\n%s", warn, rendered)
	}
	blockEnd := warnIdx
	for blockEnd+1 < len(lines) && lines[blockEnd+1] != "" {
		blockEnd++
	}
	if blockEnd+2 >= len(lines) {
		t.Fatalf("not enough lines after the warning block (block ends at %d of %d):\n%s", blockEnd, len(lines), rendered)
	}
	if lines[blockEnd+1] != "" {
		t.Errorf("expected exactly one blank line directly below the warning block, got %q", lines[blockEnd+1])
	}
	if lines[blockEnd+2] == "" {
		t.Errorf("expected the chat table directly after the single blank line, got a second blank line:\n%s", rendered)
	}
}

// TestChatPicker_WarningBlockHeightMatchesRenderedRows guards the coupling
// between tableHeight's reservation (warningBlockHeight) and what View
// actually renders. Removing the blank line above the warning (BOS-122)
// without shrinking the reservation would over-reserve a row and drop a chat
// from the table in constrained terminals.
func TestChatPicker_WarningBlockHeightMatchesRenderedRows(t *testing.T) {
	const warn = "finalize failed (pr_failed): worktree has uncommitted changes"
	stub := &chatPickerStub{}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{{
			SessionId:      "session-1",
			AgentSessionId: "agent-1",
			Title:          "A chat",
			CreatedAt:      timestamppb.Now(),
		}},
		daemonStatuses: map[string]string{"agent-1": statusWorking},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{
		Id:              "session-1",
		AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Summary: warn},
	}

	rendered := m.View().Content
	lines := strings.Split(trimLineRightSpace(stripANSI(rendered)), "\n")
	tableIdx := -1
	for i, ln := range lines {
		if strings.Contains(ln, "CHAT") {
			tableIdx = i
			break
		}
	}
	if tableIdx == -1 {
		t.Fatalf("chat table header not found in:\n%s", rendered)
	}
	// The warning block plus its single trailing blank line occupy exactly
	// tableIdx lines before the table; the reservation must match.
	if got := m.warningBlockHeight(); got != tableIdx {
		t.Errorf("warningBlockHeight()=%d but the warning block + blank line occupy %d rendered lines before the table; table-height reservation is out of sync:\n%s", got, tableIdx, rendered)
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

// TestChatPicker_AgentPickerDefaultsToSessionAgent covers the no-configured-
// default case, where the session's own agent is the fallback cursor. When a
// default IS configured it wins instead — see
// TestChatPicker_AgentPickerPrefersDefaultAgentOverSessionAgent.
func TestChatPicker_AgentPickerDefaultsToSessionAgent(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.session = &pb.Session{Id: "session-1", AgentName: "codex"}
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

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a switchViewMsg cmd from enter")
	}
	_ = updated.(ChatPickerModel)
	out := cmd()
	sw, ok := out.(switchViewMsg)
	if !ok {
		t.Fatalf("expected switchViewMsg, got %T", out)
	}
	if sw.agentName != "codex" {
		t.Errorf("switchViewMsg.agentName = %q, want %q", sw.agentName, "codex")
	}
}

// seedAgentPicker builds a chat picker whose session carries sessionAgent, whose
// configured default is preferred (empty = unset), and whose daemon has loaded
// the named runners in the given order.
func seedAgentPicker(sessionAgent, preferred string, loaded []string) ChatPickerModel {
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{Id: "session-1", AgentName: sessionAgent}
	m.SetPreferredAgent(preferred)
	agents := make([]client.AgentInfo, len(loaded))
	for i, name := range loaded {
		agents[i] = client.AgentInfo{Name: name}
	}
	updated, _ := m.Update(agentsMsg{agents: agents})
	return updated.(ChatPickerModel)
}

// confirmAgentPickerDefault opens the [n] overlay and presses Enter on whatever
// the default cursor landed on, returning the updated model and the agent name
// the emitted switchViewMsg carries.
func confirmAgentPickerDefault(t *testing.T, m ChatPickerModel) (ChatPickerModel, string) {
	t.Helper()
	updated, _ := m.Update(keyPress('n'))
	m = updated.(ChatPickerModel)
	if !m.pickingAgent {
		t.Fatalf("setup: expected pickingAgent=true")
	}
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(ChatPickerModel)
	if cmd == nil {
		t.Fatal("expected a switchViewMsg cmd from enter")
	}
	out := cmd()
	sw, ok := out.(switchViewMsg)
	if !ok {
		t.Fatalf("expected switchViewMsg, got %T", out)
	}
	return m, sw.agentName
}

// TestChatPicker_AgentPickerPrefersDefaultAgentOverSessionAgent is the
// regression. sessions.agent_name is written once at create time and never
// updated, so a session created with one runner used to default every later
// [n]ew chat to that runner permanently — even after the operator had set a
// different default and started many chats on it. The configured default now
// leads.
func TestChatPicker_AgentPickerPrefersDefaultAgentOverSessionAgent(t *testing.T) {
	m := seedAgentPicker("opencode", "claude", []string{"claude", "codex", "opencode"})

	_, agentName := confirmAgentPickerDefault(t, m)

	if agentName != "claude" {
		t.Errorf("agentName = %q, want %q (configured default must beat the session's create-time agent)", agentName, "claude")
	}
}

// TestChatPicker_AgentPickerFallsBackToSessionAgentWhenDefaultUnloaded pins the
// second rung of the precedence: a default naming a runner this daemon has not
// loaded is not selectable, so the session's agent still serves as the fallback
// rather than silently dropping to row 0.
func TestChatPicker_AgentPickerFallsBackToSessionAgentWhenDefaultUnloaded(t *testing.T) {
	m := seedAgentPicker("codex", "opencode", []string{"claude", "codex"})

	_, agentName := confirmAgentPickerDefault(t, m)

	if agentName != "codex" {
		t.Errorf("agentName = %q, want %q (unloaded default must fall back to the session agent)", agentName, "codex")
	}
}

// TestChatPicker_AgentPickerFallsBackToFirstRowWhenNeitherLoaded pins the last
// rung: neither the default nor the session agent is loaded, so the cursor
// lands on row 0 instead of an out-of-range index.
func TestChatPicker_AgentPickerFallsBackToFirstRowWhenNeitherLoaded(t *testing.T) {
	m := seedAgentPicker("opencode", "opencode", []string{"claude", "codex"})

	_, agentName := confirmAgentPickerDefault(t, m)

	if agentName != "claude" {
		t.Errorf("agentName = %q, want %q (first loaded runner)", agentName, "claude")
	}
}

// TestChatPicker_AgentPickerPersistsSelection pins the other half of the fix:
// confirming a pick writes it back through the selection handler, so the next
// [n] — in this session or any other, and in the new-session wizard, which
// writes the same setting — opens on the runner last chosen.
func TestChatPicker_AgentPickerPersistsSelection(t *testing.T) {
	var saved []string
	m := seedAgentPicker("codex", "", []string{"claude", "codex"})
	m.SetAgentSelectionHandler(func(name string) error {
		saved = append(saved, name)
		return nil
	})

	got, agentName := confirmAgentPickerDefault(t, m)

	if agentName != "codex" {
		t.Fatalf("setup: agentName = %q, want %q", agentName, "codex")
	}
	if !slices.Equal(saved, []string{"codex"}) {
		t.Errorf("saved = %v, want exactly one save of %q", saved, "codex")
	}
	if got.preferredAgent != "codex" {
		t.Errorf("preferredAgent = %q, want %q — the in-memory default must track the pick too", got.preferredAgent, "codex")
	}
}

// TestChatPicker_AgentPickerSaveFailureDoesNotBlockChat pins that a settings
// write that fails is reported on the transient status line and nothing more:
// the chat the operator asked for still starts, and m.err — which would replace
// the entire view with an error screen — stays nil.
func TestChatPicker_AgentPickerSaveFailureDoesNotBlockChat(t *testing.T) {
	m := seedAgentPicker("codex", "", []string{"claude", "codex"})
	m.SetAgentSelectionHandler(func(string) error {
		return errors.New("settings are read-only")
	})

	got, agentName := confirmAgentPickerDefault(t, m)

	if agentName != "codex" {
		t.Errorf("agentName = %q, want %q — a failed save must not change the chat's agent", agentName, "codex")
	}
	if got.err != nil {
		t.Errorf("err = %v, want nil — a failed settings write must not take over the view", got.err)
	}
	if !strings.Contains(got.statusMsg, "read-only") {
		t.Errorf("statusMsg = %q, want it to carry the save failure", got.statusMsg)
	}
	if got.preferredAgent != "codex" {
		t.Errorf("preferredAgent = %q, want %q even when the write failed", got.preferredAgent, "codex")
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

func TestChatPicker_LoadsGitHubRepoWebLink(t *testing.T) {
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1"},
		repos: []*pb.Repo{
			{Id: "repo-1", OriginUrl: "git@github.com:owner/repo.git"},
		},
	}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")

	updated, cmd := m.Update(chatPickerSessionMsg{session: stub.session})
	m = updated.(ChatPickerModel)
	if cmd == nil {
		t.Fatal("expected session load to return a batched command")
	}

	msg := m.fetchRepoWebLink()()
	updated, _ = m.Update(msg)
	m = updated.(ChatPickerModel)

	if m.repoWebLink.provider != "github" {
		t.Fatalf("repoWebLink.provider = %q, want github", m.repoWebLink.provider)
	}
	if m.repoWebLink.url != "https://github.com/owner/repo" {
		t.Fatalf("repoWebLink.url = %q, want https://github.com/owner/repo", m.repoWebLink.url)
	}
}

func TestChatPicker_LoadsGitHubPullRequestWebLinkWhenPRNumberKnown(t *testing.T) {
	prNumber := int32(42)
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber},
		repos: []*pb.Repo{
			{Id: "repo-1", OriginUrl: "git@github.com:owner/repo.git"},
		},
	}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	m.session = stub.session

	msg := m.fetchRepoWebLink()()
	updated, _ := m.Update(msg)
	m = updated.(ChatPickerModel)

	if m.repoWebLink.provider != "github" {
		t.Fatalf("repoWebLink.provider = %q, want github", m.repoWebLink.provider)
	}
	if m.repoWebLink.url != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("repoWebLink.url = %q, want https://github.com/owner/repo/pull/42", m.repoWebLink.url)
	}
}

func TestChatPicker_RefreshRefetchesWebLinkWhenPRNumberAppears(t *testing.T) {
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1"},
		repos: []*pb.Repo{
			{Id: "repo-1", OriginUrl: "git@github.com:owner/repo.git"},
		},
	}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	m.session = stub.session

	// Initial fetch happens before a PR exists: caches the plain repo URL.
	updated, _ := m.Update(m.fetchRepoWebLink()())
	m = updated.(ChatPickerModel)
	if m.repoWebLink.url != "https://github.com/owner/repo" {
		t.Fatalf("initial repoWebLink.url = %q, want plain repo URL", m.repoWebLink.url)
	}

	// Polling later discovers the PR number; the refresh must re-fetch the link.
	prNumber := int32(42)
	refreshed := &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber}
	updated, cmd := m.Update(chatPickerRefreshMsg{session: refreshed})
	m = updated.(ChatPickerModel)
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

	updated, _ = m.Update(cmd())
	m = updated.(ChatPickerModel)
	if m.repoWebLink.url != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("repoWebLink.url = %q, want PR URL after PR number appears", m.repoWebLink.url)
	}
	if !m.canOpenGitHub() {
		t.Fatal("canOpenGitHub must be true once the PR web link is installed")
	}
}

// TestChatPicker_ArchivePendingShowsArchiving guards the BOS-425 daemon-driven
// signal: when a refresh reports a merged session whose daemon has an archive
// actually in flight (archive_pending=true), the detail view shows the
// "Archiving..." status. This replaced the old BOS-46 heuristic that inferred
// archiving from MERGED + repo flag alone (which never cleared for a
// resurrected merged session).
func TestChatPicker_ArchivePendingShowsArchiving(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	updated, _ = m.Update(chatPickerRefreshMsg{session: &pb.Session{
		Id:                                  "session-1",
		DisplayStatus:                       pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		RepoShouldArchiveSessionsAfterMerge: true,
		ArchivePending:                      true,
	}})
	cp := updated.(ChatPickerModel)
	if !cp.isArchiving() {
		t.Fatal("expected isArchiving() true for a session with archive_pending set")
	}
	if !strings.Contains(cp.View().Content, "Archiving") {
		t.Errorf("expected the detail view to show an Archiving status:\n%s", cp.View().Content)
	}
}

// TestChatPicker_MergedWithArchiveFlagButNotPendingDoesNotShowArchiving is the
// BOS-425 regression guard: a merged session in an archive-after-merge repo with
// archive_pending=false (the resurrected case) must NOT be considered archiving.
func TestChatPicker_MergedWithArchiveFlagButNotPendingDoesNotShowArchiving(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatPickerRefreshMsg{session: &pb.Session{
		Id:                                  "session-1",
		DisplayStatus:                       pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		RepoShouldArchiveSessionsAfterMerge: true,
		ArchivePending:                      false,
	}})
	if cp := updated.(ChatPickerModel); cp.isArchiving() {
		t.Error("expected isArchiving() false for a resurrected merged session (archive_pending=false)")
	}
}

// TestChatPicker_RefreshClearsOptimisticLatchWhenArchiveSettingIsDisabled guards
// against leaving the detail view in a permanent "Archiving..." state after
// another client disables the repo setting between polls, so the optimistic
// merge latch cannot get stuck.
func TestChatPicker_RefreshClearsOptimisticLatchWhenArchiveSettingIsDisabled(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.optimisticArchiveLatch = true
	updated, _ := m.Update(chatPickerRefreshMsg{session: &pb.Session{
		Id:            "session-1",
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
	}})
	cp := updated.(ChatPickerModel)
	if cp.optimisticArchiveLatch {
		t.Error("expected refresh with archive-after-merge disabled to clear the optimistic latch")
	}
}

func TestChatPicker_RefreshDoesNotRefetchWebLinkWhenPRNumberUnchanged(t *testing.T) {
	prNumber := int32(42)
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber},
	}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	m.session = stub.session

	refreshed := &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber}
	_, cmd := m.Update(chatPickerRefreshMsg{session: refreshed})
	if cmd != nil {
		t.Fatal("expected no web-link re-fetch when the PR number is unchanged")
	}
}

func TestChatPicker_DiscardsStaleRepoWebLinkAfterPRChanges(t *testing.T) {
	prNumber := int32(42)
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.session = &pb.Session{Id: "session-1", RepoId: "repo-1", PrNumber: &prNumber}

	// The current PR-targeted fetch installs the PR URL.
	updated, _ := m.Update(repoWebLinkMsg{
		repoID:   "repo-1",
		prNumber: 42,
		link:     repoWebLink{provider: "github", url: "https://github.com/owner/repo/pull/42"},
	})
	m = updated.(ChatPickerModel)

	// A slow fetch started before the PR existed (prNumber=0) resolves late with
	// the plain repo URL. It must be discarded, not overwrite the PR link.
	updated, _ = m.Update(repoWebLinkMsg{
		repoID:   "repo-1",
		prNumber: 0,
		link:     repoWebLink{provider: "github", url: "https://github.com/owner/repo"},
	})
	m = updated.(ChatPickerModel)

	if m.repoWebLink.url != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("repoWebLink.url = %q, want PR URL preserved against stale fetch", m.repoWebLink.url)
	}
	if !m.canOpenGitHub() {
		t.Fatal("canOpenGitHub must remain true after discarding the stale fetch")
	}
}

func TestChatPicker_HidesGitHubActionForNonGitHubRepo(t *testing.T) {
	stub := &chatPickerStub{
		session: &pb.Session{Id: "session-1", RepoId: "repo-1"},
		repos: []*pb.Repo{
			{Id: "repo-1", OriginUrl: "git@gitlab.com:owner/repo.git"},
		},
	}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	m.session = stub.session

	msg := m.fetchRepoWebLink()()
	updated, _ := m.Update(msg)
	m = updated.(ChatPickerModel)

	if m.repoWebLink.url != "" {
		t.Fatalf("repoWebLink.url = %q, want empty", m.repoWebLink.url)
	}
}

func TestChatPicker_G_OpensGitHubRepo(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.repoWebLink = repoWebLink{provider: "github", url: "https://github.com/owner/repo"}
	// canOpenGitHub requires a known PR number.
	prNum := int32(42)
	m.session = &pb.Session{Id: "session-1", PrNumber: &prNum}

	var opened string
	oldOpenURL := openURLFunc
	openURLFunc = func(rawURL string) error {
		opened = rawURL
		return nil
	}
	defer func() { openURLFunc = oldOpenURL }()

	_, cmd := m.Update(keyPress('g'))
	if cmd == nil {
		t.Fatal("expected a command from pressing g with a GitHub web link and known PR")
	}
	_ = cmd()
	if opened != "https://github.com/owner/repo" {
		t.Fatalf("opened URL = %q, want https://github.com/owner/repo", opened)
	}
}

func TestChatPicker_G_IsNoOpWithGitHubLinkButNoPR(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.repoWebLink = repoWebLink{provider: "github", url: "https://github.com/owner/repo"}
	// session has no pr_number — canOpenGitHub must return false.
	m.chats = append(m.chats, &pb.ClaudeChat{
		SessionId:      "session-1",
		AgentSessionId: "agent-2",
		Title:          "Second chat",
		CreatedAt:      timestamppb.Now(),
	})
	m.buildTableRows()
	m.table.SetCursor(1)

	var opened bool
	oldOpenURL := openURLFunc
	openURLFunc = func(string) error {
		opened = true
		return nil
	}
	defer func() { openURLFunc = oldOpenURL }()

	updated, cmd := m.Update(keyPress('g'))
	if cmd != nil {
		_ = cmd()
	}
	m = updated.(ChatPickerModel)
	if opened {
		t.Fatal("openURLFunc called when session has no PR number; g should be a no-op")
	}
	// g is hidden here, so it must be swallowed — not fall through to the
	// table's go-to-top binding and move the cursor.
	if got := m.table.Cursor(); got != 1 {
		t.Fatalf("table cursor after hidden g = %d, want 1 (g must be swallowed)", got)
	}
}

func TestChatPicker_G_SwallowedWithoutGitHubAction(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	// No repo web link at all — canOpenGitHub is false.
	m.chats = append(m.chats, &pb.ClaudeChat{
		SessionId:      "session-1",
		AgentSessionId: "agent-2",
		Title:          "Second chat",
		CreatedAt:      timestamppb.Now(),
	})
	m.buildTableRows()
	m.table.SetCursor(1)

	var opened bool
	oldOpenURL := openURLFunc
	openURLFunc = func(string) error {
		opened = true
		return nil
	}
	defer func() { openURLFunc = oldOpenURL }()

	updated, cmd := m.Update(keyPress('g'))
	if cmd != nil {
		_ = cmd()
	}
	m = updated.(ChatPickerModel)
	if opened {
		t.Fatal("openURLFunc called without a GitHub web link")
	}
	// The [g]ithub action is hidden, so g must be a no-op rather than moving
	// the table cursor to the top.
	if got := m.table.Cursor(); got != 1 {
		t.Fatalf("table cursor after hidden g = %d, want 1 (g must be swallowed)", got)
	}
}

func TestChatPicker_RendersGitHubActionWhenRepoWebLinkAvailable(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.repoWebLink = repoWebLink{provider: "github", url: "https://github.com/owner/repo"}
	// canOpenGitHub requires a known PR number in addition to the web link.
	prNum := int32(42)
	m.session = &pb.Session{Id: "session-1", PrNumber: &prNum}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if !strings.Contains(rendered, "[g]ithub") {
		t.Fatalf("rendered chat picker missing [g]ithub action:\n%s", rendered)
	}
}

func TestChatPicker_HidesGitHubActionWithGitHubLinkButNoPR(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.repoWebLink = repoWebLink{provider: "github", url: "https://github.com/owner/repo"}
	// session has no pr_number — [g]ithub must not appear.

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[g]ithub") {
		t.Fatalf("rendered chat picker shows [g]ithub without a PR number:\n%s", rendered)
	}
}

func TestChatPicker_HidesGitHubActionWithoutRepoWebLink(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[g]ithub") {
		t.Fatalf("rendered chat picker should not show [g]ithub action without repo link:\\n%s", rendered)
	}
}

// seedChatPickerWithPassingPR returns a ChatPickerModel whose session has an
// open passing PR — the conditions that make canMerge() return true when
// m.merged is false.
func seedChatPickerWithPassingPR() ChatPickerModel {
	prNum := int32(42)
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:            "session-1",
		PrNumber:      &prNum,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
	}
	return m
}

// TestChatPickerCanMerge_ApprovedCleanPR guards the TUI policy against
// rejecting an approved, clean PR that the live MergeSession gate accepts.
func TestChatPickerCanMerge_ApprovedCleanPR(t *testing.T) {
	prNum := int32(42)
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:            "session-1",
		PrNumber:      &prNum,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_APPROVED,
	}

	if !m.canMerge() {
		t.Fatal("canMerge() = false for an approved clean PR; want true because the live MergeSession gate accepts it")
	}
}

// TestChatPickerCanMergeDropsAfterMerge guards that canMerge() returns false
// once m.merged is true, even when the session otherwise qualifies for merge
// (open passing PR). The !m.merged guard is what drops [m]erge from the bar.
func TestChatPickerCanMergeDropsAfterMerge(t *testing.T) {
	m := seedChatPickerWithPassingPR()

	if !m.canMerge() {
		t.Fatal("canMerge() = false before merge; test pre-condition broken (session must have an open passing PR)")
	}

	m.merged = true
	if m.canMerge() {
		t.Fatal("canMerge() = true after merge; expected false so [m]erge drops from the action bar")
	}
}

// TestChatPickerViewAfterMergeShowsMergedStatusAndArchive guards the rendered
// output when m.merged=true: the view must contain a merged indicator and the
// [a]rchive action, but must NOT contain [m]erge.
func TestChatPickerViewAfterMergeShowsMergedStatusAndArchive(t *testing.T) {
	m := seedChatPickerWithPassingPR()
	m.merged = true

	// Give the model a viewport so View() renders the full action bar.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content

	// Must show some merged indicator (e.g. "✓ merged" or "✓ PR #42 merged").
	if !strings.Contains(rendered, "merged") {
		t.Errorf("view after merge missing merged indicator:\n%s", rendered)
	}
	// Archive must still be available.
	if !strings.Contains(rendered, "[a]rchive") {
		t.Errorf("view after merge missing [a]rchive action:\n%s", rendered)
	}
	// Merge action must be gone.
	if strings.Contains(rendered, "[m]erge") {
		t.Errorf("view after merge still shows [m]erge action (should be dropped):\n%s", rendered)
	}
}

// seedChatPickerWithCheckingPR returns a ChatPickerModel whose session has an
// open PR in CHECKING state with the given display_has_failures value.
func seedChatPickerWithCheckingPR(hasFailures bool) ChatPickerModel {
	prNum := int32(42)
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:                 "session-1",
		PrNumber:           &prNum,
		DisplayStatus:      pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
		DisplayHasFailures: hasFailures,
	}
	return m
}

// seedChatPickerWithFailingPR returns a ChatPickerModel whose session has an
// open PR in FAILING state.
func seedChatPickerWithFailingPR() ChatPickerModel {
	prNum := int32(42)
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:            "session-1",
		PrNumber:      &prNum,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING,
	}
	return m
}

// seedChatPickerNoPR returns a ChatPickerModel whose session has no PR number.
func seedChatPickerNoPR() ChatPickerModel {
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:            "session-1",
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
	}
	return m
}

// TestChatPickerCanMerge_NonFailingChecking guards that canMerge() returns
// false even for a non-failing CHECKING PR. The backend MergeSession RPC does
// an immediate merge and rejects anything that is not passing ("PR is not
// passing"), so offering [m]erge while checks are still running would only lead
// the user into a confirm dialog that errors.
func TestChatPickerCanMerge_NonFailingChecking(t *testing.T) {
	m := seedChatPickerWithCheckingPR(false)
	if m.canMerge() {
		t.Fatal("canMerge() = true for non-failing CHECKING PR; expected false (backend rejects non-passing merges)")
	}
}

// TestChatPickerCanMerge_CheckingWithFailures guards that canMerge() returns
// false when display_status is CHECKING and display_has_failures is true.
func TestChatPickerCanMerge_CheckingWithFailures(t *testing.T) {
	m := seedChatPickerWithCheckingPR(true)
	if m.canMerge() {
		t.Fatal("canMerge() = true for CHECKING PR with failures; expected false")
	}
}

// TestChatPickerCanMerge_CheckingWithChangesRequested guards that canMerge()
// returns false when display_status is CHECKING but a reviewer has requested
// changes. status.go renders CHECKING-with-changes-requested in the danger
// style exactly like CHECKING-with-failures, so the merge affordance must be
// hidden in this state too — it is not "non-failing checking".
func TestChatPickerCanMerge_CheckingWithChangesRequested(t *testing.T) {
	prNum := int32(42)
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:                         "session-1",
		PrNumber:                   &prNum,
		DisplayStatus:              pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
		DisplayHasChangesRequested: true,
	}
	if m.canMerge() {
		t.Fatal("canMerge() = true for CHECKING PR with changes requested; expected false")
	}
}

// TestChatPickerCanMerge_FailingPR guards that canMerge() returns false when
// display_status is FAILING.
func TestChatPickerCanMerge_FailingPR(t *testing.T) {
	m := seedChatPickerWithFailingPR()
	if m.canMerge() {
		t.Fatal("canMerge() = true for FAILING PR; expected false")
	}
}

// TestChatPickerCanMerge_NoPR guards that canMerge() returns false when the
// session has no PR number.
func TestChatPickerCanMerge_NoPR(t *testing.T) {
	m := seedChatPickerNoPR()
	if m.canMerge() {
		t.Fatal("canMerge() = true with no PR number; expected false")
	}
}

// TestChatPickerView_HidesMergeForNonFailingChecking guards that the rendered
// action bar omits [m]erge when display_status is CHECKING even with no
// failures — the backend rejects non-passing merges, so the affordance stays
// hidden until the PR is passing.
func TestChatPickerView_HidesMergeForNonFailingChecking(t *testing.T) {
	m := seedChatPickerWithCheckingPR(false)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar shows [m]erge for non-failing CHECKING PR (should be hidden):\n%s", rendered)
	}
}

// TestChatPickerView_HidesMergeForCheckingWithFailures guards that the rendered
// action bar omits [m]erge when display_status is CHECKING with failures.
func TestChatPickerView_HidesMergeForCheckingWithFailures(t *testing.T) {
	m := seedChatPickerWithCheckingPR(true)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar shows [m]erge for CHECKING PR with failures (should be hidden):\n%s", rendered)
	}
}

// TestChatPickerView_HidesMergeForCheckingWithChangesRequested guards that the
// rendered action bar omits [m]erge when display_status is CHECKING but a
// reviewer has requested changes — the View-layer mirror of the predicate test.
func TestChatPickerView_HidesMergeForCheckingWithChangesRequested(t *testing.T) {
	prNum := int32(42)
	m := seedChatPicker(&chatPickerStub{}, statusWorking)
	m.session = &pb.Session{
		Id:                         "session-1",
		PrNumber:                   &prNum,
		DisplayStatus:              pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
		DisplayHasChangesRequested: true,
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar shows [m]erge for CHECKING PR with changes requested (should be hidden):\n%s", rendered)
	}
}

// TestChatPickerView_HidesMergeForFailingPR guards that the rendered action bar
// omits [m]erge when display_status is FAILING.
func TestChatPickerView_HidesMergeForFailingPR(t *testing.T) {
	m := seedChatPickerWithFailingPR()

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar shows [m]erge for FAILING PR (should be hidden):\n%s", rendered)
	}
}

// TestChatPickerView_HidesMergeForNoPR guards that the rendered action bar
// omits [m]erge when the session has no PR.
func TestChatPickerView_HidesMergeForNoPR(t *testing.T) {
	m := seedChatPickerNoPR()

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar shows [m]erge with no PR (should be hidden):\n%s", rendered)
	}
}

// TestChatPicker_M_IsNoOpForFailingPR guards the Update()-level behaviour the
// plan (§3) requires: pressing m when canMerge() is false (here, a FAILING PR)
// must not open the merge confirmation. The g key has a symmetric no-op test;
// this gives m the same direct coverage rather than relying only on the
// canMerge() predicate tests.
func TestChatPicker_M_IsNoOpForFailingPR(t *testing.T) {
	m := seedChatPickerWithFailingPR()

	updated, cmd := m.Update(keyPress('m'))
	m = updated.(ChatPickerModel)
	if cmd != nil {
		t.Fatal("expected no command from pressing m with a FAILING PR")
	}
	if m.confirm != confirmNone {
		t.Fatalf("m.confirm = %v after pressing m on FAILING PR; want confirmNone", m.confirm)
	}
}

// --- [l]inear shortcut tests ---

func TestChatPicker_L_OpensTrackerURL(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	trackerURL := "https://linear.app/myteam/issue/BOS-90"
	m.session = &pb.Session{Id: "session-1", TrackerUrl: &trackerURL}

	var opened string
	oldOpenURL := openURLFunc
	openURLFunc = func(rawURL string) error {
		opened = rawURL
		return nil
	}
	defer func() { openURLFunc = oldOpenURL }()

	_, cmd := m.Update(keyPress('l'))
	if cmd == nil {
		t.Fatal("expected a command from pressing l with a tracker URL set")
	}
	_ = cmd()
	if opened != trackerURL {
		t.Fatalf("opened URL = %q, want %q", opened, trackerURL)
	}
}

func TestChatPicker_RendersLinearActionWhenTrackerURLSet(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	trackerURL := "https://linear.app/myteam/issue/BOS-90"
	m.session = &pb.Session{Id: "session-1", TrackerUrl: &trackerURL}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if !strings.Contains(rendered, "[l]inear") {
		t.Fatalf("rendered chat picker missing [l]inear action:\n%s", rendered)
	}
}

func TestChatPicker_L_IsNoOpWithNoTrackerURL(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	// session has no TrackerUrl — l must be a no-op.
	m.session = &pb.Session{Id: "session-1"}
	m.chats = append(m.chats, &pb.ClaudeChat{
		SessionId:      "session-1",
		AgentSessionId: "agent-2",
		Title:          "Second chat",
		CreatedAt:      timestamppb.Now(),
	})
	m.buildTableRows()
	m.table.SetCursor(1)

	var opened bool
	oldOpenURL := openURLFunc
	openURLFunc = func(string) error {
		opened = true
		return nil
	}
	defer func() { openURLFunc = oldOpenURL }()

	updated, cmd := m.Update(keyPress('l'))
	if cmd != nil {
		_ = cmd()
	}
	m = updated.(ChatPickerModel)
	if opened {
		t.Fatal("openURLFunc called when session has no TrackerUrl; l should be a no-op")
	}
	// l is hidden here, so it must be swallowed — not fall through to a table
	// keybinding and move the cursor.
	if got := m.table.Cursor(); got != 1 {
		t.Fatalf("table cursor after hidden l = %d, want 1 (l must be swallowed)", got)
	}
}

func TestChatPicker_HidesLinearActionWithNoTrackerURL(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	// session has no TrackerUrl.
	m.session = &pb.Session{Id: "session-1"}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[l]inear") {
		t.Fatalf("rendered chat picker shows [l]inear without a tracker URL:\n%s", rendered)
	}
}

func TestChatPicker_L_IsNoOpWithNoSession(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.session = nil
	m.chats = append(m.chats, &pb.ClaudeChat{
		SessionId:      "session-1",
		AgentSessionId: "agent-2",
		Title:          "Second chat",
		CreatedAt:      timestamppb.Now(),
	})
	m.buildTableRows()
	m.table.SetCursor(1)

	var opened bool
	oldOpenURL := openURLFunc
	openURLFunc = func(string) error {
		opened = true
		return nil
	}
	defer func() { openURLFunc = oldOpenURL }()

	updated, cmd := m.Update(keyPress('l'))
	if cmd != nil {
		_ = cmd()
	}
	m = updated.(ChatPickerModel)
	if opened {
		t.Fatal("openURLFunc called when session is nil; l should be a no-op")
	}
	// l is hidden here, so it must be swallowed — not fall through to a table
	// keybinding and move the cursor.
	if got := m.table.Cursor(); got != 1 {
		t.Fatalf("table cursor after hidden l = %d, want 1 (l must be swallowed)", got)
	}
}

func TestChatPicker_HidesLinearActionWithNoSession(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	m.session = nil

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := m.View().Content
	if strings.Contains(rendered, "[l]inear") {
		t.Fatalf("rendered chat picker shows [l]inear with nil session:\n%s", rendered)
	}
}

func TestChatPicker_WebOpenErrorMessageIsGeneric(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)

	// webOpenResultMsg is shared by the [g]ithub and [l]inear shortcuts, so a
	// failed open must not name a specific destination (e.g. "GitHub") that
	// would be wrong for the other shortcut.
	updated, _ := m.Update(webOpenResultMsg{err: errors.New("no browser found")})
	m = updated.(ChatPickerModel)

	if !strings.Contains(m.statusMsg, "Couldn't open browser") {
		t.Fatalf("web-open error statusMsg = %q, want a generic \"Couldn't open browser\" message", m.statusMsg)
	}
	if strings.Contains(m.statusMsg, "GitHub") {
		t.Fatalf("web-open error statusMsg = %q, must not be GitHub-specific (shared with [l]inear)", m.statusMsg)
	}
}

func (s *chatPickerStub) SwitchSessionAccount(_ context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switchCalls = append(s.switchCalls, req)
	if s.switchErr != nil {
		return nil, s.switchErr
	}
	if s.switchResp != nil {
		return s.switchResp, nil
	}
	return &pb.SwitchSessionAccountResponse{}, nil
}

// switchCallsSnapshot returns a copy of the recorded switch requests.
func (s *chatPickerStub) switchCallsSnapshot() []*pb.SwitchSessionAccountRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pb.SwitchSessionAccountRequest, len(s.switchCalls))
	copy(out, s.switchCalls)
	return out
}

// --- BOS-494: initial cursor preselects the pending-question row -------------

// bos494Chat builds a chat with an explicit created-at offset so the tests can
// control both list order (the picker sorts newest-first) and the
// chatLastActive fallback used for the longest-waiting tie-break.
func bos494Chat(agentID, title string, createdAgo time.Duration) *pb.ClaudeChat {
	return &pb.ClaudeChat{
		SessionId:      "session-1",
		AgentSessionId: agentID,
		Title:          title,
		AgentName:      "claude",
		CreatedAt:      timestamppb.New(time.Now().Add(-createdAgo)),
	}
}

// TestChatPicker_InitialCursor_PrefersQuestionOverNewerWorking is the reported
// bug: with no explicit highlight, a newer working chat must not steal the
// cursor from an older chat that is actually asking a question.
func TestChatPicker_InitialCursor_PrefersQuestionOverNewerWorking(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			bos494Chat("agent-working", "Newer working", 1*time.Hour),
			bos494Chat("agent-question", "Older question", 3*time.Hour),
		},
		daemonStatuses: map[string]string{
			"agent-working":  statusWorking,
			"agent-question": statusQuestion,
		},
	})
	m = updated.(ChatPickerModel)
	// Newest-first: [working(row 0), question(row 1)]. Cursor must land on the
	// question row, not the newer working one.
	if got := m.table.Cursor(); got != 1 {
		t.Fatalf("cursor = %d, want 1 (the question row) not the newer working row", got)
	}
}

// TestChatPicker_InitialCursor_LongestWaitingQuestionWins covers the tie-break:
// among several question chats, the longest-waiting one (oldest chatLastActive)
// is selected. The CreatedAt order and the LastOutputAt order deliberately
// DISAGREE so the assertion only passes if the tie-break ranks by
// chatLastActive's LastOutputAt, not by creation time: the winning chat is the
// newer-created one (so it sorts to row 0) but has the older LastOutputAt.
func TestChatPicker_InitialCursor_LongestWaitingQuestionWins(t *testing.T) {
	now := time.Now()
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			// Newer CreatedAt but the OLDEST LastOutputAt — longest-waiting.
			bos494Chat("q-win", "Longest waiting", 1*time.Hour),
			// Older CreatedAt but a RECENT LastOutputAt — not waiting as long.
			bos494Chat("q-lose", "Recently asked", 2*time.Hour),
		},
		daemonStatuses: map[string]string{
			"q-win":  statusQuestion,
			"q-lose": statusQuestion,
		},
		daemonLastOutput: map[string]time.Time{
			"q-win":  now.Add(-30 * time.Minute),
			"q-lose": now.Add(-5 * time.Minute),
		},
	})
	m = updated.(ChatPickerModel)
	// Newest-first by CreatedAt: [q-win(row 0), q-lose(row 1)]. q-win has the
	// older LastOutputAt, so a CreatedAt-based tie-break would wrongly pick row 1
	// (q-lose, older creation); only a LastOutputAt-based tie-break picks row 0.
	if got := m.table.Cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0 (longest-waiting question by LastOutputAt, not CreatedAt)", got)
	}
}

// TestChatPicker_InitialCursor_ExplicitHighlightBeatsQuestion confirms the
// explicit-highlight branch (detach / back-nav) still wins over question
// priority.
func TestChatPicker_InitialCursor_ExplicitHighlightBeatsQuestion(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "agent-working")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			bos494Chat("agent-working", "Newer working", 1*time.Hour),
			bos494Chat("agent-question", "Older question", 3*time.Hour),
		},
		daemonStatuses: map[string]string{
			"agent-working":  statusWorking,
			"agent-question": statusQuestion,
		},
	})
	m = updated.(ChatPickerModel)
	// Explicit highlight on the working chat (row 0) must win over the question row.
	if got := m.table.Cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0 (explicit highlightID honored over question)", got)
	}
}

// TestChatPicker_InitialCursor_NoQuestionKeepsFirstActive guards the unchanged
// behavior: with no question chat, the cursor still lands on the first
// working/idle/limited chat.
func TestChatPicker_InitialCursor_NoQuestionKeepsFirstActive(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			bos494Chat("agent-working", "Newer working", 1*time.Hour),
			bos494Chat("agent-idle", "Older idle", 3*time.Hour),
		},
		daemonStatuses: map[string]string{
			"agent-working": statusWorking,
			"agent-idle":    statusIdle,
		},
	})
	m = updated.(ChatPickerModel)
	// Newest-first: [working(row 0), idle(row 1)]. First active is the working row.
	if got := m.table.Cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0 (first working/idle chat, unchanged behavior)", got)
	}
}

// TestChatPicker_InitialCursor_NilStatusesLeavesRowZero confirms no cursor move
// when heartbeat statuses are unavailable.
func TestChatPicker_InitialCursor_NilStatusesLeavesRowZero(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			bos494Chat("agent-a", "Chat A", 1*time.Hour),
			bos494Chat("agent-b", "Chat B", 2*time.Hour),
		},
		daemonStatuses: nil,
	})
	m = updated.(ChatPickerModel)
	if got := m.table.Cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0 (no daemon statuses leaves the fallback untouched)", got)
	}
}

// --- BOS-474: session HTTP endpoint line above the chat table ---------------

// seedChatPickerForEndpoints builds a sized ChatPicker with one chat, ready for
// a caller to attach a session carrying HTTP endpoints.
func seedChatPickerForEndpoints(t *testing.T) ChatPickerModel {
	t.Helper()
	return seedChatPickerSized(t, 1, 30)
}

// seedChatPickerSized builds a ChatPicker with chatCount chats in a terminal
// termHeight rows tall, so a caller can put the table in the height-constrained
// regime where overhead reservations actually bite.
func seedChatPickerSized(t *testing.T, chatCount, termHeight int) ChatPickerModel {
	t.Helper()
	stub := &chatPickerStub{}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	chats := make([]*pb.ClaudeChat, 0, chatCount)
	statuses := make(map[string]string, chatCount)
	for i := range chatCount {
		id := fmt.Sprintf("agent-%d", i)
		chats = append(chats, &pb.ClaudeChat{
			SessionId:      "session-1",
			AgentSessionId: id,
			Title:          fmt.Sprintf("Chat %d", i),
			CreatedAt:      timestamppb.Now(),
		})
		statuses[id] = statusWorking
	}
	updated, _ := m.Update(chatsListedMsg{chats: chats, daemonStatuses: statuses})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: termHeight})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{Id: "session-1"}
	return m
}

// TestChatPicker_HTTPLineHeightZeroWithoutEndpoints pins that a session with no
// endpoints reserves nothing and renders no HTTP line, so existing output is
// byte-identical.
func TestChatPicker_HTTPLineHeightZeroWithoutEndpoints(t *testing.T) {
	m := seedChatPickerForEndpoints(t)
	if got := m.httpLineHeight(); got != 0 {
		t.Errorf("httpLineHeight() = %d, want 0 with no endpoints", got)
	}
	if strings.Contains(stripANSI(m.View().Content), "HTTP") {
		t.Errorf("no-endpoint view must not mention HTTP:\n%s", m.View().Content)
	}
}

// TestChatPicker_HTTPLineRendersAboveTable verifies the HTTP line occupies
// exactly one line directly above the chat table (no extra blank line), shows
// every port, and that tableHeight's reservation matches what View renders.
func TestChatPicker_HTTPLineRendersAboveTable(t *testing.T) {
	m := seedChatPickerForEndpoints(t)
	m.session = &pb.Session{
		Id: "session-1",
		HttpEndpoints: []*pb.HttpEndpoint{
			{Port: 3000, Url: "http://127.0.0.1:3000"},
			{Port: 5173, Url: "http://127.0.0.1:5173"},
		},
	}

	rendered := m.View().Content
	lines := strings.Split(trimLineRightSpace(stripANSI(rendered)), "\n")
	httpIdx, tableIdx := -1, -1
	for i, ln := range lines {
		if httpIdx == -1 && strings.Contains(ln, "HTTP") {
			httpIdx = i
		}
		if strings.Contains(ln, "CHAT") {
			tableIdx = i
			break
		}
	}
	if httpIdx == -1 || tableIdx == -1 {
		t.Fatalf("HTTP line (%d) or chat table header (%d) not found in:\n%s", httpIdx, tableIdx, rendered)
	}
	if tableIdx != httpIdx+1 {
		t.Errorf("HTTP line at %d, table header at %d: want the table on the immediately following line (no blank line)", httpIdx, tableIdx)
	}
	// BOS-616: the ports are joined by a single space, so assert on the visible
	// text (OSC 8 envelopes stripped) and pin the removal of the old middot.
	visibleHTTP := visibleRowText(lines[httpIdx])
	if !strings.Contains(visibleHTTP, ":3000 :5173") {
		t.Errorf("HTTP line = %q, want it to contain %q", visibleHTTP, ":3000 :5173")
	}
	if strings.Contains(visibleHTTP, "·") {
		t.Errorf("HTTP line = %q, want no separator glyph between the ports", visibleHTTP)
	}
	if got := m.httpLineHeight(); got != 1 {
		t.Errorf("httpLineHeight() = %d, want 1", got)
	}
	// The HTTP line is the only thing above the table here, so the reservation
	// must equal the number of rendered lines preceding it.
	if got := m.httpLineHeight(); got != tableIdx {
		t.Errorf("httpLineHeight()=%d but %d lines precede the table; tableHeight reservation is out of sync:\n%s", got, tableIdx, rendered)
	}
}

// TestChatPicker_HTTPLinePortsIndependentlyClickable checks each port carries
// its own OSC 8 envelope pointing at its own URL.
func TestChatPicker_HTTPLinePortsIndependentlyClickable(t *testing.T) {
	m := seedChatPickerForEndpoints(t)
	m.session = &pb.Session{
		Id: "session-1",
		HttpEndpoints: []*pb.HttpEndpoint{
			{Port: 3000, Url: "http://127.0.0.1:3000"},
			{Port: 5173, Url: "https://127.0.0.1:5173"},
		},
	}
	line := m.httpEndpointLine()
	for _, want := range []string{
		osc8Link("http://127.0.0.1:3000", mutedUnderlineOpen+":3000"+mutedUnderlineClose),
		osc8Link("https://127.0.0.1:5173", mutedUnderlineOpen+":5173"+mutedUnderlineClose),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("httpEndpointLine() = %q\nmissing independent link %q", line, want)
		}
	}
	if got := strings.TrimSpace(visibleRowText(line)); !strings.HasPrefix(got, "HTTP  :3000") {
		t.Errorf("httpEndpointLine() visible text = %q, want it to start with %q", got, "HTTP  :3000")
	}
}

// TestChatPicker_HTTPLinkSurvivesViewRender asserts on the rendered View rather
// than on httpEndpointLine() alone: the line is written raw into a builder that
// also feeds lipgloss-styled blocks, so this is what proves the ports are still
// clickable on screen (the acceptance criterion) and not just well-formed in
// isolation.
func TestChatPicker_HTTPLinkSurvivesViewRender(t *testing.T) {
	m := seedChatPickerForEndpoints(t)
	m.session = &pb.Session{
		Id: "session-1",
		HttpEndpoints: []*pb.HttpEndpoint{
			{Port: 3000, Url: "http://127.0.0.1:3000"},
			{Port: 5173, Url: "https://127.0.0.1:5173"},
		},
	}
	content := m.View().Content
	for _, want := range []string{
		"\x1b]8;;http://127.0.0.1:3000\x1b\\",
		"\x1b]8;;https://127.0.0.1:5173\x1b\\",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("rendered chat-picker view lost the hyperlink %q:\n%q", want, content)
		}
	}
	if !strings.Contains(visibleRowText(content), "HTTP  :3000 :5173") {
		t.Errorf("rendered chat-picker view lost the visible HTTP line:\n%q", content)
	}
}

// TestChatPicker_HTTPLineShrinksTable proves the reserved line comes out of the
// chat table rather than pushing a chat row off-screen.
func TestChatPicker_HTTPLineShrinksTable(t *testing.T) {
	// Many chats in a short terminal so the table is height-constrained and the
	// reservation actually shrinks it (rather than the chat count deciding).
	base := seedChatPickerSized(t, 20, 24)
	baseHeight := base.tableHeight()
	if baseHeight <= 1 {
		t.Fatalf("baseline tableHeight() = %d; test needs the unclamped constrained regime", baseHeight)
	}

	withEP := seedChatPickerSized(t, 20, 24)
	withEP.session = &pb.Session{
		Id:            "session-1",
		HttpEndpoints: []*pb.HttpEndpoint{{Port: 3000, Url: "http://127.0.0.1:3000"}},
	}
	if got := withEP.tableHeight(); got != baseHeight-1 {
		t.Errorf("tableHeight() with one endpoint = %d, want %d (baseline %d minus the reserved HTTP line)", got, baseHeight-1, baseHeight)
	}
}

// TestChatPicker_HTTPLineNonHTTPURLNotClickable ensures a non-HTTP or malformed
// URL still shows its port but never gets wrapped in an OSC 8 envelope.
func TestChatPicker_HTTPLineNonHTTPURLNotClickable(t *testing.T) {
	m := seedChatPickerForEndpoints(t)
	m.session = &pb.Session{
		Id: "session-1",
		HttpEndpoints: []*pb.HttpEndpoint{
			{Port: 3000, Url: "file:///etc/passwd"},
			{Port: 5173, Url: ""},
		},
	}
	line := m.httpEndpointLine()
	if strings.Contains(line, "\x1b]8;;") {
		t.Errorf("httpEndpointLine() = %q, want no OSC 8 envelope for non-HTTP/empty URLs", line)
	}
	if visible := visibleRowText(line); !strings.Contains(visible, ":3000") || !strings.Contains(visible, ":5173") {
		t.Errorf("httpEndpointLine() visible text = %q, want both ports still shown", visible)
	}
}

// manyEndpoints builds n endpoints on consecutive ports, enough label text to
// overflow a narrow terminal.
func manyEndpoints(n int) []*pb.HttpEndpoint {
	eps := make([]*pb.HttpEndpoint, 0, n)
	for i := range n {
		port := uint32(31000 + i)
		eps = append(eps, &pb.HttpEndpoint{Port: port, Url: fmt.Sprintf("http://127.0.0.1:%d", port)})
	}
	return eps
}

// TestChatPicker_HTTPLineNarrowTerminalStaysOneLine is the counterpart to
// TestHomeEndpointRowNarrowTerminal. Nothing upstream caps how many listeners a
// worktree exposes, so a session with many endpoints must still occupy exactly
// the ONE line httpLineHeight reserves — otherwise the wrapped remainder pushes
// the last chat row (or the action bar) off screen. The load-bearing assertion is
// the VISIBLE width: bubbletea writes View().Content verbatim and the terminal is
// what soft-wraps, so a too-wide line costs a second row without ever containing a
// newline. Twelve ":31000"-class labels are ~112 columns, so every width here is a
// real overflow.
func TestChatPicker_HTTPLineNarrowTerminalStaysOneLine(t *testing.T) {
	for _, width := range []int{40, 60, 80} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := seedChatPickerSized(t, 6, 24)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			m = updated.(ChatPickerModel)
			m.session = &pb.Session{Id: "session-1", HttpEndpoints: manyEndpoints(12)}

			line := m.httpEndpointLine()
			if got := ansi.StringWidth(line); got > width {
				t.Errorf("httpEndpointLine() visible width = %d, want <= %d (it would soft-wrap past the reserved line)", got, width)
			}
			if strings.Contains(line, "\n") {
				t.Errorf("httpEndpointLine() = %q, want a single line", line)
			}
			// A truncated line must never leave a hyperlink envelope open: each
			// OSC 8 link contributes exactly two "\x1b]8;;" markers (introducer
			// and terminator), so an odd count means a dangling introducer that
			// would swallow the rest of the screen into one link.
			if n := strings.Count(line, "\x1b]8;;"); n%2 != 0 {
				t.Errorf("httpEndpointLine() has %d OSC 8 markers (odd) — a hyperlink envelope was left open: %q", n, line)
			}
			if got := m.httpLineHeight(); got != 1 {
				t.Errorf("httpLineHeight() = %d, want 1", got)
			}

			// The rendered View must gain exactly one line over the same view
			// with no endpoints — a delta assertion, so it stays valid if
			// anything is ever added above the HTTP line.
			base := seedChatPickerSized(t, 6, 24)
			updatedBase, _ := base.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			base = updatedBase.(ChatPickerModel)
			linesAbove := func(v ChatPickerModel) int {
				for i, ln := range strings.Split(stripANSI(v.View().Content), "\n") {
					if strings.Contains(ln, "CHAT") {
						return i
					}
				}
				t.Fatalf("chat table header not found in:\n%s", v.View().Content)
				return -1
			}
			if got, want := linesAbove(m)-linesAbove(base), m.httpLineHeight(); got != want {
				t.Errorf("HTTP line added %d rendered lines above the table, but httpLineHeight() reserved %d", got, want)
			}
		})
	}
}

// --- BOS-532: stable wrap width for the chat picker's prose blocks ---

// chatPickerWrapColumns builds a column set whose columnsWidth is exactly want.
// columnsWidth adds tableColumnGap per column, so a single column of
// (want - tableColumnGap) yields exactly want.
func chatPickerWrapColumns(want int) []table.Column {
	return []table.Column{{Title: "CHAT", Width: want - tableColumnGap}}
}

// TestChatPickerBlockWrapWidth pins the width rule the chat picker's wrapped
// prose blocks (session warning, rotation history) use — the ChatPicker
// counterpart of TestHomeStatusWrapWidth (BOS-532).
func TestChatPickerBlockWrapWidth(t *testing.T) {
	tests := []struct {
		name    string
		width   int
		columns []table.Column
		want    int
	}{
		{
			name:    "unknown terminal width leaves the blocks unconstrained",
			width:   0,
			columns: chatPickerWrapColumns(96),
			want:    0,
		},
		{
			name:    "negative terminal width leaves the blocks unconstrained",
			width:   -10,
			columns: chatPickerWrapColumns(96),
			want:    0,
		},
		{
			name:    "wide terminal tracks the table's content width",
			width:   200,
			columns: chatPickerWrapColumns(96),
			want:    96,
		},
		{
			name:    "wide terminal with a narrow table floors",
			width:   200,
			columns: chatPickerWrapColumns(38),
			want:    minStatusWrapWidth,
		},
		// The clamp is against the PROSE inset, not the table's (BOS-718):
		// these blocks are rendered inside chatPickerProseBlock, so the room
		// they have is what chatPickerProsePadding leaves. Clamping against the
		// narrower chatPickerBlockPadding would report two columns more than
		// the terminal can actually show and overhang it — the BOS-530/BOS-532
		// defect. TestChatPicker_BlocksNeverOverhangNarrowTerminal checks the
		// rendered consequence; these cases pin the arithmetic.
		{
			name:    "narrow terminal clamps to the room left by the prose padding",
			width:   50,
			columns: chatPickerWrapColumns(116),
			want:    50 - chatPickerProsePadding*2,
		},
		{
			name:    "terminal narrower than the floor clamps below it",
			width:   40,
			columns: chatPickerWrapColumns(116),
			want:    40 - chatPickerProsePadding*2,
		},
		{
			name:    "terminal with no room left after the prose padding",
			width:   chatPickerProsePadding * 2,
			columns: chatPickerWrapColumns(116),
			want:    0,
		},
		{
			name:    "narrowest terminal the padding guard still admits",
			width:   chatPickerProsePadding*2 + 1,
			columns: chatPickerWrapColumns(116),
			want:    1,
		},
		{
			name:    "no columns yet falls back to the floor",
			width:   200,
			columns: nil,
			want:    minStatusWrapWidth,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewChatPickerModel(nil, context.Background(), "session-1", "")
			m.width = tt.width
			m.table.SetColumns(tt.columns)
			if got := m.blockWrapWidth(); got != tt.want {
				t.Fatalf("blockWrapWidth() = %d, want %d", got, tt.want)
			}
		})
	}
}

// The terminal sizes the wrap probes use: a starting size, and a wider one to
// resize into. Both are wider than the fixture table's content width, so the
// wrap width is expected to track the table at either size.
const (
	wrapProbeWidth      = 120
	wrapProbeWiderWidth = 200
	wrapProbeHeight     = 40
)

// seedChatPickerForWrap builds a loaded chat picker for the given session at
// wrapProbeWidth, with the session attached BEFORE the size is delivered so
// the layout is computed the way a real run computes it.
func seedChatPickerForWrap(t *testing.T, session *pb.Session) ChatPickerModel {
	t.Helper()
	return seedChatPickerForWrapAt(t, session, wrapProbeWidth)
}

// seedChatPickerForWrapAt is seedChatPickerForWrap at an explicit terminal
// width, for the narrow-terminal overhang probes.
func seedChatPickerForWrapAt(t *testing.T, session *pb.Session, width int) ChatPickerModel {
	t.Helper()
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.session = session
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{{
			SessionId:      "session-1",
			AgentSessionId: "agent-1",
			// Long enough that the table's content width clears
			// minStatusWrapWidth, so the derived width — not the floor — is
			// the rule under test.
			Title:     strings.Repeat("chat title ", 6),
			CreatedAt: timestamppb.Now(),
		}},
		daemonStatuses: map[string]string{"agent-1": statusWorking},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: wrapProbeHeight})
	return updated.(ChatPickerModel)
}

// resizeChatPickerWider delivers the widening tea.WindowSizeMsg the reflow
// regression turns on and returns the updated model.
func resizeChatPickerWider(m ChatPickerModel) ChatPickerModel {
	updated, _ := m.Update(tea.WindowSizeMsg{Width: wrapProbeWiderWidth, Height: wrapProbeHeight})
	return updated.(ChatPickerModel)
}

// rotationBlockLines returns the rendered rotation-history block's lines: the
// run of non-blank lines from its "Rotation history" header on.
//
// Bounded at the next blank line rather than taken to the end of the content,
// for the same reason warningBlockLines anchors on its own text: relying on
// this being the last thing View writes would silently fold anything appended
// below it into the measurement, hiding a reflow in the width guard.
func rotationBlockLines(t *testing.T, m ChatPickerModel) []string {
	t.Helper()
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "Rotation history") {
			end := i
			for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
				end++
			}
			return lines[i:end]
		}
	}
	t.Fatalf("rotation-history block not found in:\n%s", m.View().Content)
	return nil
}

// warningBlockLines returns the rendered session-warning block's lines: the
// run of non-blank lines around the warning's own opening words, since a
// single blank line closes the block.
//
// Anchored on the warning text rather than on "the first thing View writes",
// even though the warning block is currently first. That framing was fragile:
// if a block were ever rendered above it, this helper would silently measure
// the new block instead and the width and height guards would pass while
// testing nothing. The anchor makes that failure mode impossible.
func warningBlockLines(t *testing.T, m ChatPickerModel) []string {
	t.Helper()
	// The block's opening two words: 15 columns, well inside the narrowest
	// wrap any probe produces (48), so they always land on its first line
	// intact. Shorter is safer here — a longer anchor is more likely to
	// straddle a wrap point, not less.
	anchor := strings.Join(strings.Fields(longSessionWarning)[:2], " ")
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	start := -1
	for i, ln := range lines {
		if strings.Contains(ln, anchor) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("session-warning block (anchor %q) not found in:\n%s", anchor, m.View().Content)
	}
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	end := start
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	return lines[start:end]
}

// maxLineWidth returns the widest rendered line width. lipgloss.Width is used
// rather than len so multi-byte content measures in columns, not bytes; every
// caller strips ANSI first, so styling is already gone by this point.
func maxLineWidth(lines []string) int {
	w := 0
	for _, ln := range lines {
		if lw := lipgloss.Width(ln); lw > w {
			w = lw
		}
	}
	return w
}

// longRotationDetail is long enough to wrap at any plausible terminal width,
// so the block's max line width reports the wrap point rather than whatever
// the content happens to be.
const longRotationDetail = "stale failover-proxy port: the pane baked 52106 into its environment before the account rotated, so every subsequent request authenticated as the previous account and the daemon respawned the chat to clear it (BOS-409)"

const longSessionWarning = "finalize failed (pr_failed): worktree has uncommitted changes that could not be stashed automatically, so the branch was left as-is and the pull request was never opened; resolve the conflict by hand and re-run finalize"

func rotatingSession() *pb.Session {
	return &pb.Session{
		Id:    "session-1",
		Title: "rotating",
		RotationEvents: []*pb.RotationEvent{{
			Id:        "rot-1",
			Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
			Detail:    longRotationDetail,
			CreatedAt: timestamppb.Now(),
		}},
	}
}

func warningSession() *pb.Session {
	return &pb.Session{
		Id:    "session-1",
		Title: "warned",
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Summary:        longSessionWarning,
		},
	}
}

// TestChatPicker_RotationHistoryWidthStableAcrossResize is the regression this
// ticket exists to prevent (BOS-532): the rotation-history block used to wrap
// at m.table.Width(), which means the table's content width before the first
// tea.WindowSizeMsg and the terminal width after it — so the block visibly
// reflowed on the first resize and stayed reflowed.
func TestChatPicker_RotationHistoryWidthStableAcrossResize(t *testing.T) {
	m := seedChatPickerForWrap(t, rotatingSession())
	before := maxLineWidth(rotationBlockLines(t, m))

	m = resizeChatPickerWider(m)
	after := maxLineWidth(rotationBlockLines(t, m))

	if before != after {
		t.Errorf("rotation-history block reflowed on resize: max line width %d before, %d after", before, after)
	}
}

// TestChatPicker_SessionWarningWidthStableAcrossResize is the same regression
// guard for the finalize/repair warning block (BOS-532).
func TestChatPicker_SessionWarningWidthStableAcrossResize(t *testing.T) {
	m := seedChatPickerForWrap(t, warningSession())
	before := maxLineWidth(warningBlockLines(t, m))

	m = resizeChatPickerWider(m)
	after := maxLineWidth(warningBlockLines(t, m))

	if before != after {
		t.Errorf("session-warning block reflowed on resize: max line width %d before, %d after", before, after)
	}
}

// TestChatPicker_RotationHistoryHeightMatchesRenderedBlock pins measure/render
// agreement (BOS-532): rotationHistoryHeight() backs tableHeight()'s row
// reservation, so it must equal the number of lines the block actually
// occupies in View() — before and after a resize. A future edit that changes
// one of the four wrap-width call sites without the others fails here.
func TestChatPicker_RotationHistoryHeightMatchesRenderedBlock(t *testing.T) {
	m := seedChatPickerForWrap(t, rotatingSession())
	if got, want := m.rotationHistoryHeight(), len(rotationBlockLines(t, m)); got != want {
		t.Errorf("before resize: rotationHistoryHeight() = %d, rendered block occupies %d lines", got, want)
	}

	m = resizeChatPickerWider(m)
	if got, want := m.rotationHistoryHeight(), len(rotationBlockLines(t, m)); got != want {
		t.Errorf("after resize: rotationHistoryHeight() = %d, rendered block occupies %d lines", got, want)
	}
}

// TestChatPicker_WarningBlockHeightMatchesRenderedBlock is the same
// measure/render agreement guard for the warning block, whose reservation
// includes the single blank line View renders below it (BOS-532).
//
// Both agreement guards constrain width only. rotationHistoryHeight and View
// each call time.Now() independently, and rotationEventTime renders same-day
// events as "15:04" but older ones as "2006-01-02 15:04", so a local-midnight
// crossing between the two calls can still change the line count. That skew
// predates BOS-532 (BOS-432) and is not what these tests pin.
func TestChatPicker_WarningBlockHeightMatchesRenderedBlock(t *testing.T) {
	m := seedChatPickerForWrap(t, warningSession())
	if got, want := m.warningBlockHeight(), len(warningBlockLines(t, m))+1; got != want {
		t.Errorf("before resize: warningBlockHeight() = %d, rendered block plus its blank line occupies %d lines", got, want)
	}

	m = resizeChatPickerWider(m)
	if got, want := m.warningBlockHeight(), len(warningBlockLines(t, m))+1; got != want {
		t.Errorf("after resize: warningBlockHeight() = %d, rendered block plus its blank line occupies %d lines", got, want)
	}
}

// TestChatPicker_ResizedTableLeavesRoomForItsInset guards the table's half of
// the same overhang rule the prose blocks get from blockWrapWidth. The table's
// viewport pads every visible line out to its width and View then insets it via
// chatPickerContentBlock, so sizing the viewport to the raw terminal width drew
// width+2 columns and soft-wrapped every row — double-counting against the rows
// tableHeight had reserved — until the next chat poll reset the width to
// columnsWidth. Only the resize path needs this: buildTableRows sizes the
// viewport to the content, which is already inset-free.
func TestChatPicker_ResizedTableLeavesRoomForItsInset(t *testing.T) {
	for _, width := range []int{50, 61, wrapProbeWidth, wrapProbeWiderWidth} {
		t.Run(fmt.Sprintf("%d columns", width), func(t *testing.T) {
			m := seedChatPickerForWrapAt(t, rotatingSession(), width)
			if got := m.table.Width() + chatPickerBlockPadding*2; got > width {
				t.Errorf("resized table leaves no room for its inset: viewport %d + inset %d = %d > terminal width %d",
					m.table.Width(), chatPickerBlockPadding*2, got, width)
			}
		})
	}
}

// TestChatPicker_BlocksNeverOverhangNarrowTerminal pins the invariant the
// padding-aware clamp in blockWrapWidth exists to protect: View renders each
// prose block inside chatPickerProseBlock's Padding(0, chatPickerProsePadding)
// *on top of* the wrap width, so the on-screen block is wider than
// blockWrapWidth() reports.
// TestChatPickerBlockWrapWidth only checks that arithmetic; this checks the
// rendered result, so widening the outer padding (or adding a third block with
// its own padding) without teaching blockWrapWidth about it fails here rather
// than silently overhanging the terminal — the defect BOS-530/BOS-532 exist to
// prevent.
//
// "Narrow" is deliberate rather than universal: a terminal only a few columns
// wide cannot show these blocks at all. At m.width <= chatPickerProsePadding*2
// the guard returns 0 and lipgloss treats Width(0) as unconstrained; one column
// above that, lipgloss has no break point to use and emits the longest
// unbreakable segment. Either way the block is wider than the terminal. Both
// are documented degenerate cases (see blockWrapWidth) that Home shares, so
// they are out of this test's scope rather than a gap in it.
func TestChatPicker_BlocksNeverOverhangNarrowTerminal(t *testing.T) {
	blocks := map[string]struct {
		session *pb.Session
		lines   func(*testing.T, ChatPickerModel) []string
	}{
		"rotation history": {session: rotatingSession(), lines: rotationBlockLines},
		"session warning":  {session: warningSession(), lines: warningBlockLines},
	}
	// 50 and 61 sit below the fixture table's content width, so the clamp
	// binds; wrapProbeWidth is the ordinary wide case.
	for _, width := range []int{50, 61, wrapProbeWidth} {
		for name, blk := range blocks {
			t.Run(fmt.Sprintf("%s at %d columns", name, width), func(t *testing.T) {
				m := seedChatPickerForWrapAt(t, blk.session, width)
				if got := maxLineWidth(blk.lines(t, m)); got > width {
					t.Errorf("rendered block overhangs the terminal: max line width %d > terminal width %d", got, width)
				}
			})
		}
	}
}

// --- BOS-718: the chat picker's prose sits in the same column as its chrome ---

// The terminal size the BOS-718 alignment probes render at. Wide and tall
// enough that nothing here wraps, truncates or is pushed off screen, so each
// line's first non-space column is its inset rather than an artefact of a
// reflow.
const (
	alignProbeWidth  = 120
	alignProbeHeight = 40
)

// The fixture text each measured line is located by. Distinct from the other
// fixtures in this file so a probe cannot accidentally anchor on another
// block's content.
const (
	alignWarningText    = "finalize failed (pr_failed): worktree has uncommitted changes"
	alignWaitingReason  = "awaiting checks_passed_ready on acme/my-app#668"
	alignRotationDetail = "refreshed auth in place"

	// The warning block wraps at blockWrapWidth (the table's content width), so
	// the probes anchor on its opening words rather than the whole summary —
	// short enough to always land intact on the block's FIRST line, which is the
	// line whose inset is under test. Same reason warningBlockLines anchors on
	// two words.
	alignWarningAnchor = "finalize failed"
)

// seedChatPickerForAlignment builds a chat picker that draws every prose line
// the view has at once — the finalize/repair warning block, the usage-limited
// hint, the waiting-reason line, the HTTP endpoint line and the
// rotation-history block — alongside the chat table and the action bar, so all
// of their columns can be measured from a single render.
func seedChatPickerForAlignment(t *testing.T) ChatPickerModel {
	t.Helper()
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			{
				SessionId:      "session-1",
				AgentSessionId: "agent-1",
				Title:          "A chat",
				AgentName:      "claude",
				CreatedAt:      timestamppb.Now(),
			},
			{
				SessionId:      "session-1",
				AgentSessionId: "agent-2",
				Title:          "Parked chat",
				AgentName:      "claude",
				CreatedAt:      timestamppb.Now(),
			},
		},
		// agent-1 limited drives limitedProviderLine; agent-2 waiting plus a
		// reason drives waitingReasonLine.
		daemonStatuses:       map[string]string{"agent-1": statusLimited, "agent-2": statusWaiting},
		daemonWaitingReasons: map[string]string{"agent-2": alignWaitingReason},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: alignProbeWidth, Height: alignProbeHeight})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{
		Id:              "session-1",
		Title:           "aligned",
		AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Summary: alignWarningText},
		HttpEndpoints: []*pb.HttpEndpoint{
			{Port: 3000, Url: "http://127.0.0.1:3000"},
			{Port: 5173, Url: "http://127.0.0.1:5173"},
		},
		RotationEvents: []*pb.RotationEvent{{
			Id:        "rot-1",
			Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
			Detail:    alignRotationDetail,
			CreatedAt: timestamppb.Now(),
		}},
	}
	return m
}

// lineColumnOf returns the display column (0-based) at which the first rendered
// line containing needle begins — i.e. the width of its leading run of spaces.
//
// Per-line rather than per-block, because the BOS-718 probes measure lines that
// share one render. OSC 8 envelopes are stripped alongside the SGR escapes so
// the HTTP endpoint line, the one site that emits them, measures in the same
// unit as the lipgloss-rendered lines rather than counting escape bytes as
// columns.
func lineColumnOf(t *testing.T, rendered, needle string) int {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		visible := visibleRowText(line)
		if strings.Contains(visible, needle) {
			return firstNonSpaceColumn(t, visible)
		}
	}
	t.Fatalf("no rendered line contains %q in:\n%s", needle, visibleRowText(rendered))
	return -1
}

// TestChatPicker_StatusLinesAlignWithActionBar is the point of BOS-718: every
// prose line the chat picker draws must start at the same display column as the
// action bar, which is the view's chrome column (styleActionBar, renderBanner
// and styleToast all sit there). Before this, all five sat one column short,
// because they shared the tables' narrower inset while a table's own cell
// padding supplied the column they lacked.
//
// Both columns are measured from one real render at one width, in the shape
// TestToast_AlignsWithTableCursorChevron established, so the test states the
// intent rather than re-asserting a hard-coded indent on both sides. Table
// driven so a regression names the offending line instead of the first one.
func TestChatPicker_StatusLinesAlignWithActionBar(t *testing.T) {
	m := seedChatPickerForAlignment(t)
	rendered := stripANSI(m.View().Content)

	// The action bar's own line, located by its rightmost group so a prose line
	// that happens to mention a key name cannot be mistaken for it.
	want := lineColumnOf(t, rendered, "[esc] back")

	lines := []struct {
		name   string
		needle string
	}{
		{name: "session warning block", needle: alignWarningAnchor},
		{name: "usage-limited hint", needle: "usage-limited"},
		{name: "waiting reason line", needle: alignWaitingReason},
		// The HTTP line hand-rolls its inset as spaces (lipgloss mangles its
		// OSC 8 envelopes), so it is the one site a change to the prose column
		// can leave behind. Anchored on ":3000" rather than "HTTP" so the
		// assertion also proves the envelopes did not displace the text.
		{name: "http endpoint line", needle: ":3000"},
		{name: "rotation history block", needle: alignRotationDetail},
	}
	for _, tt := range lines {
		t.Run(tt.name, func(t *testing.T) {
			if got := lineColumnOf(t, rendered, tt.needle); got != want {
				t.Errorf("%s starts at column %d, action bar at column %d; want them aligned\n%s",
					tt.name, got, want, visibleRowText(rendered))
			}
		})
	}
}

// TestChatPicker_StatusLinesAlignWithTableCaret pins the other half of BOS-718:
// the prose moved, the tables did not. The chat table's ❯ caret is already in
// the chrome column (chatPickerBlockPadding plus the cell's own
// Padding(0, 0, 0, 1)), so asserting the prose equals it proves the fix was a
// prose-side inset rather than a bump of chatPickerBlockPadding, which would
// have pushed the caret to column 3 and moved both together.
func TestChatPicker_StatusLinesAlignWithTableCaret(t *testing.T) {
	m := seedChatPickerForAlignment(t)
	rendered := m.View().Content

	caretCol := renderedColumnOf(t, rendered, cursorChevron)
	for _, needle := range []string{alignWarningAnchor, "usage-limited", alignWaitingReason, ":3000", alignRotationDetail} {
		if got := lineColumnOf(t, stripANSI(rendered), needle); got != caretCol {
			t.Errorf("line %q starts at column %d, table caret at column %d; want them aligned\n%s",
				needle, got, caretCol, visibleRowText(stripANSI(rendered)))
		}
	}
	// And the caret is where it always was. Hard-coded rather than written as
	// chatPickerBlockPadding+1: derived from the constant it is meant to pin,
	// this would stay green for *any* value of chatPickerBlockPadding — bump it
	// to 2 and both the caret and the expectation move to 3 together. 2 is the
	// chrome column, a fact about the TUI rather than about either constant.
	if caretCol != 2 {
		t.Errorf("table caret at column %d, want 2 (chatPickerBlockPadding=%d plus the cell's own left pad of 1); the table moved",
			caretCol, chatPickerBlockPadding)
	}
}

// TestChatPicker_HTTPLineInsetMatchesTheProseConstant pins the hand-rolled
// inset directly, not just via the rendered view: httpEndpointLine builds its
// leading spaces itself, so a future edit could restore the table constant
// there while every lipgloss-rendered line stayed correct.
func TestChatPicker_HTTPLineInsetMatchesTheProseConstant(t *testing.T) {
	m := seedChatPickerForAlignment(t)
	line := visibleRowText(m.httpEndpointLine())
	got := len(line) - len(strings.TrimLeft(line, " "))
	if got != chatPickerProsePadding {
		t.Errorf("httpEndpointLine() inset = %d spaces, want chatPickerProsePadding (%d): %q",
			got, chatPickerProsePadding, line)
	}
	// The prose helper and the hand-rolled inset must agree, which is the
	// property that actually keeps the line in step.
	if want := firstNonSpaceColumn(t, chatPickerProseBlock("x")); got != want {
		t.Errorf("httpEndpointLine() inset = %d, chatPickerProseBlock inset = %d; the two have drifted", got, want)
	}
}

// TestChatPickerProseInsetIsWiderThanTheTableInset states the invariant the
// whole change rests on, so a future edit that collapses the two constants back
// into one fails here with the reason rather than only as a column mismatch in
// the render probes: the tables sit one column narrower because each table cell
// carries its own left pad, and the prose has none to borrow.
func TestChatPickerProseInsetIsWiderThanTheTableInset(t *testing.T) {
	if chatPickerBlockPadding != 1 {
		t.Errorf("chatPickerBlockPadding = %d, want 1; moving it moves the chat table's caret", chatPickerBlockPadding)
	}
	// No assertion that chatPickerProsePadding == statusLinePadding: that one
	// compares the constant against its own definition (`= statusLinePadding`),
	// so no edit preserving the declaration could make it fire. The two
	// assertions here compare against values declared elsewhere — a literal, and
	// the other constant — so each does fail when what it pins moves.
	if chatPickerProsePadding != chatPickerBlockPadding+1 {
		t.Errorf("chatPickerProsePadding = %d, want chatPickerBlockPadding+1 (%d): the table's cell pad supplies exactly one column",
			chatPickerProsePadding, chatPickerBlockPadding+1)
	}
}

// TestChatPicker_SingleLineHintsNeverOverhangTheTerminal pins the guard
// fitProseLine adds, for the reservation invariant tableHeight's doc states:
// limitedLineHeight and waitingLineHeight each claim a fixed row count, so a
// line wider than the terminal soft-wraps onto a row nobody reserved.
//
// Measured as a width through the real render path — chatPickerProseBlock's
// inset included, since the inset is what the reservation forgets. Deliberately
// NOT asserted as lipgloss.Height: chatPickerProseBlock sets no Width, so
// lipgloss never inserts a newline and Height reports 1 for an overhanging line
// too. The soft wrap happens in the terminal, where only the column count can
// see it.
//
// Both callers are exercised: dropping fitProseLine from either one alone must
// fail here.
func TestChatPicker_SingleLineHintsNeverOverhangTheTerminal(t *testing.T) {
	// Shaped like the real things: waitingHintLine's reason is a daemon-supplied
	// "owner/repo#N" with no upstream bound on the repo name, and the limited
	// hint grows with the number of distinct agent names in the session.
	//
	// The limited hint (159 columns) overhangs at all four widths below; the
	// waiting reason (104) overhangs at 40, 61 and 80 but fits at 120, which is
	// the control — the guard must leave a line that already fits alone.
	const longReason = "checks_passed_ready on some-very-long-organisation-name/an-equally-long-repository-name#123456"
	longAgents := []string{
		"claude-opus-with-a-long-account-label", "codex-gpt-with-a-long-account-label",
		"opencode-with-a-long-account-label", "another-agent-with-a-long-label",
	}

	hints := []struct {
		name string
		// seed returns a model showing only this hint, and the line it draws.
		seed func(width int) (ChatPickerModel, string)
	}{
		{
			name: "waiting reason",
			seed: func(width int) (ChatPickerModel, string) {
				m := seedChatPickerHint(width, []*pb.ClaudeChat{{
					SessionId: "session-1", AgentSessionId: "agent-1",
					Title: "Parked chat", AgentName: "claude", CreatedAt: timestamppb.Now(),
				}}, map[string]string{"agent-1": statusWaiting},
					map[string]string{"agent-1": longReason})
				return m, m.waitingReasonLine()
			},
		},
		{
			name: "usage-limited hint",
			seed: func(width int) (ChatPickerModel, string) {
				chats := make([]*pb.ClaudeChat, 0, len(longAgents))
				statuses := make(map[string]string, len(longAgents))
				for i, agent := range longAgents {
					id := fmt.Sprintf("agent-%d", i)
					chats = append(chats, &pb.ClaudeChat{
						SessionId: "session-1", AgentSessionId: id,
						Title: "Limited chat", AgentName: agent, CreatedAt: timestamppb.Now(),
					})
					statuses[id] = statusLimited
				}
				m := seedChatPickerHint(width, chats, statuses, nil)
				return m, m.limitedProviderLine()
			},
		},
	}

	for _, hint := range hints {
		for _, width := range []int{40, 61, 80, 120} {
			t.Run(fmt.Sprintf("%s/%d columns", hint.name, width), func(t *testing.T) {
				m, line := hint.seed(width)
				if line == "" {
					t.Fatalf("%s is empty; the fixture no longer drives it", hint.name)
				}
				rendered := visibleRowText(chatPickerProseBlock(styleStatusInfo.Render(line)))
				if got := ansi.StringWidth(rendered); got > width {
					t.Errorf("rendered %s is %d columns wide in a %d-column terminal: %q",
						hint.name, got, width, rendered)
				}
				// The reservation this width protects, so a failure names it.
				if got := m.limitedLineHeight() + m.waitingLineHeight(); got != 2 {
					t.Errorf("reserved rows = %d, want 2 (one hint on screen)", got)
				}
			})
		}
	}
}

// seedChatPickerHint builds a sized chat picker showing one status hint.
func seedChatPickerHint(width int, chats []*pb.ClaudeChat, statuses, reasons map[string]string) ChatPickerModel {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats:                chats,
		daemonStatuses:       statuses,
		daemonWaitingReasons: reasons,
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	return updated.(ChatPickerModel)
}
