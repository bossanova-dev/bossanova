package views

import (
	"context"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubClient implements client.BossClient for testing NewSessionModel.
// Only the methods used by the wizard are implemented; the rest panic.
type stubClient struct {
	repos            []*pb.Repo
	reposErr         error
	created          *pb.Session
	createErr        error
	createReq        *pb.CreateSessionRequest // captures the last CreateSession request
	prs              []*pb.PRSummary
	prsErr           error
	trackerIssues    []*pb.TrackerIssue
	trackerIssuesErr error
}

func (s *stubClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	return s.repos, s.reposErr
}

func (s *stubClient) CreateSession(_ context.Context, req *pb.CreateSessionRequest) (client.CreateSessionStream, error) {
	s.createReq = req
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &stubCreateStream{session: s.created}, nil
}

func (s *stubClient) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	return s.prs, s.prsErr
}

func (s *stubClient) ListTrackerIssues(context.Context, string) ([]*pb.TrackerIssue, error) {
	return s.trackerIssues, s.trackerIssuesErr
}

// stubCreateStream implements client.CreateSessionStream for testing.
// It yields a single SessionCreated message with the provided session.
type stubCreateStream struct {
	session  *pb.Session
	received bool
}

func (s *stubCreateStream) Receive() bool {
	if s.received {
		return false
	}
	s.received = true
	return true
}

func (s *stubCreateStream) Msg() *pb.CreateSessionResponse {
	return &pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{
				Session: s.session,
			},
		},
	}
}

func (s *stubCreateStream) Err() error {
	return nil
}

func (s *stubCreateStream) Close() error {
	return nil
}

// Unused interface methods — panic if called unexpectedly.
func (s *stubClient) Ping(context.Context) error { panic("unused") }
func (s *stubClient) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	panic("unused")
}
func (s *stubClient) ValidateRepoPath(context.Context, string) (*pb.ValidateRepoPathResponse, error) {
	panic("unused")
}
func (s *stubClient) RegisterRepo(context.Context, *pb.RegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubClient) CloneAndRegisterRepo(context.Context, *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubClient) RemoveRepo(context.Context, string) error { panic("unused") }
func (s *stubClient) UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubClient) GetSession(context.Context, string) (*pb.Session, error) { panic("unused") }
func (s *stubClient) ListSessions(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
	panic("unused")
}
func (s *stubClient) AttachSession(context.Context, string) (client.AttachStream, error) {
	panic("unused")
}
func (s *stubClient) StopSession(context.Context, string) (*pb.Session, error)   { panic("unused") }
func (s *stubClient) PauseSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *stubClient) ResumeSession(context.Context, string) (*pb.Session, error) { panic("unused") }
func (s *stubClient) RetrySession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *stubClient) CloseSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *stubClient) RemoveSession(context.Context, string) error                { panic("unused") }
func (s *stubClient) UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error) {
	panic("unused")
}
func (s *stubClient) ArchiveSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubClient) ResurrectSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubClient) EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error) {
	panic("unused")
}
func (s *stubClient) RecordChat(context.Context, string, string, string) (*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubClient) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubClient) UpdateChatTitle(context.Context, string, string) error { panic("unused") }
func (s *stubClient) DeleteChat(context.Context, string) error              { panic("unused") }
func (s *stubClient) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	panic("unused")
}
func (s *stubClient) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	panic("unused")
}
func (s *stubClient) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	panic("unused")
}
func (s *stubClient) StartAutopilot(context.Context, *pb.StartAutopilotRequest) (*pb.AutopilotWorkflow, error) {
	panic("unused")
}
func (s *stubClient) PauseAutopilot(context.Context, string) (*pb.AutopilotWorkflow, error) {
	panic("unused")
}
func (s *stubClient) ResumeAutopilot(context.Context, string) (*pb.AutopilotWorkflow, error) {
	panic("unused")
}
func (s *stubClient) CancelAutopilot(context.Context, string) (*pb.AutopilotWorkflow, error) {
	panic("unused")
}
func (s *stubClient) GetAutopilotStatus(context.Context, string) (*pb.AutopilotWorkflow, error) {
	panic("unused")
}
func (s *stubClient) ListAutopilotWorkflows(context.Context, *pb.ListAutopilotWorkflowsRequest) ([]*pb.AutopilotWorkflow, error) {
	panic("unused")
}
func (s *stubClient) StreamAutopilotOutput(context.Context, string) (client.AutopilotOutputStream, error) {
	panic("unused")
}

func (s *stubClient) EnsureTmuxSession(context.Context, string, string, string) (string, string, error) {
	panic("unused")
}

// --- Helpers ---

func twoRepos() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main"},
		{Id: "repo-2", DisplayName: "beta", LocalPath: "/path/beta", DefaultBaseBranch: "main"},
	}
}

func oneRepo() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main"},
	}
}

// keyPress creates a KeyPressMsg for a printable rune (e.g. "j", "q").
func keyPress(ch rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ch, Text: string(ch)}
}

// specialKeyPress creates a KeyPressMsg for a special key (e.g. tea.KeyEnter).
func specialKeyPress(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

// sendKey simulates a key press through the model's Update, mimicking how
// bubbletea calls Update with a value receiver and stores the returned copy.
func sendKey(t *testing.T, m NewSessionModel, ch rune) NewSessionModel {
	t.Helper()
	updated, _ := m.Update(keyPress(ch))
	return assertValueType(t, updated)
}

func sendSpecialKey(t *testing.T, m NewSessionModel, code rune) NewSessionModel {
	t.Helper()
	updated, _ := m.Update(specialKeyPress(code))
	return assertValueType(t, updated)
}

// sendMsg sends an arbitrary tea.Msg through Update.
func sendMsg(t *testing.T, m NewSessionModel, msg tea.Msg) NewSessionModel {
	t.Helper()
	updated, _ := m.Update(msg)
	return assertValueType(t, updated)
}

// assertValueType asserts Update returned NewSessionModel (not *NewSessionModel).
func assertValueType(t *testing.T, model tea.Model) NewSessionModel {
	t.Helper()
	m, ok := model.(NewSessionModel)
	if !ok {
		t.Fatalf("Update returned %T, want views.NewSessionModel (value type)", model)
	}
	return m
}

// --- Tests ---

func TestNewSession_SingleRepoAutoSelects(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())

	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect (%d)", m.phase, newSessionPhaseTypeSelect)
	}
	if m.selectedRepoID != "repo-1" {
		t.Fatalf("selectedRepoID = %q, want %q", m.selectedRepoID, "repo-1")
	}
}

func TestNewSession_MultipleReposShowTable(t *testing.T) {
	sc := &stubClient{repos: twoRepos()}
	m := NewNewSessionModel(sc, context.Background())

	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	if m.phase != newSessionPhaseRepoSelect {
		t.Fatalf("phase = %d, want newSessionPhaseRepoSelect (%d)", m.phase, newSessionPhaseRepoSelect)
	}
}

func TestNewSession_TableSelectTransitionsToTypeSelect(t *testing.T) {
	sc := &stubClient{repos: twoRepos()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Move to second repo and press enter.
	m = sendKey(t, m, 'j')                 // down
	m = sendSpecialKey(t, m, tea.KeyEnter) // select

	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect (%d)", m.phase, newSessionPhaseTypeSelect)
	}
	if m.selectedRepoID != "repo-2" {
		t.Fatalf("selectedRepoID = %q, want %q", m.selectedRepoID, "repo-2")
	}
}

func TestNewSession_FormDataSurvivesCopies(t *testing.T) {
	// Regression test for the stale-pointer bug: huh form Value() pointers
	// must target heap-allocated formData, not stack fields that get
	// invalidated by value-receiver copies.
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Advance to form phase (type select → form).
	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()

	if m.fd == nil {
		t.Fatal("formData is nil after buildForm")
	}

	// Simulate what the huh form does: write to fd fields via the stable pointer.
	m.fd.title = "my feature"

	// Simulate multiple value-receiver copies (as bubbletea does on each Update).
	copy1 := m
	copy2 := copy1

	if copy2.fd.title != "my feature" {
		t.Fatalf("fd.title = %q after copies, want %q", copy2.fd.title, "my feature")
	}

	// Mutate via one copy — should be visible in all (shared heap pointer).
	copy1.fd.title = "updated title"
	if copy2.fd.title != "updated title" {
		t.Fatalf("fd.title = %q in copy2, want %q — formData is not shared", copy2.fd.title, "updated title")
	}
}

func TestNewSession_HandleFormCompletedReturnsValueType(t *testing.T) {
	// Regression test: handleFormCompleted has a pointer receiver and must
	// return *m (dereferenced), not m (which would be *NewSessionModel).
	sc := &stubClient{
		repos:   oneRepo(),
		created: &pb.Session{Id: "sess-1", Title: "test", BranchName: "boss/test"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()
	m.fd.title = "test title"

	result, _ := m.handleFormCompleted()
	assertValueType(t, result)
}

func TestNewSession_CreateSessionReceivesTitle(t *testing.T) {
	sc := &stubClient{
		repos:   oneRepo(),
		created: &pb.Session{Id: "sess-1", Title: "my feature", BranchName: "boss/my-feature"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()
	m.fd.title = "my feature"

	cmd := m.startCreating()
	if cmd == nil {
		t.Fatal("startCreating returned nil cmd")
	}
	msg := cmd()
	streamMsg := msg.(createSessionStreamMsg)
	if streamMsg.err != nil {
		t.Fatalf("unexpected error: %v", streamMsg.err)
	}

	if sc.createReq == nil {
		t.Fatal("CreateSession was not called")
	}
	if sc.createReq.Title != "my feature" {
		t.Fatalf("CreateSession title = %q, want %q", sc.createReq.Title, "my feature")
	}
}

func TestNewSession_FormDataSharedAcrossUpdateCycles(t *testing.T) {
	// End-to-end regression: simulate the full bubbletea Update cycle where
	// the model is copied on every call. Verify that fd written by the form
	// in one cycle is readable in a later cycle's handleFormCompleted.
	sc := &stubClient{
		repos:   twoRepos(),
		created: &pb.Session{Id: "sess-1", Title: "test", BranchName: "boss/test"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Select first repo (goes to type select now).
	m = sendSpecialKey(t, m, tea.KeyEnter)
	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect", m.phase)
	}

	// Advance to form phase.
	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()

	if m.fd == nil {
		t.Fatal("fd is nil after buildForm")
	}

	// Simulate form writing values (as huh would via Value pointers).
	m.fd.title = "works across copies"

	// Simulate several Update cycles (each creates a new value-receiver copy).
	for i := 0; i < 5; i++ {
		m = sendMsg(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	}

	// fd must still be accessible and correct.
	if m.fd.title != "works across copies" {
		t.Fatalf("fd.title = %q after 5 Update cycles, want %q", m.fd.title, "works across copies")
	}

	// handleFormCompleted should read the correct title.
	result, cmd := m.handleFormCompleted()
	rm := assertValueType(t, result)
	if rm.phase != newSessionPhaseCreating {
		t.Fatalf("phase = %d, want newSessionPhaseCreating (%d)", rm.phase, newSessionPhaseCreating)
	}
	if cmd == nil {
		t.Fatal("handleFormCompleted returned nil cmd")
	}

	// Execute the command to trigger CreateSession.
	cmd()
	if sc.createReq.Title != "works across copies" {
		t.Fatalf("CreateSession title = %q, want %q", sc.createReq.Title, "works across copies")
	}
}

func TestNewSession_FormPhase_EscGoesBackToTypeSelect(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Advance to form phase.
	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()
	m.fd.title = "my feature"

	// Press esc — should go back to typeSelect, not cancel.
	m = sendSpecialKey(t, m, tea.KeyEscape)

	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect (%d)", m.phase, newSessionPhaseTypeSelect)
	}
	if m.Cancelled() {
		t.Error("expected not cancelled — should return to type select, not exit")
	}
	if m.form != nil {
		t.Error("expected form to be nil after going back")
	}
	if m.fd != nil {
		t.Error("expected fd to be nil after going back")
	}
}

func TestNewSession_ConfirmOverwrite_EscGoesBackToForm(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Set up form phase with a title, then simulate overwrite confirmation.
	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()
	m.fd.title = "my feature"
	m.confirmingOverwrite = true

	// Press esc — should go back to form with title preserved.
	m = sendSpecialKey(t, m, tea.KeyEscape)

	if m.phase != newSessionPhaseForm {
		t.Fatalf("phase = %d, want newSessionPhaseForm (%d)", m.phase, newSessionPhaseForm)
	}
	if m.confirmingOverwrite {
		t.Error("expected confirmingOverwrite=false after esc")
	}
	if m.Cancelled() {
		t.Error("expected not cancelled — should return to form, not exit")
	}
	if m.form == nil {
		t.Error("expected form to be rebuilt")
	}
	if m.fd == nil || m.fd.title != "my feature" {
		t.Fatalf("fd.title = %q, want %q — title should be preserved", m.fd.title, "my feature")
	}
}

func TestNewSession_ErrorInFormPhase_EscGoesBackToTypeSelect(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Set up form phase with an error.
	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()
	m.err = fmt.Errorf("something went wrong")

	// Press esc — should clear error and go back to typeSelect.
	m = sendSpecialKey(t, m, tea.KeyEscape)

	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect (%d)", m.phase, newSessionPhaseTypeSelect)
	}
	if m.Cancelled() {
		t.Error("expected not cancelled — should return to type select, not exit")
	}
	if m.err != nil {
		t.Errorf("expected err to be nil, got %v", m.err)
	}
}

// --- Linear Issue Tests ---

func TestNewSession_LinearTicketOptionHiddenWithoutConfig(t *testing.T) {
	sc := &stubClient{repos: []*pb.Repo{
		{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main"},
	}}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Should be at type select phase
	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect (%d)", m.phase, newSessionPhaseTypeSelect)
	}

	// Build options - should not include Linear issue
	opts := m.buildSessionTypeOptions()
	if len(opts) != 3 {
		t.Fatalf("len(opts) = %d, want 3 (no Linear option without API key)", len(opts))
	}

	// Verify Linear option is not in the list
	for _, opt := range opts {
		if opt.typ == sessionTypeLinearTicket {
			t.Fatal("Linear issue option should not be shown when LinearApiKey is empty")
		}
	}
}

func TestNewSession_LinearTicketOptionShownWithConfig(t *testing.T) {
	sc := &stubClient{repos: []*pb.Repo{
		{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main", LinearApiKey: "lin_api_abc123"},
	}}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Should be at type select phase
	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d, want newSessionPhaseTypeSelect (%d)", m.phase, newSessionPhaseTypeSelect)
	}

	// Build options - should include Linear issue
	opts := m.buildSessionTypeOptions()
	if len(opts) != 4 {
		t.Fatalf("len(opts) = %d, want 4 (including Linear option)", len(opts))
	}

	// Verify Linear option is in the list
	found := false
	for _, opt := range opts {
		if opt.typ == sessionTypeLinearTicket {
			found = true
			if opt.label != "Work on a Linear issue" {
				t.Fatalf("Linear option label = %q, want %q", opt.label, "Work on a Linear issue")
			}
		}
	}
	if !found {
		t.Fatal("Linear issue option should be shown when LinearApiKey is set")
	}
}

func TestNewSession_LinearTicketCreatesSessionWithBracketTitle(t *testing.T) {
	sc := &stubClient{
		repos: []*pb.Repo{
			{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main", LinearApiKey: "lin_api_abc123"},
		},
		trackerIssues: []*pb.TrackerIssue{
			{ExternalId: "ENG-123", Title: "Add authentication", Description: "Implement user auth flow", State: "In Progress", Url: "https://linear.app/team/issue/ENG-123"},
		},
		created: &pb.Session{Id: "session-1"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Select Linear issue type
	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading

	// Simulate receiving issues
	m = sendMsg(t, m, issuesMsg{issues: sc.trackerIssues})

	// Should be at issue select phase
	if m.phase != newSessionPhaseIssueSelect {
		t.Fatalf("phase = %d, want newSessionPhaseIssueSelect (%d)", m.phase, newSessionPhaseIssueSelect)
	}

	// Select first issue and press enter
	m.selectedIssue = sc.trackerIssues[0]
	cmd := m.startCreating()

	// Execute the command to trigger CreateSession
	if cmd != nil {
		cmd()
	}

	// Verify request has bracket title format
	if sc.createReq.Title != "[ENG-123] Add authentication" {
		t.Fatalf("CreateSession title = %q, want %q", sc.createReq.Title, "[ENG-123] Add authentication")
	}

	// Verify plan is set to description
	if sc.createReq.Plan != "Implement user auth flow" {
		t.Fatalf("CreateSession plan = %q, want %q", sc.createReq.Plan, "Implement user auth flow")
	}

	// Verify no PR number is set for new issue
	if sc.createReq.PrNumber != nil {
		t.Fatalf("CreateSession PrNumber = %v, want nil for new issue", sc.createReq.PrNumber)
	}

	// Verify tracker fields are passed through
	if sc.createReq.TrackerId == nil || *sc.createReq.TrackerId != "ENG-123" {
		t.Fatalf("CreateSession TrackerId = %v, want %q", sc.createReq.TrackerId, "ENG-123")
	}
	if sc.createReq.TrackerUrl == nil || *sc.createReq.TrackerUrl != "https://linear.app/team/issue/ENG-123" {
		t.Fatalf("CreateSession TrackerUrl = %v, want %q", sc.createReq.TrackerUrl, "https://linear.app/team/issue/ENG-123")
	}
}

func TestNewSession_LinearTicketExistingPRAttaches(t *testing.T) {
	prNum := int32(456)
	sc := &stubClient{
		repos: []*pb.Repo{
			{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main", LinearApiKey: "lin_api_abc123"},
		},
		trackerIssues: []*pb.TrackerIssue{
			{
				ExternalId:     "ENG-456",
				Title:          "Fix bug",
				Description:    "Fix critical bug",
				State:          "In Progress",
				PrNumber:       prNum,
				ExistingBranch: "eng-456-fix-bug",
			},
		},
		created: &pb.Session{Id: "session-1"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Select Linear issue type and receive issues
	m.selectedType = sessionTypeLinearTicket
	m = sendMsg(t, m, issuesMsg{issues: sc.trackerIssues})

	// Select issue with existing PR
	m.selectedIssue = sc.trackerIssues[0]
	cmd := m.startCreating()

	// Execute the command to trigger CreateSession
	if cmd != nil {
		cmd()
	}

	// Verify request attaches to existing PR
	if sc.createReq.PrNumber == nil {
		t.Fatal("CreateSession PrNumber should be set for issue with existing PR")
	}
	if *sc.createReq.PrNumber != prNum {
		t.Fatalf("CreateSession PrNumber = %d, want %d", *sc.createReq.PrNumber, prNum)
	}

	// Verify no branch name is set (using existing PR's branch)
	if sc.createReq.BranchName != nil {
		t.Fatalf("CreateSession BranchName = %v, want nil when attaching to existing PR", *sc.createReq.BranchName)
	}
}

func TestNewSession_LinearTicketNewBranch(t *testing.T) {
	sc := &stubClient{
		repos: []*pb.Repo{
			{Id: "repo-1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main", LinearApiKey: "lin_api_abc123"},
		},
		trackerIssues: []*pb.TrackerIssue{
			{
				ExternalId:  "ENG-789",
				Title:       "New feature",
				Description: "Add new feature",
				State:       "Todo",
				BranchName:  "eng-789-new-feature",
			},
		},
		created: &pb.Session{Id: "session-1"},
	}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Select Linear issue type and receive issues
	m.selectedType = sessionTypeLinearTicket
	m = sendMsg(t, m, issuesMsg{issues: sc.trackerIssues})

	// Select issue without existing PR
	m.selectedIssue = sc.trackerIssues[0]
	cmd := m.startCreating()

	// Execute the command to trigger CreateSession
	if cmd != nil {
		cmd()
	}

	// Verify request uses Linear's suggested branch name
	if sc.createReq.BranchName == nil {
		t.Fatal("CreateSession BranchName should be set for new issue")
	}
	if *sc.createReq.BranchName != "eng-789-new-feature" {
		t.Fatalf("CreateSession BranchName = %q, want %q", *sc.createReq.BranchName, "eng-789-new-feature")
	}

	// Verify no PR number is set for new issue
	if sc.createReq.PrNumber != nil {
		t.Fatalf("CreateSession PrNumber = %v, want nil for new issue", sc.createReq.PrNumber)
	}

	// Verify plan is set
	if sc.createReq.Plan != "Add new feature" {
		t.Fatalf("CreateSession plan = %q, want %q", sc.createReq.Plan, "Add new feature")
	}
}
