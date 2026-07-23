package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubSessionSettingsClient implements client.BossClient for SessionSettingsModel
// tests. GetSession and UpdateSession are functional and capture their inputs;
// every other method panics so an unexpected call fails loudly.
type stubSessionSettingsClient struct {
	session   *pb.Session
	getErr    error
	updated   *pb.Session
	updateErr error
	updateReq *pb.UpdateSessionRequest // captures the last UpdateSession request
}

var _ client.BossClient = (*stubSessionSettingsClient)(nil)

func (s *stubSessionSettingsClient) GetSession(context.Context, string) (*pb.Session, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.session, nil
}

func (s *stubSessionSettingsClient) UpdateSession(_ context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	s.updateReq = req
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return s.updated, nil
}

// Unused interface methods — panic if called unexpectedly.
func (s *stubSessionSettingsClient) Ping(context.Context) error { panic("unused") }
func (s *stubSessionSettingsClient) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ValidateRepoPath(context.Context, string) (*pb.ValidateRepoPathResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RegisterRepo(context.Context, *pb.RegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) CloneAndRegisterRepo(context.Context, *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RemoveRepo(context.Context, string) error { panic("unused") }
func (s *stubSessionSettingsClient) UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListTrackerIssues(context.Context, string, string, string) ([]*pb.TrackerIssue, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) CreateSession(context.Context, *pb.CreateSessionRequest) (client.CreateSessionStream, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListSessions(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) AttachSession(context.Context, string) (client.AttachStream, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) StopSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) PauseSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ResumeSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RetrySession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) CloseSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) MergeSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RemoveSession(context.Context, string) error { panic("unused") }
func (s *stubSessionSettingsClient) LinkSessionPR(context.Context, string, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ArchiveSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ResurrectSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RecordChat(context.Context, string, string, string, string, bool) (*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) UpdateChatTitle(context.Context, string, string) error {
	panic("unused")
}
func (s *stubSessionSettingsClient) DeleteChat(context.Context, string) error { panic("unused") }
func (s *stubSessionSettingsClient) WakeChat(context.Context, string, string, bool) (*pb.WakeChatResponse, error) {
	panic("unused")
}

func (s *stubSessionSettingsClient) DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	panic("unused")
}
func (s *stubSessionSettingsClient) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) SendChatMessage(context.Context, *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) NotifyAuthChange(context.Context, string) error { return nil }
func (s *stubSessionSettingsClient) CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) GetCronJob(context.Context, string) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListCronJobs(context.Context, string) ([]*pb.CronJob, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) DeleteCronJob(context.Context, string) error { panic("unused") }
func (s *stubSessionSettingsClient) CreateGithubCallback(context.Context, *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListGithubCallbacks(context.Context, *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) DeleteGithubCallback(context.Context, string, string) error {
	panic("unused")
}
func (s *stubSessionSettingsClient) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListAccounts(context.Context, string, bool) ([]*pb.Account, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) AddAccount(context.Context, *pb.AddAccountRequest) (*pb.Account, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) UpdateAccount(context.Context, *pb.UpdateAccountRequest) (*pb.Account, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RemoveAccount(context.Context, string) error { panic("unused") }
func (s *stubSessionSettingsClient) TestAccount(context.Context, string) (*pb.TestAccountResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) RepairDoctor(context.Context) (*pb.RepairDoctorResponse, error) {
	panic("unused")
}

func (s *stubSessionSettingsClient) StartRepairWorkflow(context.Context) (*pb.StartRepairWorkflowResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListCheckSnapshots(context.Context, string, int32) (*pb.ListCheckSnapshotsResponse, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListAgents(context.Context) ([]client.AgentInfo, error) {
	panic("unused")
}
func (s *stubSessionSettingsClient) ListPlugins(context.Context) ([]*pb.InstalledPlugin, error) {
	panic("unused")
}

// newSessionSettingsModel builds a model and loads the given session via the
// real loaded-message path so tests start from a realistic post-load state.
func newSessionSettingsModel(t *testing.T, stub *stubSessionSettingsClient) SessionSettingsModel {
	t.Helper()
	m := NewSessionSettingsModel(stub, context.Background(), "sess-1")
	loaded, _ := m.Update(m.Init()())
	return loaded.(SessionSettingsModel)
}

func TestSessionSettingsInit(t *testing.T) {
	t.Run("success delivers session", func(t *testing.T) {
		stub := &stubSessionSettingsClient{session: &pb.Session{Title: "hello"}}
		m := NewSessionSettingsModel(stub, context.Background(), "sess-1")
		msg, ok := m.Init()().(sessionSettingsLoadedMsg)
		if !ok {
			t.Fatalf("Init did not return sessionSettingsLoadedMsg")
		}
		if msg.err != nil {
			t.Fatalf("unexpected err: %v", msg.err)
		}
		if msg.session == nil || msg.session.Title != "hello" {
			t.Fatalf("session not propagated: %+v", msg.session)
		}
	})

	t.Run("error is propagated", func(t *testing.T) {
		wantErr := errors.New("boom")
		stub := &stubSessionSettingsClient{getErr: wantErr}
		m := NewSessionSettingsModel(stub, context.Background(), "sess-1")
		msg, ok := m.Init()().(sessionSettingsLoadedMsg)
		if !ok {
			t.Fatalf("Init did not return sessionSettingsLoadedMsg")
		}
		if !errors.Is(msg.err, wantErr) {
			t.Fatalf("err = %v, want %v", msg.err, wantErr)
		}
		if msg.session != nil {
			t.Fatalf("expected nil session on error, got %+v", msg.session)
		}
	})
}

func TestSessionSettingsLoadedMsg(t *testing.T) {
	t.Run("success seeds name input", func(t *testing.T) {
		m := NewSessionSettingsModel(&stubSessionSettingsClient{}, context.Background(), "sess-1")
		updated, _ := m.Update(sessionSettingsLoadedMsg{session: &pb.Session{Title: "my title"}})
		got := updated.(SessionSettingsModel)
		if got.session == nil || got.session.Title != "my title" {
			t.Fatalf("session not stored: %+v", got.session)
		}
		if got.nameInput.Value() != "my title" {
			t.Fatalf("name input = %q, want %q", got.nameInput.Value(), "my title")
		}
	})

	t.Run("error sets err and leaves session nil", func(t *testing.T) {
		wantErr := errors.New("load failed")
		m := NewSessionSettingsModel(&stubSessionSettingsClient{}, context.Background(), "sess-1")
		updated, _ := m.Update(sessionSettingsLoadedMsg{err: wantErr})
		got := updated.(SessionSettingsModel)
		if !errors.Is(got.err, wantErr) {
			t.Fatalf("err = %v, want %v", got.err, wantErr)
		}
		if got.session != nil {
			t.Fatalf("expected nil session, got %+v", got.session)
		}
	})
}

func TestSessionSettingsSavedMsg(t *testing.T) {
	t.Run("success replaces session and clears err", func(t *testing.T) {
		m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "old"}})
		m.err = errors.New("stale")
		updated, _ := m.Update(sessionSettingsSavedMsg{session: &pb.Session{Title: "new"}})
		got := updated.(SessionSettingsModel)
		if got.session == nil || got.session.Title != "new" {
			t.Fatalf("session not replaced: %+v", got.session)
		}
		if got.err != nil {
			t.Fatalf("err not cleared: %v", got.err)
		}
	})

	t.Run("error is retained", func(t *testing.T) {
		wantErr := errors.New("save failed")
		m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "old"}})
		updated, _ := m.Update(sessionSettingsSavedMsg{err: wantErr})
		got := updated.(SessionSettingsModel)
		if !errors.Is(got.err, wantErr) {
			t.Fatalf("err = %v, want %v", got.err, wantErr)
		}
		if got.session == nil || got.session.Title != "old" {
			t.Fatalf("session should be unchanged on error: %+v", got.session)
		}
	})
}

func TestSessionSettingsWindowSize(t *testing.T) {
	m := NewSessionSettingsModel(&stubSessionSettingsClient{}, context.Background(), "sess-1")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if got := updated.(SessionSettingsModel).width; got != 120 {
		t.Fatalf("width = %d, want 120", got)
	}
}

func TestSessionSettingsNavigationClampsCursor(t *testing.T) {
	m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "t"}})
	if m.cursor != 0 {
		t.Fatalf("precondition: cursor = %d, want 0", m.cursor)
	}

	// Only one row exists, so down must not advance past row 0.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := updated.(SessionSettingsModel).cursor; got != 0 {
		t.Fatalf("after down: cursor = %d, want 0", got)
	}

	// Up at the top row must not move below 0.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := updated.(SessionSettingsModel).cursor; got != 0 {
		t.Fatalf("after up: cursor = %d, want 0", got)
	}
}

func TestSessionSettingsEscCancels(t *testing.T) {
	m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "t"}})
	if m.Cancelled() {
		t.Fatal("precondition: should not be cancelled")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !updated.(SessionSettingsModel).Cancelled() {
		t.Fatal("esc should set cancelled")
	}
}

func TestSessionSettingsActivateRow(t *testing.T) {
	t.Run("enter on name row enters editing", func(t *testing.T) {
		m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "t"}})
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(SessionSettingsModel)
		if got.editingField != sessionSettingsRowName {
			t.Fatalf("editingField = %d, want %d", got.editingField, sessionSettingsRowName)
		}
		if cmd == nil {
			t.Fatal("expected a focus command")
		}
	})

	t.Run("enter without a loaded session is a no-op", func(t *testing.T) {
		m := NewSessionSettingsModel(&stubSessionSettingsClient{}, context.Background(), "sess-1")
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(SessionSettingsModel)
		if got.editingField != -1 {
			t.Fatalf("editingField = %d, want -1 (no session loaded)", got.editingField)
		}
		if cmd != nil {
			t.Fatal("expected no command without a session")
		}
	})
}

func TestSessionSettingsCommitEdit(t *testing.T) {
	t.Run("empty name is rejected without saving", func(t *testing.T) {
		stub := &stubSessionSettingsClient{session: &pb.Session{Title: "t"}}
		m := newSessionSettingsModel(t, stub)
		// Enter editing, clear the input, then press enter.
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(SessionSettingsModel)
		m.nameInput.SetValue("   ")
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(SessionSettingsModel)
		if got.err == nil || !strings.Contains(got.err.Error(), "name cannot be empty") {
			t.Fatalf("err = %v, want empty-name error", got.err)
		}
		if cmd != nil {
			t.Fatal("expected no save command for empty name")
		}
		if got.editingField != sessionSettingsRowName {
			t.Fatalf("should remain in editing on error, editingField = %d", got.editingField)
		}
		if stub.updateReq != nil {
			t.Fatalf("UpdateSession should not have been called: %+v", stub.updateReq)
		}
	})

	t.Run("valid name triggers save with trimmed title", func(t *testing.T) {
		stub := &stubSessionSettingsClient{
			session: &pb.Session{Title: "old"},
			updated: &pb.Session{Title: "renamed"},
		}
		m := newSessionSettingsModel(t, stub)
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(SessionSettingsModel)
		m.nameInput.SetValue("  renamed  ")
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(SessionSettingsModel)
		if got.editingField != -1 {
			t.Fatalf("editingField = %d, want -1 after commit", got.editingField)
		}
		if got.err != nil {
			t.Fatalf("unexpected err: %v", got.err)
		}
		if cmd == nil {
			t.Fatal("expected a save command")
		}
		// Drive the save command and confirm the request was well-formed.
		saved, ok := cmd().(sessionSettingsSavedMsg)
		if !ok {
			t.Fatal("save command did not return sessionSettingsSavedMsg")
		}
		if saved.err != nil {
			t.Fatalf("save returned err: %v", saved.err)
		}
		if stub.updateReq == nil {
			t.Fatal("UpdateSession was not called")
		}
		if stub.updateReq.Id != "sess-1" {
			t.Fatalf("update Id = %q, want sess-1", stub.updateReq.Id)
		}
		if stub.updateReq.Title == nil || *stub.updateReq.Title != "renamed" {
			t.Fatalf("update Title = %v, want \"renamed\"", stub.updateReq.Title)
		}
	})
}

func TestSessionSettingsCancelEdit(t *testing.T) {
	stub := &stubSessionSettingsClient{session: &pb.Session{Title: "original"}}
	m := newSessionSettingsModel(t, stub)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(SessionSettingsModel)
	m.nameInput.SetValue("scratch edit")

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := updated.(SessionSettingsModel)
	if got.editingField != -1 {
		t.Fatalf("editingField = %d, want -1 after cancel", got.editingField)
	}
	if got.nameInput.Value() != "original" {
		t.Fatalf("name input = %q, want reset to %q", got.nameInput.Value(), "original")
	}
}

func TestSessionSettingsEditingForwardsKeysToInput(t *testing.T) {
	m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: ""}})
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(SessionSettingsModel)
	if m.editingField != sessionSettingsRowName {
		t.Fatalf("precondition: not in editing mode (editingField=%d)", m.editingField)
	}
	updated, _ = m.Update(keyPress('z'))
	if got := updated.(SessionSettingsModel).nameInput.Value(); got != "z" {
		t.Fatalf("name input = %q, want the typed %q", got, "z")
	}
}

func TestSessionSettingsPasteMsgForwardedToInput(t *testing.T) {
	// Regression test for BOS-78: pasting text into the session-rename field was
	// silently dropped because Update only routed tea.KeyMsg into updateEditing,
	// not tea.PasteMsg.
	m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: ""}})

	// Enter editing mode on the name row.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(SessionSettingsModel)
	if m.editingField != sessionSettingsRowName {
		t.Fatalf("precondition: not in editing mode (editingField=%d)", m.editingField)
	}

	// Paste multi-character text. Before the fix this was dropped entirely.
	updated, _ = m.Update(tea.PasteMsg{Content: "pasted text"})
	got := updated.(SessionSettingsModel).nameInput.Value()
	if !strings.Contains(got, "pasted text") {
		t.Fatalf("nameInput.Value() = %q, want it to contain %q (PasteMsg was dropped)", got, "pasted text")
	}
}

func TestSessionSettingsView(t *testing.T) {
	t.Run("loading state before session arrives", func(t *testing.T) {
		m := NewSessionSettingsModel(&stubSessionSettingsClient{}, context.Background(), "sess-1")
		if got := m.View().Content; !strings.Contains(got, "Loading session") {
			t.Fatalf("view = %q, want loading message", got)
		}
	})

	t.Run("load error before session", func(t *testing.T) {
		m := NewSessionSettingsModel(&stubSessionSettingsClient{}, context.Background(), "sess-1")
		updated, _ := m.Update(sessionSettingsLoadedMsg{err: errors.New("nope")})
		got := updated.(SessionSettingsModel).View().Content
		if !strings.Contains(got, "nope") || !strings.Contains(got, "[esc] back") {
			t.Fatalf("error view = %q, want error text and back action", got)
		}
	})

	t.Run("loaded row shows title and edit action", func(t *testing.T) {
		m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "Acme"}})
		got := m.View().Content
		if !strings.Contains(got, "Acme") {
			t.Fatalf("view = %q, want session title", got)
		}
		if !strings.Contains(got, "[enter/space] edit") {
			t.Fatalf("view = %q, want edit action bar", got)
		}
	})

	t.Run("editing mode shows save/cancel actions", func(t *testing.T) {
		m := newSessionSettingsModel(t, &stubSessionSettingsClient{session: &pb.Session{Title: "Acme"}})
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(SessionSettingsModel).View().Content
		if !strings.Contains(got, "[enter] save") || !strings.Contains(got, "[esc] cancel") {
			t.Fatalf("editing view = %q, want save/cancel actions", got)
		}
	})
}

func (s *stubSessionSettingsClient) SwitchSessionAccount(context.Context, *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	panic("unused")
}
