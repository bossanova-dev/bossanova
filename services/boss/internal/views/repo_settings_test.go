package views

import (
	"context"
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

	// Navigate to API key row and activate
	m.cursor = repoSettingsRowLinearApiKey
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

// TestRepoSettings_CursorNavigatesToLinearRows verifies that all rows
// are reachable via cursor navigation.
func TestRepoSettings_CursorNavigatesToLinearRows(t *testing.T) {
	stub := &stubRepoClient{
		repos: []*pb.Repo{{
			Id:          "repo-1",
			DisplayName: "Test Repo",
		}},
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")

	// Initialize the model
	initCmd := m.Init()
	msg := initCmd()
	updatedModel, _ := m.Update(msg)
	m = updatedModel.(RepoSettingsModel)

	// Navigate down through all rows
	expectedRows := []int{
		repoSettingsRowName,                    // 0
		repoSettingsRowSetupScript,             // 1
		repoSettingsRowMergeStrategy,           // 2
		repoSettingsRowCanAutoMerge,            // 3
		repoSettingsRowCanAutoMergeDependabot,  // 4
		repoSettingsRowCanAutoAddressReviews,   // 5
		repoSettingsRowCanAutoResolveConflicts, // 6
		repoSettingsRowLinearApiKey,            // 7
	}

	for i, expectedRow := range expectedRows {
		if m.cursor != expectedRow {
			t.Errorf("cursor at step %d = %d, want %d", i, m.cursor, expectedRow)
		}

		// Press down arrow unless we're at the last row
		if i < len(expectedRows)-1 {
			updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			m = updatedModel.(RepoSettingsModel)
		}
	}

	// Verify we ended at the last row
	if m.cursor != repoSettingsRowLinearApiKey {
		t.Errorf("final cursor = %d, want %d (Linear API key row)", m.cursor, repoSettingsRowLinearApiKey)
	}

	// Try to go down one more time - cursor should stay at last row
	updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updatedModel.(RepoSettingsModel)
	if m.cursor != repoSettingsRowLinearApiKey {
		t.Errorf("cursor after boundary = %d, want %d (should stay at last row)", m.cursor, repoSettingsRowLinearApiKey)
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
func (s *stubRepoClient) ArchiveSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) ResurrectSession(context.Context, string) (*pb.Session, error) {
	panic("unused")
}
func (s *stubRepoClient) EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error) {
	panic("unused")
}
func (s *stubRepoClient) RecordChat(context.Context, string, string, string, bool) (*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubRepoClient) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	panic("unused")
}
func (s *stubRepoClient) UpdateChatTitle(context.Context, string, string) error { panic("unused") }
func (s *stubRepoClient) DeleteChat(context.Context, string) error              { panic("unused") }
func (s *stubRepoClient) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	panic("unused")
}
func (s *stubRepoClient) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	panic("unused")
}
func (s *stubRepoClient) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	panic("unused")
}
func (s *stubRepoClient) NotifyAuthChange(context.Context, string) error { return nil }
func (s *stubRepoClient) ShutdownDaemon(context.Context) error           { panic("unused") }
func (s *stubRepoClient) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	panic("unused")
}
func (s *stubRepoClient) ListTrackerIssues(context.Context, string, string) ([]*pb.TrackerIssue, error) {
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
