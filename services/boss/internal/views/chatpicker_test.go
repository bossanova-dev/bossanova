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
func (s *chatPickerStub) GetSession(context.Context, string) (*pb.Session, error) {
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
func (s *chatPickerStub) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
	panic("unused")
}
func (s *chatPickerStub) ListAccounts(context.Context, string) ([]*pb.Account, error) {
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
	if !strings.Contains(rendered, warn) {
		t.Errorf("view-session screen missing full warning %q in:\n%s", warn, rendered)
	}
}

func TestChatPicker_NoWarningBlockForCleanSession(t *testing.T) {
	rendered := renderChatPickerWith(t, &pb.Session{Id: "session-1", Title: "clean"})
	if strings.Contains(rendered, "finalize failed") || strings.Contains(rendered, "repair failed") {
		t.Errorf("view-session screen rendered a warning block for a clean session:\n%s", rendered)
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
