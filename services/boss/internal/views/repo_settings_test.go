package views

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestRepoSettings_MaskAPIKey verifies maskAPIKey masking logic.
func TestRepoSettings_MaskAPIKey(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		expected string
	}{
		{"empty", "", "(not set)"},
		{"short key (4 chars)", "test", "test"},
		{"short key (3 chars)", "abc", "abc"},
		{"normal key", "lin_api_abcdefghij1234", "******************1234"},
		{"minimal masked", "12345", "*2345"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskAPIKey(tt.key)
			if result != tt.expected {
				t.Errorf("maskAPIKey(%q) = %q, want %q", tt.key, result, tt.expected)
			}
		})
	}
}

// TestRepoSettings_LinearApiKeyEditIsFullReplace verifies that API key editing
// always starts with an empty input (full replace, not edit).
func TestRepoSettings_LinearApiKeyEditIsFullReplace(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:           "repo-1",
			DisplayName:  "Test Repo",
			LinearApiKey: "lin_api_existing_key_1234",
		}},
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")

	// Initialize the model
	initCmd := m.Init()
	msg := initCmd()
	updatedModel, _ := m.Update(msg)
	m = updatedModel.(RepoSettingsModel)

	// Navigate to API key row and activate. Linear is expanded because the key
	// is set, so the child row is navigable.
	cursorToRow(t, &m, repoSettingsRowLinearApiKey)
	updatedModel, _ = m.activateRow()
	m = updatedModel.(RepoSettingsModel)

	// Verify input is empty (not pre-filled)
	if m.linearApiKeyInput.Value() != "" {
		t.Errorf("linearApiKeyInput.Value() = %q, want empty string (full replace)", m.linearApiKeyInput.Value())
	}

	// Verify editing field is set
	if m.editingField != repoSettingsRowLinearApiKey {
		t.Errorf("editingField = %d, want %d", m.editingField, repoSettingsRowLinearApiKey)
	}
}

// cursorToRow positions the cursor on the given rowID, expanding the owning
// integration first if the row is an integration child, then failing the test if
// the row is still not visible.
func cursorToRow(t *testing.T, m *RepoSettingsModel, row rowID) {
	t.Helper()
	switch row {
	case repoSettingsRowLinearApiKey:
		m.linearExpanded = true
	case repoSettingsRowSentryApiKey, repoSettingsRowSentryOrg:
		m.sentryExpanded = true
	default:
		// Non-child rows are always visible; no expansion needed.
	}
	for i, r := range m.visibleRows() {
		if r == row {
			m.cursor = i
			return
		}
	}
	t.Fatalf("row %d is not visible; visibleRows = %v", row, m.visibleRows())
}

// TestRepoSettings_CollapsedNavigationSkipsChildRows verifies that when both
// integrations are collapsed, only the headers (not their child fields) are in
// the navigable row set, and down-navigation lands on each in order.
func TestRepoSettings_CollapsedNavigationSkipsChildRows(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:          "repo-1",
			DisplayName: "Test Repo",
		}},
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	updatedModel, _ := m.Update(m.Init()())
	m = updatedModel.(RepoSettingsModel)

	// No credentials → both integrations collapsed. Child rows are hidden.
	expectedRows := []rowID{
		repoSettingsRowName,
		repoSettingsRowSetupScript,
		repoSettingsRowMergeStrategy,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowLinearHeader,
		repoSettingsRowSentryHeader,
	}

	for i, want := range expectedRows {
		if m.currentRow() != want {
			t.Errorf("currentRow at step %d = %d, want %d", i, m.currentRow(), want)
		}
		if i < len(expectedRows)-1 {
			updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			m = updatedModel.(RepoSettingsModel)
		}
	}

	// Down past the last row stays on the Sentry header.
	updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updatedModel.(RepoSettingsModel)
	if m.currentRow() != repoSettingsRowSentryHeader {
		t.Errorf("cursor after boundary = %d, want Sentry header", m.currentRow())
	}
}

// TestRepoSettings_ExpandedNavigationIncludesChildRows verifies that when both
// integrations are expanded (credentials present), the child field rows are
// navigable in order.
func TestRepoSettings_ExpandedNavigationIncludesChildRows(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:           "repo-1",
			DisplayName:  "Test Repo",
			LinearApiKey: "lin_api_123",
			SentryApiKey: "sntrys_123",
			SentryOrg:    "acme",
		}},
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	updatedModel, _ := m.Update(m.Init()())
	m = updatedModel.(RepoSettingsModel)

	expectedRows := []rowID{
		repoSettingsRowName,
		repoSettingsRowSetupScript,
		repoSettingsRowMergeStrategy,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowLinearHeader,
		repoSettingsRowLinearApiKey,
		repoSettingsRowSentryHeader,
		repoSettingsRowSentryApiKey,
		repoSettingsRowSentryOrg,
	}

	for i, want := range expectedRows {
		if m.currentRow() != want {
			t.Errorf("currentRow at step %d = %d, want %d", i, m.currentRow(), want)
		}
		if i < len(expectedRows)-1 {
			updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			m = updatedModel.(RepoSettingsModel)
		}
	}

	if m.currentRow() != repoSettingsRowSentryOrg {
		t.Errorf("final cursor = %d, want Sentry org", m.currentRow())
	}
}

func TestRepoSettings_GitHubActionLabels(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:          "repo-1",
			DisplayName: "Test Repo",
			OriginUrl:   "https://github.com/freshclaim/widgets.git",
		}},
	}
	gh := &fakeRepoSettingsGitHubAppClient{
		repos: []*pb.GitHubAppRepoStatus{{
			Owner:          "freshclaim",
			Name:           "widgets",
			Installed:      true,
			InstallationId: 133291047,
		}},
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetGitHubAppInstall(gh)
	initCmd := m.Init()
	updated, statusCmd := m.Update(initCmd())
	m = updated.(RepoSettingsModel)
	if statusCmd == nil {
		t.Fatal("expected GitHub App status load command")
	}
	updated, _ = m.Update(statusCmd())
	m = updated.(RepoSettingsModel)

	view := m.View().Content
	if !strings.Contains(view, "[g]ithub app") {
		t.Fatalf("expected github app action; view:\n%s", view)
	}
	if strings.Contains(view, "[g] connect Github") {
		t.Fatalf("did not expect connect action for installed repo; view:\n%s", view)
	}
}

func TestRepoSettings_GitHubConnectPromptOpensInstallURL(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:          "repo-1",
			DisplayName: "Test Repo",
			OriginUrl:   "https://github.com/freshclaim/widgets.git",
		}},
	}
	gh := &fakeRepoSettingsGitHubAppClient{
		installURL: "https://github.com/apps/bossanova-dev/installations/new",
	}

	originalOpen := openGitHubAppInstallURL
	originalLookup := lookupGitHubAppInstallTarget
	var openedURL string
	openGitHubAppInstallURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	lookupGitHubAppInstallTarget = func(context.Context, string) (githubAppInstallTarget, error) {
		return githubAppInstallTarget{}, nil
	}
	t.Cleanup(func() {
		openGitHubAppInstallURL = originalOpen
		lookupGitHubAppInstallTarget = originalLookup
	})

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetGitHubAppInstall(gh)
	initCmd := m.Init()
	updated, statusCmd := m.Update(initCmd())
	m = updated.(RepoSettingsModel)
	if statusCmd != nil {
		updated, _ = m.Update(statusCmd())
		m = updated.(RepoSettingsModel)
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	m = updated.(RepoSettingsModel)
	if cmd != nil {
		t.Fatal("expected prompt before opening GitHub App URL")
	}
	if !strings.Contains(m.View().Content, "Install the Bossanova Github App on freshclaim/widgets?") {
		t.Fatalf("expected GitHub App prompt; view:\n%s", m.View().Content)
	}

	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("expected install URL request command")
	}
	updated, cmd = m.Update(cmd())
	m = updated.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("expected browser open command")
	}
	updated, _ = m.Update(cmd())
	m = updated.(RepoSettingsModel)

	if openedURL != gh.installURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, gh.installURL)
	}
	if m.githubAppMode != repoSettingsGitHubModeNone {
		t.Fatalf("githubAppMode = %v, want none after successful open", m.githubAppMode)
	}
}

func TestRepoSettings_GitHubInstalledOpensInstallationSettingsURL(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:          "repo-1",
			DisplayName: "Test Repo",
			OriginUrl:   "https://github.com/freshclaim/widgets.git",
		}},
	}
	gh := &fakeRepoSettingsGitHubAppClient{
		repos: []*pb.GitHubAppRepoStatus{{
			Owner:          "freshclaim",
			Name:           "widgets",
			Installed:      true,
			InstallationId: 133291047,
		}},
	}
	originalOpen := openGitHubAppInstallURL
	var openedURL string
	openGitHubAppInstallURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	t.Cleanup(func() { openGitHubAppInstallURL = originalOpen })

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetGitHubAppInstall(gh)
	initCmd := m.Init()
	updated, statusCmd := m.Update(initCmd())
	m = updated.(RepoSettingsModel)
	if statusCmd == nil {
		t.Fatal("expected GitHub App status load command")
	}
	updated, _ = m.Update(statusCmd())
	m = updated.(RepoSettingsModel)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	m = updated.(RepoSettingsModel)
	if cmd != nil {
		t.Fatal("expected confirmation prompt before opening installed settings page")
	}
	if !strings.Contains(m.View().Content, "Open the Bossanova GitHub App settings for freshclaim/widgets") {
		t.Fatalf("expected settings-open prompt; view:\n%s", m.View().Content)
	}
	if openedURL != "" {
		t.Fatalf("opened URL = %q, want none before confirmation", openedURL)
	}

	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("expected browser open command after confirmation")
	}
	updated, _ = m.Update(cmd())
	m = updated.(RepoSettingsModel)

	want := "https://github.com/organizations/freshclaim/settings/installations/133291047"
	if openedURL != want {
		t.Fatalf("opened URL = %q, want %q", openedURL, want)
	}
	if m.githubAppMode != repoSettingsGitHubModeNone {
		t.Fatalf("githubAppMode = %v, want none after successful open", m.githubAppMode)
	}
	if gh.installURLCalls != 0 {
		t.Fatalf("install URL calls = %d, want 0 for installed settings path", gh.installURLCalls)
	}
}

func TestRepoSettings_GitHubConnectSurfacesInstallURLLookupFailure(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:          "repo-1",
			DisplayName: "Test Repo",
			OriginUrl:   "https://github.com/freshclaim/widgets.git",
		}},
	}
	lookupErr := errors.New("lookup failed")
	gh := &fakeRepoSettingsGitHubAppClient{installErr: lookupErr}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetGitHubAppInstall(gh)
	initCmd := m.Init()
	updated, statusCmd := m.Update(initCmd())
	m = updated.(RepoSettingsModel)
	if statusCmd != nil {
		updated, _ = m.Update(statusCmd())
		m = updated.(RepoSettingsModel)
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	m = updated.(RepoSettingsModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("expected install URL request command")
	}
	updated, followup := m.Update(cmd())
	m = updated.(RepoSettingsModel)
	if followup != nil {
		t.Fatal("unexpected follow-up command after install URL failure")
	}
	if !errors.Is(m.githubAppInstallErr, lookupErr) {
		t.Fatalf("githubAppInstallErr = %v, want %v", m.githubAppInstallErr, lookupErr)
	}
	if gh.installURLCalls != 1 {
		t.Fatalf("install URL calls = %d, want 1", gh.installURLCalls)
	}
	if m.githubAppMode != repoSettingsGitHubModeOpening {
		t.Fatalf("githubAppMode = %v, want opening so retry remains available", m.githubAppMode)
	}
	if !strings.Contains(m.View().Content, "Could not start GitHub App installation: lookup failed") {
		t.Fatalf("view did not surface lookup failure:\n%s", m.View().Content)
	}
}

func TestGitHubAppInstallationSettingsURL(t *testing.T) {
	status := &pb.GitHubAppRepoStatus{
		Owner:          "freshclaim",
		InstallationId: 133291047,
	}
	got := githubAppInstallationSettingsURL(status)
	want := "https://github.com/organizations/freshclaim/settings/installations/133291047"
	if got != want {
		t.Fatalf("githubAppInstallationSettingsURL() = %q, want %q", got, want)
	}
}

// TestRepoSettingsLoadFailureSurfacesErrorAndCancels verifies that when the
// initial repo load fails (e.g. opening repo settings for a repo that can't be
// loaded), the error is shown to the user and esc cancels the view.
func TestRepoSettingsLoadFailureSurfacesErrorAndCancels(t *testing.T) {
	stub := &stubRepoClient{
		reposErr: errors.New("load failed"),
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")

	// Feed the load-failure message directly (same as Init() → ListRepos returning error).
	updated, _ := m.Update(repoSettingsLoadedMsg{err: errors.New("load failed")})
	m = updated.(RepoSettingsModel)

	// The error text must appear in the view.
	view := m.View().Content
	if !strings.Contains(view, "load failed") {
		t.Fatalf("expected 'load failed' in view; got:\n%s", view)
	}

	// Pressing esc must cancel the view.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(RepoSettingsModel)
	if !m.Cancelled() {
		t.Fatal("expected Cancelled() to be true after esc on error screen")
	}
}

func TestRepoSettings_AutomationsAndIntegrationsHeadings(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:                     "repo-1",
		DisplayName:            "Test Repo",
		CanAutoMerge:           true,
		CanAutoMergeDependabot: true,
		CanAutoRepair:          true,
	}}}
	m := initSettings(t, stub)
	out := m.View().Content

	if !strings.Contains(out, "Automations") {
		t.Errorf("expected an \"Automations\" section heading, not found in:\n%s", out)
	}
	if !strings.Contains(out, "Integrations") {
		t.Errorf("expected an \"Integrations\" section heading, not found in:\n%s", out)
	}
	// "Automations" renders above "Integrations".
	if strings.Index(out, "Automations") >= strings.Index(out, "Integrations") {
		t.Errorf("expected the Automations heading above the Integrations heading:\n%s", out)
	}
}

func TestRepoSettings_AutomationTogglesUnderAutomationsHeading(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:                     "repo-1",
		DisplayName:            "Test Repo",
		CanAutoMerge:           true,
		CanAutoMergeDependabot: true,
		CanAutoRepair:          true,
	}}}
	m := initSettings(t, stub)
	out := m.View().Content

	automationsIdx := strings.Index(out, "Automations")
	markReadyIdx := strings.Index(out, "Mark ready for review when checks pass")
	depIdx := strings.Index(out, "Auto-merge Dependabot PRs")
	repairIdx := strings.Index(out, "Automatic repair (failing checks, conflicts, review feedback)")
	integrationsIdx := strings.Index(out, "Integrations")

	if automationsIdx < 0 || markReadyIdx < 0 || depIdx < 0 || repairIdx < 0 || integrationsIdx < 0 {
		t.Fatalf("missing expected labels: automations=%d markReady=%d dep=%d repair=%d integrations=%d\n%s",
			automationsIdx, markReadyIdx, depIdx, repairIdx, integrationsIdx, out)
	}
	// Under "Automations", in order: Mark ready, Auto-merge Dependabot, Automatic repair.
	if automationsIdx >= markReadyIdx || markReadyIdx >= depIdx || depIdx >= repairIdx {
		t.Errorf("expected order Automations < markReady < dependabot < repair; got %d < %d < %d < %d",
			automationsIdx, markReadyIdx, depIdx, repairIdx)
	}
	// The three automation toggles all render above the Integrations heading.
	if repairIdx >= integrationsIdx {
		t.Errorf("automation toggles should render above the Integrations heading; repair=%d integrations=%d",
			repairIdx, integrationsIdx)
	}
}

func TestRepoSettings_VisibleRowsOrderMatchesRender(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:          "repo-1",
		DisplayName: "Test Repo",
	}}}
	m := initSettings(t, stub)
	got := m.visibleRows()
	want := []rowID{
		repoSettingsRowName,
		repoSettingsRowSetupScript,
		repoSettingsRowMergeStrategy,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowLinearHeader,
		repoSettingsRowSentryHeader,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visibleRows order mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// stubRepoClient implements client.BossClient for testing RepoSettingsModel.
type stubRepoClient struct {
	repos     []*pb.Repo
	reposErr  error
	updated   *pb.Repo
	updateErr error
	updateReq *pb.UpdateRepoRequest // captures the last UpdateRepo request
}

type fakeRepoSettingsGitHubAppClient struct {
	installURL      string
	installErr      error
	installURLCalls int
	repos           []*pb.GitHubAppRepoStatus
}

func (f *fakeRepoSettingsGitHubAppClient) GetGitHubAppInstallURL(context.Context, string) (string, error) {
	f.installURLCalls++
	return f.installURL, f.installErr
}

func (f *fakeRepoSettingsGitHubAppClient) ListGitHubAppRepos(context.Context) ([]*pb.GitHubAppRepoStatus, error) {
	return f.repos, nil
}

func (s *stubRepoClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	return s.repos, s.reposErr
}

func (s *stubRepoClient) UpdateRepo(_ context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	s.updateReq = req
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return s.updated, nil
}

// Unused interface methods — panic if called unexpectedly.
func (s *stubRepoClient) Ping(context.Context) error { panic("unused") }
func (s *stubRepoClient) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) ValidateRepoPath(context.Context, string) (*pb.ValidateRepoPathResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) RegisterRepo(context.Context, *pb.RegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubRepoClient) CloneAndRegisterRepo(context.Context, *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	panic("unused")
}
func (s *stubRepoClient) RemoveRepo(context.Context, string) error { panic("unused") }
func (s *stubRepoClient) GetSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) ListSessions(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) AttachSession(context.Context, string) (client.AttachStream, error) {
	panic("unused")
}
func (s *stubRepoClient) CreateSession(context.Context, *pb.CreateSessionRequest) (client.CreateSessionStream, error) {
	panic("unused")
}
func (s *stubRepoClient) RenameSession(context.Context, string, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) StopSession(context.Context, string) (*pb.Session, error)  { panic("unused") }
func (s *stubRepoClient) PauseSession(context.Context, string) (*pb.Session, error) { panic("unused") }
func (s *stubRepoClient) ResumeSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) RetrySession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) CloseSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) MergeSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) RemoveSession(context.Context, string) error { panic("unused") }
func (s *stubRepoClient) UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) LinkSessionPR(context.Context, string, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) ArchiveSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) ResurrectSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error) {
	panic("unused")
}
func (s *stubRepoClient) RecordChat(context.Context, string, string, string, string, bool) (*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubRepoClient) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubRepoClient) UpdateChatTitle(context.Context, string, string) error { panic("unused") }
func (s *stubRepoClient) DeleteChat(context.Context, string) error              { panic("unused") }
func (s *stubRepoClient) WakeChat(context.Context, string, string, bool) (*pb.WakeChatResponse, error) {
	panic("unused")
}

func (s *stubRepoClient) DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	panic("unused")
}
func (s *stubRepoClient) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	panic("unused")
}
func (s *stubRepoClient) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	panic("unused")
}
func (s *stubRepoClient) GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) SendChatMessage(context.Context, *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) NotifyAuthChange(context.Context, string) error { return nil }
func (s *stubRepoClient) ShutdownDaemon(context.Context) error           { panic("unused") }
func (s *stubRepoClient) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	panic("unused")
}
func (s *stubRepoClient) ListTrackerIssues(context.Context, string, string, string) ([]*pb.TrackerIssue, error) {
	panic("unused")
}
func (s *stubRepoClient) CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubRepoClient) ListCronJobs(context.Context) ([]*pb.CronJob, error) { panic("unused") }
func (s *stubRepoClient) UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	panic("unused")
}
func (s *stubRepoClient) DeleteCronJob(context.Context, string) error { panic("unused") }
func (s *stubRepoClient) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) RepairDoctor(context.Context) (*pb.RepairDoctorResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) ListCheckSnapshots(context.Context, string, int32) (*pb.ListCheckSnapshotsResponse, error) {
	panic("unused")
}
func (s *stubRepoClient) ListAgents(context.Context) ([]client.AgentInfo, error) { return nil, nil }
func (s *stubRepoClient) ListPlugins(context.Context) ([]*pb.InstalledPlugin, error) {
	return nil, nil
}
