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

func (s *stubClient) ListTrackerIssues(context.Context, string, string) ([]*pb.TrackerIssue, error) {
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

// trackingCreateStream records whether Close was called and can be configured
// to return an error on Receive or an unknown event type on Msg.
type trackingCreateStream struct {
	received   bool
	receiveErr error // if non-nil, Receive returns false and Err returns this
	unknown    bool  // if true, Msg returns an event the wizard does not handle
	closed     bool
}

func (s *trackingCreateStream) Receive() bool {
	if s.receiveErr != nil {
		return false
	}
	if s.received {
		return false
	}
	s.received = true
	return true
}

func (s *trackingCreateStream) Msg() *pb.CreateSessionResponse {
	if s.unknown {
		// Empty response — no Event oneof set, hits the default branch.
		return &pb.CreateSessionResponse{}
	}
	return &pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{},
		},
	}
}

func (s *trackingCreateStream) Err() error {
	return s.receiveErr
}

func (s *trackingCreateStream) Close() error {
	s.closed = true
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
func (s *stubClient) MergeSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
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
func (s *stubClient) RecordChat(context.Context, string, string, string, bool) (*pb.ClaudeChat, error) {
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
func (s *stubClient) NotifyAuthChange(context.Context, string) error { return nil }

func (s *stubClient) CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubClient) ListCronJobs(context.Context) ([]*pb.CronJob, error) { panic("unused") }
func (s *stubClient) UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubClient) DeleteCronJob(context.Context, string) error { panic("unused") }
func (s *stubClient) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
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

	// Verify plan is set to the formatted Linear block (header + description + URL).
	wantPlan := "Linear issue:\n\n[ENG-123] Add authentication\n\nImplement user auth flow\n\nhttps://linear.app/team/issue/ENG-123\n"
	if sc.createReq.Plan != wantPlan {
		t.Fatalf("CreateSession plan = %q, want %q", sc.createReq.Plan, wantPlan)
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

	// Verify plan is the formatted Linear block. Issue has no URL set, so
	// the URL line is absent.
	wantPlan := "Linear issue:\n\n[ENG-789] New feature\n\nAdd new feature\n"
	if sc.createReq.Plan != wantPlan {
		t.Fatalf("CreateSession plan = %q, want %q", sc.createReq.Plan, wantPlan)
	}
}

func TestFormatLinearPrompt(t *testing.T) {
	tests := []struct {
		name  string
		issue *pb.TrackerIssue
		want  string
	}{
		{
			name: "all fields",
			issue: &pb.TrackerIssue{
				ExternalId:  "ENG-1",
				Title:       "Do the thing",
				Description: "Full description body.",
				Url:         "https://linear.app/x/issue/ENG-1",
			},
			want: "Linear issue:\n\n[ENG-1] Do the thing\n\nFull description body.\n\nhttps://linear.app/x/issue/ENG-1\n",
		},
		{
			name: "no url",
			issue: &pb.TrackerIssue{
				ExternalId:  "ENG-2",
				Title:       "Something",
				Description: "Short description.",
			},
			want: "Linear issue:\n\n[ENG-2] Something\n\nShort description.\n",
		},
		{
			name: "no description",
			issue: &pb.TrackerIssue{
				ExternalId: "ENG-3",
				Title:      "Bare title",
				Url:        "https://linear.app/x/issue/ENG-3",
			},
			want: "Linear issue:\n\n[ENG-3] Bare title\n\nhttps://linear.app/x/issue/ENG-3\n",
		},
		{
			name: "description surrounded by whitespace gets trimmed",
			issue: &pb.TrackerIssue{
				ExternalId:  "ENG-4",
				Title:       "Padded",
				Description: "\n\n  body text  \n\n",
			},
			want: "Linear issue:\n\n[ENG-4] Padded\n\nbody text\n",
		},
		{
			name:  "nil issue",
			issue: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLinearPrompt(tt.issue)
			if got != tt.want {
				t.Errorf("formatLinearPrompt mismatch\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// TestReadNextStreamMsgClosesOnTerminalPaths verifies that the reader goroutine
// closes the stream on every terminal branch — receive error, clean EOF, and
// unknown event type — so that a reader error cannot leak the underlying RPC
// stream. SetupOutput is non-terminal and must leave the stream open.
func TestReadNextStreamMsgClosesOnTerminalPaths(t *testing.T) {
	cases := []struct {
		name   string
		stream *trackingCreateStream
		want   any
	}{
		{
			name:   "receive error",
			stream: &trackingCreateStream{receiveErr: fmt.Errorf("boom")},
			want:   streamErrorMsg{},
		},
		{
			name:   "unexpected event",
			stream: &trackingCreateStream{unknown: true},
			want:   streamErrorMsg{},
		},
		{
			name:   "session created",
			stream: &trackingCreateStream{},
			want:   streamSessionCreatedMsg{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := readNextStreamMsg(tc.stream)
			msg := cmd()
			switch tc.want.(type) {
			case streamErrorMsg:
				if _, ok := msg.(streamErrorMsg); !ok {
					t.Fatalf("got %T, want streamErrorMsg", msg)
				}
			case streamSessionCreatedMsg:
				if _, ok := msg.(streamSessionCreatedMsg); !ok {
					t.Fatalf("got %T, want streamSessionCreatedMsg", msg)
				}
			}
			if !tc.stream.closed {
				t.Fatal("stream was not closed on terminal path")
			}
		})
	}
}

// TestReadNextStreamMsgDoesNotCloseOnSetupOutput ensures that non-terminal
// SetupOutput messages leave the stream open so the wizard can keep reading.
func TestReadNextStreamMsgDoesNotCloseOnSetupOutput(t *testing.T) {
	stream := &setupOutputStream{}
	cmd := readNextStreamMsg(stream)
	msg := cmd()
	if _, ok := msg.(setupScriptLineMsg); !ok {
		t.Fatalf("got %T, want setupScriptLineMsg", msg)
	}
	if stream.closed {
		t.Fatal("stream must remain open on non-terminal SetupOutput")
	}
}

// setupOutputStream yields a single SetupOutput event then EOFs. Close is
// tracked so tests can assert the stream stayed open on non-terminal paths.
type setupOutputStream struct {
	received bool
	closed   bool
}

func (s *setupOutputStream) Receive() bool {
	if s.received {
		return false
	}
	s.received = true
	return true
}

func (s *setupOutputStream) Msg() *pb.CreateSessionResponse {
	return &pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "line"},
		},
	}
}

func (s *setupOutputStream) Err() error   { return nil }
func (s *setupOutputStream) Close() error { s.closed = true; return nil }

// TestNewSession_PRRefetchResetsStaleFilteredIndices is a regression test for
// the panic that occurred when the user navigated back and fetched a shorter
// PR list: the first prsMsg populated m.prsFiltered with indices into the old
// (longer) list, and a second prsMsg with fewer PRs would reuse those stale
// indices and index past the end of m.prs.
func TestNewSession_PRRefetchResetsStaleFilteredIndices(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	longList := []*pb.PRSummary{
		{Number: 1, Title: "alpha", HeadBranch: "a"},
		{Number: 2, Title: "beta", HeadBranch: "b"},
		{Number: 3, Title: "gamma", HeadBranch: "c"},
	}
	m = sendMsg(t, m, prsMsg{prs: longList})
	if got := len(m.prsFiltered); got != len(longList) {
		t.Fatalf("after first prsMsg: len(prsFiltered)=%d, want %d", got, len(longList))
	}

	// Simulate the user navigating back and a second fetch returning fewer
	// PRs. Without the fix this would reuse stale indices (0,1,2) against a
	// two-element slice and panic in buildPRTable's row loop.
	shortList := []*pb.PRSummary{
		{Number: 10, Title: "fresh-one", HeadBranch: "x"},
		{Number: 11, Title: "fresh-two", HeadBranch: "y"},
	}
	m = sendMsg(t, m, prsMsg{prs: shortList})

	if got := len(m.prs); got != len(shortList) {
		t.Fatalf("len(prs)=%d, want %d", got, len(shortList))
	}
	if got := len(m.prsFiltered); got != len(shortList) {
		t.Fatalf("len(prsFiltered)=%d, want %d after refetch", got, len(shortList))
	}
	for _, i := range m.prsFiltered {
		if i < 0 || i >= len(m.prs) {
			t.Fatalf("stale index %d in prsFiltered (len(prs)=%d)", i, len(m.prs))
		}
	}
}

// TestNewSession_PRFilterActivationRebuildsTable is a regression test for the
// bug where pressing "/" from idle transitioned the filter line from hidden
// (Height=0) to visible (Height=1) but did not rebuild the PR table. The
// table's stored height was then stale by one row until the user typed the
// first character — enough to overflow the terminal.
func TestNewSession_PRFilterActivationRebuildsTable(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	// Small terminal height so the clamp in prTableHeight() reacts to the
	// filter's 1-row overhead — otherwise `needed` stays below `avail` and
	// the bug is invisible.
	m = sendMsg(t, m, tea.WindowSizeMsg{Width: 200, Height: 13})

	m.selectedType = sessionTypeExistingPR
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, prsMsg{prs: []*pb.PRSummary{
		{Number: 1, Title: "alpha", HeadBranch: "a"},
		{Number: 2, Title: "beta", HeadBranch: "b"},
		{Number: 3, Title: "gamma", HeadBranch: "c"},
	}})
	if m.phase != newSessionPhasePRSelect {
		t.Fatalf("phase = %d, want newSessionPhasePRSelect (%d)", m.phase, newSessionPhasePRSelect)
	}

	heightBefore := m.prTable.Height()
	expectedAfter := heightBefore - 1 // filter line steals one row from the table
	m = sendKey(t, m, '/')
	if !m.prFilter.Active() {
		t.Fatalf("prFilter.Active() = false after '/', want true")
	}
	if got := m.prTable.Height(); got != expectedAfter {
		t.Fatalf("prTable.Height() = %d after '/', want %d (before=%d, minus filter overhead) — table not rebuilt on filter activation", got, expectedAfter, heightBefore)
	}
}

// TestNewSession_IssueFilterActivationRebuildsTable mirrors the PR regression
// for the Linear issue selector.
func TestNewSession_IssueFilterActivationRebuildsTable(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m = sendMsg(t, m, tea.WindowSizeMsg{Width: 200, Height: 13})

	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, issuesMsg{issues: []*pb.TrackerIssue{
		{ExternalId: "ENG-1", Title: "alpha", State: "open"},
		{ExternalId: "ENG-2", Title: "beta", State: "open"},
		{ExternalId: "ENG-3", Title: "gamma", State: "open"},
	}})
	if m.phase != newSessionPhaseIssueSelect {
		t.Fatalf("phase = %d, want newSessionPhaseIssueSelect (%d)", m.phase, newSessionPhaseIssueSelect)
	}

	heightBefore := m.issueTable.Height()
	expectedAfter := heightBefore - 1
	m = sendKey(t, m, '/')
	if !m.issueFilter.Active() {
		t.Fatalf("issueFilter.Active() = false after '/', want true")
	}
	if got := m.issueTable.Height(); got != expectedAfter {
		t.Fatalf("issueTable.Height() = %d after '/', want %d (before=%d, minus filter overhead) — table not rebuilt on filter activation", got, expectedAfter, heightBefore)
	}
}

// TestNewSession_IssueFilterDebouncedSearch verifies that typing into the
// issue filter schedules a debounced server-side search instead of only doing
// local matching, and that stale responses are dropped while fresh ones land.
func TestNewSession_IssueFilterDebouncedSearch(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m = sendMsg(t, m, tea.WindowSizeMsg{Width: 200, Height: 13})

	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, issuesMsg{issues: []*pb.TrackerIssue{
		{ExternalId: "ENG-1", Title: "alpha", State: "open"},
		{ExternalId: "ENG-2", Title: "beta", State: "open"},
	}})

	// Activate filter and type a character.
	m = sendKey(t, m, '/')
	startSeq := m.issueSearchSeq

	updated, cmd := m.Update(keyPress('a'))
	m = assertValueType(t, updated)

	if m.issueSearchSeq != startSeq+1 {
		t.Errorf("issueSearchSeq = %d after keystroke, want %d", m.issueSearchSeq, startSeq+1)
	}
	if !m.issuesFetching {
		t.Error("issuesFetching = false after keystroke, want true")
	}
	if cmd == nil {
		t.Fatal("expected a cmd batch (tick + input update) after keystroke, got nil")
	}

	// A stale issuesMsg (query doesn't match the live one) must be ignored —
	// trackerIssues should keep its prior contents.
	prevTitles := []string{m.trackerIssues[0].Title, m.trackerIssues[1].Title}
	m = sendMsg(t, m, issuesMsg{
		issues: []*pb.TrackerIssue{{ExternalId: "STALE", Title: "stale-result"}},
		query:  "different-query",
	})
	if len(m.trackerIssues) != 2 || m.trackerIssues[0].Title != prevTitles[0] {
		t.Errorf("stale issuesMsg overwrote trackerIssues: got %+v, want titles %v", m.trackerIssues, prevTitles)
	}

	// Simulate the debounce tick firing — handler should set issueSearchQuery
	// to the live query and dispatch a fetch (cmd is the fetch closure).
	tickQuery := m.issueFilter.Query()
	updated, cmd = m.Update(searchIssuesTickMsg{seq: m.issueSearchSeq, query: tickQuery})
	m = assertValueType(t, updated)
	if m.issueSearchQuery != tickQuery {
		t.Errorf("issueSearchQuery = %q after tick, want %q", m.issueSearchQuery, tickQuery)
	}
	if cmd == nil {
		t.Error("expected fetchIssues cmd after tick fired, got nil")
	}

	// Fresh result for the live query lands and replaces trackerIssues.
	m = sendMsg(t, m, issuesMsg{
		issues: []*pb.TrackerIssue{{ExternalId: "ENG-99", Title: "fresh-match", State: "open"}},
		seq:    m.issueSearchSeq,
		query:  tickQuery,
	})
	if len(m.trackerIssues) != 1 || m.trackerIssues[0].Title != "fresh-match" {
		t.Errorf("fresh issuesMsg did not replace trackerIssues: got %+v", m.trackerIssues)
	}
	if m.issuesFetching {
		t.Error("issuesFetching = true after fresh issuesMsg, want false")
	}
}

// TestNewSession_IssueFilterDebounceSeqDropsStaleTick verifies that a tick
// whose seq has been superseded by a newer keystroke is silently dropped —
// otherwise rapid typing would fan out into many concurrent searches.
func TestNewSession_IssueFilterDebounceSeqDropsStaleTick(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m = sendMsg(t, m, tea.WindowSizeMsg{Width: 200, Height: 13})
	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, issuesMsg{issues: []*pb.TrackerIssue{{ExternalId: "ENG-1", Title: "alpha"}}})
	m = sendKey(t, m, '/')

	// Two keystrokes — second one supersedes the first.
	updated, _ := m.Update(keyPress('a'))
	m = assertValueType(t, updated)
	staleSeq := m.issueSearchSeq
	updated, _ = m.Update(keyPress('b'))
	m = assertValueType(t, updated)

	if m.issueSearchSeq <= staleSeq {
		t.Fatalf("issueSearchSeq did not advance: stale=%d, new=%d", staleSeq, m.issueSearchSeq)
	}

	// Tick with the stale seq must be a no-op (no fetch dispatched).
	updated, cmd := m.Update(searchIssuesTickMsg{seq: staleSeq, query: "a"})
	m = assertValueType(t, updated)
	if cmd != nil {
		t.Errorf("expected stale tick to be dropped (cmd=nil), got %T", cmd)
	}
	if m.issueSearchQuery == "a" {
		t.Error("stale tick updated issueSearchQuery — guard failed")
	}
}

// TestNewSession_IssueLateFetchAfterEscDoesNotSnapBack verifies that an
// in-flight issue fetch returning after the user has backed out of the issue
// selector does not re-enter the issue-select phase.
func TestNewSession_IssueLateFetchAfterEscDoesNotSnapBack(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m = sendMsg(t, m, tea.WindowSizeMsg{Width: 200, Height: 13})

	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, issuesMsg{issues: []*pb.TrackerIssue{{ExternalId: "ENG-1", Title: "alpha"}}})
	if m.phase != newSessionPhaseIssueSelect {
		t.Fatalf("phase = %d, want newSessionPhaseIssueSelect", m.phase)
	}

	// Simulate the user activating the filter, typing, and pressing Esc —
	// which deactivates the filter and dispatches a fetch for "". Capture the
	// seq the in-flight fetch was issued with.
	m = sendKey(t, m, '/')
	updated, _ := m.Update(keyPress('a'))
	m = assertValueType(t, updated)
	m = sendSpecialKey(t, m, tea.KeyEscape)
	inFlightSeq := m.issueSearchSeq

	// Second Esc — back out of the issue selector entirely. This must
	// invalidate the in-flight fetch so its response does not snap the user
	// back to the issue-select phase.
	m = sendSpecialKey(t, m, tea.KeyEscape)
	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("phase = %d after Esc, want newSessionPhaseTypeSelect", m.phase)
	}

	// Late response from the Esc-triggered refetch arrives. Handler must drop
	// it rather than force the phase back to issue select.
	m = sendMsg(t, m, issuesMsg{
		issues: []*pb.TrackerIssue{{ExternalId: "ENG-1", Title: "alpha"}},
		seq:    inFlightSeq,
		query:  "",
	})
	if m.phase != newSessionPhaseTypeSelect {
		t.Fatalf("late issuesMsg snapped phase back to %d, want newSessionPhaseTypeSelect", m.phase)
	}
}

// TestNewSession_IssueStaleFetchDroppedWhileTyping verifies that a fetch
// issued by an earlier debounce tick is discarded when the user keeps typing
// before the response arrives — otherwise the "searching…" indicator would
// disappear and the cursor would jump to row 0 mid-keystroke.
func TestNewSession_IssueStaleFetchDroppedWhileTyping(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m = sendMsg(t, m, tea.WindowSizeMsg{Width: 200, Height: 13})

	m.selectedType = sessionTypeLinearTicket
	m.phase = newSessionPhaseLoading
	m = sendMsg(t, m, issuesMsg{issues: []*pb.TrackerIssue{
		{ExternalId: "ENG-1", Title: "alpha", State: "open"},
		{ExternalId: "ENG-2", Title: "beta", State: "open"},
	}})

	// First keystroke → first tick fires → fetch dispatched with seq=1.
	m = sendKey(t, m, '/')
	updated, _ := m.Update(keyPress('a'))
	m = assertValueType(t, updated)
	updated, _ = m.Update(searchIssuesTickMsg{seq: m.issueSearchSeq, query: m.issueFilter.Query()})
	m = assertValueType(t, updated)
	firstFetchSeq := m.issueSearchSeq

	// User keeps typing before the first fetch returns — seq bumps past the
	// in-flight fetch's seq.
	updated, _ = m.Update(keyPress('b'))
	m = assertValueType(t, updated)
	if m.issueSearchSeq == firstFetchSeq {
		t.Fatalf("issueSearchSeq did not advance after second keystroke")
	}
	if !m.issuesFetching {
		t.Fatal("issuesFetching = false while user is still typing")
	}

	// Late response from the first fetch arrives. It must be dropped so the
	// "searching…" indicator stays up and the cached rows are preserved.
	m = sendMsg(t, m, issuesMsg{
		issues: []*pb.TrackerIssue{{ExternalId: "ENG-STALE", Title: "stale"}},
		seq:    firstFetchSeq,
		query:  "a",
	})
	if !m.issuesFetching {
		t.Error("stale issuesMsg cleared issuesFetching — guard failed")
	}
	if len(m.trackerIssues) != 2 {
		t.Errorf("stale issuesMsg overwrote trackerIssues: got %d entries, want 2", len(m.trackerIssues))
	}
}

// TestNewSession_IssueRefetchResetsStaleFilteredIndices mirrors the PR
// regression for the Linear issue selector.
func TestNewSession_IssueRefetchResetsStaleFilteredIndices(t *testing.T) {
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})

	longList := []*pb.TrackerIssue{
		{ExternalId: "ENG-1", Title: "alpha", State: "open"},
		{ExternalId: "ENG-2", Title: "beta", State: "open"},
		{ExternalId: "ENG-3", Title: "gamma", State: "open"},
	}
	m = sendMsg(t, m, issuesMsg{issues: longList})
	if got := len(m.issuesFiltered); got != len(longList) {
		t.Fatalf("after first issuesMsg: len(issuesFiltered)=%d, want %d", got, len(longList))
	}

	shortList := []*pb.TrackerIssue{
		{ExternalId: "ENG-9", Title: "fresh-one", State: "open"},
	}
	m = sendMsg(t, m, issuesMsg{issues: shortList})

	if got := len(m.trackerIssues); got != len(shortList) {
		t.Fatalf("len(trackerIssues)=%d, want %d", got, len(shortList))
	}
	if got := len(m.issuesFiltered); got != len(shortList) {
		t.Fatalf("len(issuesFiltered)=%d, want %d after refetch", got, len(shortList))
	}
	for _, i := range m.issuesFiltered {
		if i < 0 || i >= len(m.trackerIssues) {
			t.Fatalf("stale index %d in issuesFiltered (len=%d)", i, len(m.trackerIssues))
		}
	}
}
