package views

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type repoAddTokenStore struct {
	tokens *auth.Tokens
}

func (s *repoAddTokenStore) Save(tokens *auth.Tokens) error {
	s.tokens = tokens
	return nil
}

func (s *repoAddTokenStore) Load() (*auth.Tokens, error) {
	if s.tokens == nil {
		return nil, fmt.Errorf("no tokens stored")
	}
	return s.tokens, nil
}

func (s *repoAddTokenStore) Delete() error {
	s.tokens = nil
	return nil
}

type fakeGitHubAppClient struct {
	installURL      string
	installErr      error
	installURLCalls int
	repos           []*pb.GitHubAppRepoStatus
	listErr         error
	listCalls       int
}

func (f *fakeGitHubAppClient) GetGitHubAppInstallURL(context.Context, string) (string, error) {
	f.installURLCalls++
	return f.installURL, f.installErr
}

func (f *fakeGitHubAppClient) ListGitHubAppRepos(context.Context) ([]*pb.GitHubAppRepoStatus, error) {
	f.listCalls++
	return f.repos, f.listErr
}

func signedInRepoAddAuth() *auth.Manager {
	store := &repoAddTokenStore{
		tokens: &auth.Tokens{
			AccessToken:  "access",
			RefreshToken: "refresh",
			Email:        "dev@example.test",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	}
	return auth.NewManager(store, auth.Config{})
}

// repoAddStubClient extends stubClient with repo-add specific behavior.
type repoAddStubClient struct {
	stubClient

	validateResp *pb.ValidateRepoPathResponse
	validateErr  error
	validatePath string // captures the last ValidateRepoPath path

	registered  *pb.Repo
	registerErr error
	registerReq *pb.RegisterRepoRequest
	cloned      *pb.Repo
	cloneErr    error
	cloneReq    *pb.CloneAndRegisterRepoRequest

	updated   *pb.Repo
	updateErr error
	updateReq *pb.UpdateRepoRequest
}

func (s *repoAddStubClient) ValidateRepoPath(_ context.Context, path string) (*pb.ValidateRepoPathResponse, error) {
	s.validatePath = path
	return s.validateResp, s.validateErr
}

func (s *repoAddStubClient) RegisterRepo(_ context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	s.registerReq = req
	return s.registered, s.registerErr
}

func (s *repoAddStubClient) CloneAndRegisterRepo(_ context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	s.cloneReq = req
	return s.cloned, s.cloneErr
}

func (s *repoAddStubClient) UpdateRepo(_ context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	s.updateReq = req
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	if s.updated != nil {
		return s.updated, nil
	}
	return s.registered, nil
}

// sendRepoAddMsg sends an arbitrary tea.Msg through Update and asserts value type.
func sendRepoAddMsg(t *testing.T, m RepoAddModel, msg tea.Msg) RepoAddModel {
	t.Helper()
	updated, _ := m.Update(msg)
	rm, ok := updated.(RepoAddModel)
	if !ok {
		t.Fatalf("Update returned %T, want views.RepoAddModel (value type)", updated)
	}
	return rm
}

func TestRepoAdd_HandleFormCompletedReturnsValueType(t *testing.T) {
	// Regression test: handleFormCompleted has a pointer receiver and must
	// return *m (dereferenced), not m (which would be *RepoAddModel).
	sc := &repoAddStubClient{
		validateResp: &pb.ValidateRepoPathResponse{
			IsValid:       true,
			IsGithub:      true,
			DefaultBranch: "main",
		},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseInput
	m.fd.localPath = "/path/to/repo"
	m.sourceMode = sourceModeOpen

	// Simulate form completion — call handleFormCompleted directly.
	result, _ := m.handleFormCompleted()
	_, ok := result.(RepoAddModel)
	if !ok {
		t.Fatalf("handleFormCompleted returned %T, want views.RepoAddModel (value type)", result)
	}
}

func TestRepoAdd_SourceTable_SelectOpen(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	if m.phase != repoAddPhaseSource {
		t.Fatalf("initial phase = %d, want repoAddPhaseSource", m.phase)
	}

	// Press enter on first row (Open project).
	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.phase != repoAddPhaseInput {
		t.Fatalf("phase = %d, want repoAddPhaseInput", m.phase)
	}
	if m.sourceMode != sourceModeOpen {
		t.Fatalf("sourceMode = %d, want sourceModeOpen (%d)", m.sourceMode, sourceModeOpen)
	}
	if m.form == nil {
		t.Fatal("expected form to be built for input phase")
	}
}

func TestRepoAdd_SourceTable_SelectClone(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	// Move down to Clone row, then press enter.
	m = sendRepoAddMsg(t, m, keyPress('j'))
	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.phase != repoAddPhaseInput {
		t.Fatalf("phase = %d, want repoAddPhaseInput", m.phase)
	}
	if m.sourceMode != sourceModeClone {
		t.Fatalf("sourceMode = %d, want sourceModeClone (%d)", m.sourceMode, sourceModeClone)
	}
}

func TestRepoAdd_InputPhase_EscReturnsToSource(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	// Select open mode.
	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.phase != repoAddPhaseInput {
		t.Fatalf("phase = %d, want repoAddPhaseInput", m.phase)
	}

	// Press esc to go back to source.
	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.phase != repoAddPhaseSource {
		t.Fatalf("phase = %d, want repoAddPhaseSource", m.phase)
	}
	if m.Cancelled() {
		t.Error("expected not cancelled — should return to source, not exit")
	}
}

func TestRepoAdd_OpenMode_ValidatesPath(t *testing.T) {
	sc := &repoAddStubClient{
		validateResp: &pb.ValidateRepoPathResponse{
			IsValid:       true,
			IsGithub:      true,
			DefaultBranch: "main",
		},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseInput
	m.fd.localPath = "/my/repo"
	m.sourceMode = sourceModeOpen

	result, cmd := m.handleFormCompleted()
	rm := result.(RepoAddModel)

	if !rm.validating {
		t.Error("expected validating=true after open mode form completion")
	}

	// Execute the validation command.
	if cmd == nil {
		t.Fatal("expected non-nil cmd for validation")
	}
	msg := cmd()
	vMsg, ok := msg.(repoValidatedMsg)
	if !ok {
		t.Fatalf("expected repoValidatedMsg, got %T", msg)
	}
	if vMsg.err != nil {
		t.Fatalf("unexpected validation error: %v", vMsg.err)
	}
	if sc.validatePath != "/my/repo" {
		t.Fatalf("ValidateRepoPath path = %q, want %q", sc.validatePath, "/my/repo")
	}
}

func TestRepoAdd_OpenMode_ValidationSuccess_AdvancesToDetails(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.fd.localPath = "/my/repo"

	// Send a successful validation response with no origin URL.
	m = sendRepoAddMsg(t, m, repoValidatedMsg{
		resp: &pb.ValidateRepoPathResponse{
			IsValid:       true,
			IsGithub:      false,
			DefaultBranch: "develop",
		},
	})

	if m.phase != repoAddPhaseDetails {
		t.Fatalf("phase = %d, want repoAddPhaseDetails (%d)", m.phase, repoAddPhaseDetails)
	}
	if m.detectedBaseBranch != "develop" {
		t.Fatalf("detectedBaseBranch = %q, want %q", m.detectedBaseBranch, "develop")
	}
	if m.fd.name != "repo" {
		t.Fatalf("name = %q, want %q (basename of path when no origin URL)", m.fd.name, "repo")
	}
}

func TestRepoAdd_OpenMode_NameFromOriginURL(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.fd.localPath = "/some/deep/worktree/path"

	// Origin URL should be preferred over directory basename.
	m = sendRepoAddMsg(t, m, repoValidatedMsg{
		resp: &pb.ValidateRepoPathResponse{
			IsValid:       true,
			IsGithub:      true,
			OriginUrl:     "https://github.com/owner/my-cool-project.git",
			DefaultBranch: "main",
		},
	})

	if m.fd.name != "@owner/my-cool-project" {
		t.Fatalf("name = %q, want %q (from origin URL)", m.fd.name, "@owner/my-cool-project")
	}
}

func TestRepoAdd_OpenMode_ValidationFailure_ShowsError(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	m = sendRepoAddMsg(t, m, repoValidatedMsg{
		resp: &pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: "not a git repo",
		},
	})

	if m.err == nil {
		t.Fatal("expected error after validation failure")
	}
	if m.err.Error() != "not a git repo" {
		t.Fatalf("err = %q, want %q", m.err.Error(), "not a git repo")
	}
	// Should go back to input phase so user can fix path.
	if m.phase != repoAddPhaseInput {
		t.Fatalf("phase = %d, want repoAddPhaseInput (%d)", m.phase, repoAddPhaseInput)
	}
}

func TestRepoAdd_OpenMode_ValidationRPCError(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	m = sendRepoAddMsg(t, m, repoValidatedMsg{
		err: fmt.Errorf("connection refused"),
	})

	if m.err == nil || m.err.Error() != "connection refused" {
		t.Fatalf("err = %v, want 'connection refused'", m.err)
	}
}

func TestRepoAdd_CloneMode_AdvancesToDetails(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseInput
	m.sourceMode = sourceModeClone
	m.fd.gitURL = "https://github.com/owner/my-repo.git"

	result, _ := m.handleFormCompleted()
	rm := result.(RepoAddModel)

	if rm.phase != repoAddPhaseDetails {
		t.Fatalf("phase = %d, want repoAddPhaseDetails (%d)", rm.phase, repoAddPhaseDetails)
	}
	if rm.fd.name != "@owner/my-repo" {
		t.Fatalf("name = %q, want %q", rm.fd.name, "@owner/my-repo")
	}
}

func TestRepoAdd_DetailsPhase_RegistersRepo(t *testing.T) {
	sc := &repoAddStubClient{
		registered: &pb.Repo{Id: "repo-1", DisplayName: "test-repo"},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.fd.localPath = "/my/repo"
	m.fd.name = "test-repo"
	m.fd.confirm = true
	m.detectedBaseBranch = "main"

	result, cmd := m.handleFormCompleted()
	rm := result.(RepoAddModel)

	// Should not be cancelled.
	if rm.Cancelled() {
		t.Error("expected not cancelled when confirm=true")
	}
	if cmd == nil {
		t.Fatal("expected non-nil cmd for registration")
	}

	// Execute the registration command.
	msg := cmd()
	regMsg, ok := msg.(repoRegisteredMsg)
	if !ok {
		t.Fatalf("expected repoRegisteredMsg, got %T", msg)
	}
	if regMsg.err != nil {
		t.Fatalf("unexpected registration error: %v", regMsg.err)
	}
	if sc.registerReq.DisplayName != "test-repo" {
		t.Fatalf("RegisterRepo name = %q, want %q", sc.registerReq.DisplayName, "test-repo")
	}
}

func TestRepoAdd_DetailsPhase_CloneMode_ClonesRepo(t *testing.T) {
	sc := &repoAddStubClient{
		cloned: &pb.Repo{Id: "repo-1", DisplayName: "my-repo"},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeClone
	m.fd.gitURL = "https://github.com/owner/my-repo.git"
	m.fd.clonePath = "/home/user/Code/my-repo"
	m.fd.name = "my-repo"
	m.fd.confirm = true

	result, cmd := m.handleFormCompleted()
	rm := result.(RepoAddModel)
	if rm.Cancelled() {
		t.Error("expected not cancelled")
	}
	if cmd == nil {
		t.Fatal("expected non-nil cmd for clone")
	}

	msg := cmd()
	cloneMsg, ok := msg.(repoClonedMsg)
	if !ok {
		t.Fatalf("expected repoClonedMsg, got %T", msg)
	}
	if cloneMsg.err != nil {
		t.Fatalf("unexpected clone error: %v", cloneMsg.err)
	}
	if sc.cloneReq.CloneUrl != "https://github.com/owner/my-repo.git" {
		t.Fatalf("CloneAndRegisterRepo URL = %q, want %q", sc.cloneReq.CloneUrl, "https://github.com/owner/my-repo.git")
	}
	if sc.cloneReq.LocalPath != "/home/user/Code/my-repo" {
		t.Fatalf("CloneAndRegisterRepo path = %q, want %q", sc.cloneReq.LocalPath, "/home/user/Code/my-repo")
	}
}

func TestRepoAdd_DetailsPhase_ConfirmNo_Cancels(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.fd.confirm = false

	result, _ := m.handleFormCompleted()
	rm := result.(RepoAddModel)

	if !rm.Cancelled() {
		t.Error("expected cancelled when confirm=false")
	}
}

func TestRepoAdd_RegisteredMsg_SetsDone(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	m = sendRepoAddMsg(t, m, repoRegisteredMsg{
		repo: &pb.Repo{Id: "repo-1", DisplayName: "test"},
	})

	if !m.Done() {
		t.Error("expected Done()=true after repoRegisteredMsg")
	}
	if m.createdRepo == nil || m.createdRepo.Id != "repo-1" {
		t.Fatalf("createdRepo = %v, want repo-1", m.createdRepo)
	}
}

func TestRepoAdd_RegisteredMsg_SignedInGitHubRepoPromptsGitHubAppInstall(t *testing.T) {
	sc := &repoAddStubClient{}
	gh := &fakeGitHubAppClient{installURL: "https://github.com/apps/bossanova-dev/installations/new"}
	m := NewRepoAddModel(sc, context.Background())
	m.SetGitHubAppInstall(signedInRepoAddAuth(), gh)
	m.detectedOriginURL = "https://github.com/acme/widgets.git"

	updated, cmd := m.Update(repoRegisteredMsg{
		repo: &pb.Repo{Id: "repo-1", DisplayName: "@acme/widgets"},
	})
	rm := updated.(RepoAddModel)

	if rm.Done() {
		t.Fatal("expected install follow-up before Done()")
	}
	if rm.phase != repoAddPhaseGitHubAppPrompt {
		t.Fatalf("phase = %d, want repoAddPhaseGitHubAppPrompt", rm.phase)
	}
	if rm.githubAppInstallNWO != "acme/widgets" {
		t.Fatalf("githubAppInstallNWO = %q, want acme/widgets", rm.githubAppInstallNWO)
	}
	if cmd != nil {
		t.Fatal("expected no install URL command before user accepts prompt")
	}
	if gh.installURLCalls != 0 {
		t.Fatalf("install URL calls = %d, want 0 before prompt accepted", gh.installURLCalls)
	}
	if view := rm.View().Content; !strings.Contains(view, "Install the Bossanova Github App on acme/widgets?") ||
		!strings.Contains(view, "This enables automated realtime response to conflicts, failing tests and code reviews.") ||
		!strings.Contains(view, "You can always set this up later if you don't want to decide yet.") ||
		!strings.Contains(view, "[enter] install  [esc] skip") {
		t.Fatalf("prompt view = %q", view)
	}
}

func TestRepoAdd_GitHubAppPrompt_EscSkipsInstall(t *testing.T) {
	sc := &repoAddStubClient{}
	gh := &fakeGitHubAppClient{installURL: "https://github.com/apps/bossanova-dev/installations/new"}
	m := NewRepoAddModel(sc, context.Background())
	m.SetGitHubAppInstall(signedInRepoAddAuth(), gh)
	m.detectedOriginURL = "https://github.com/acme/widgets.git"
	m = sendRepoAddMsg(t, m, repoRegisteredMsg{repo: &pb.Repo{Id: "repo-1", DisplayName: "@acme/widgets"}})

	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if !m.Done() {
		t.Fatal("expected skip to complete add flow")
	}
	if gh.installURLCalls != 0 {
		t.Fatalf("install URL calls = %d, want 0 after skip", gh.installURLCalls)
	}
}

func TestRepoAdd_GitHubAppPrompt_EnterStartsGitHubAppInstall(t *testing.T) {
	sc := &repoAddStubClient{}
	gh := &fakeGitHubAppClient{installURL: "https://github.com/apps/bossanova-dev/installations/new"}
	m := NewRepoAddModel(sc, context.Background())
	m.SetGitHubAppInstall(signedInRepoAddAuth(), gh)
	m.detectedOriginURL = "https://github.com/acme/widgets.git"
	m = sendRepoAddMsg(t, m, repoRegisteredMsg{repo: &pb.Repo{Id: "repo-1", DisplayName: "@acme/widgets"}})

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	rm := updated.(RepoAddModel)

	if rm.phase != repoAddPhaseGitHubApp {
		t.Fatalf("phase = %d, want repoAddPhaseGitHubApp", rm.phase)
	}
	if rm.Done() {
		t.Fatal("expected install wait screen before Done()")
	}
	if cmd == nil {
		t.Fatal("expected install URL command")
	}
}

func TestRepoAdd_GitHubAppPrompt_InstallURLFailureStaysInInstallFlow(t *testing.T) {
	sc := &repoAddStubClient{}
	gh := &fakeGitHubAppClient{installErr: fmt.Errorf("install URL unavailable")}
	m := NewRepoAddModel(sc, context.Background())
	m.SetGitHubAppInstall(signedInRepoAddAuth(), gh)
	m.detectedOriginURL = "https://github.com/acme/widgets.git"
	m = sendRepoAddMsg(t, m, repoRegisteredMsg{repo: &pb.Repo{Id: "repo-1", DisplayName: "@acme/widgets"}})

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	rm := updated.(RepoAddModel)
	if cmd == nil {
		t.Fatal("expected install URL command")
	}
	updated, followup := rm.Update(rm.requestGitHubAppInstallURL(rm.githubAppAttempt)())
	rm = updated.(RepoAddModel)

	if followup != nil {
		t.Fatal("unexpected follow-up command after install URL failure")
	}
	if rm.Done() {
		t.Fatal("install URL failure should not silently complete repo add")
	}
	if rm.phase != repoAddPhaseGitHubApp {
		t.Fatalf("phase = %d, want repoAddPhaseGitHubApp", rm.phase)
	}
	if rm.githubAppInstallErr == nil || !strings.Contains(rm.githubAppInstallErr.Error(), "install URL unavailable") {
		t.Fatalf("githubAppInstallErr = %v, want install URL error", rm.githubAppInstallErr)
	}
	if gh.installURLCalls != 1 {
		t.Fatalf("install URL calls = %d, want 1", gh.installURLCalls)
	}
	if view := rm.View().Content; !strings.Contains(view, "Could not start GitHub App installation: install URL unavailable") {
		t.Fatalf("view did not surface install URL failure:\n%s", view)
	}
}

func TestRepoAdd_RegisteredMsg_UnsignedSkipsGitHubAppInstall(t *testing.T) {
	sc := &repoAddStubClient{}
	gh := &fakeGitHubAppClient{installURL: "https://github.com/apps/bossanova-dev/installations/new"}
	m := NewRepoAddModel(sc, context.Background())
	m.SetGitHubAppInstall(auth.NewManager(&repoAddTokenStore{}, auth.Config{}), gh)
	m.detectedOriginURL = "https://github.com/acme/widgets.git"

	m = sendRepoAddMsg(t, m, repoRegisteredMsg{
		repo: &pb.Repo{Id: "repo-1", DisplayName: "@acme/widgets"},
	})

	if !m.Done() {
		t.Fatal("expected unsigned registration to complete without GitHub App follow-up")
	}
	if m.phase == repoAddPhaseGitHubApp {
		t.Fatal("unexpected GitHub App install phase")
	}
}

func TestRepoAdd_GitHubAppReposMsg_CompletesWhenRepoInstalled(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseGitHubApp
	m.githubAppAttempt = 3
	m.githubAppInstallNWO = "acme/widgets"

	m = sendRepoAddMsg(t, m, githubAppReposMsg{
		attempt: 3,
		repos: []*pb.GitHubAppRepoStatus{{
			Owner:     "acme",
			Name:      "widgets",
			Installed: true,
		}},
	})

	if !m.Done() {
		t.Fatal("expected installed repo to complete follow-up")
	}
}

func TestRepoAdd_GitHubAppReposMsg_KeepsWaitingUntilTargetRepoInstalled(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseGitHubApp
	m.githubAppAttempt = 3
	m.githubAppInstallNWO = "acme/widgets"

	updated, cmd := m.Update(githubAppReposMsg{
		attempt: 3,
		repos: []*pb.GitHubAppRepoStatus{{
			Owner:     "acme",
			Name:      "other",
			Installed: true,
		}},
	})
	rm := updated.(RepoAddModel)

	if rm.Done() {
		t.Fatal("expected unrelated installed repo to keep waiting")
	}
	if cmd == nil {
		t.Fatal("expected next poll command")
	}
}

func TestRepoAdd_RegisteredMsg_Error(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	m = sendRepoAddMsg(t, m, repoRegisteredMsg{
		err: fmt.Errorf("duplicate repo"),
	})

	if m.Done() {
		t.Error("expected Done()=false after error")
	}
	if m.err == nil || m.err.Error() != "duplicate repo" {
		t.Fatalf("err = %v, want 'duplicate repo'", m.err)
	}
}

func TestRepoAdd_ClonedMsg_SetsDone(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.cloning = true

	m = sendRepoAddMsg(t, m, repoClonedMsg{
		repo: &pb.Repo{Id: "repo-1", DisplayName: "cloned"},
	})

	if !m.Done() {
		t.Error("expected Done()=true after repoClonedMsg")
	}
	if m.cloning {
		t.Error("expected cloning=false after success")
	}
}

func TestRepoAdd_EscCancels(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if !m.Cancelled() {
		t.Error("expected Cancelled()=true after esc")
	}
}

func TestRepoAdd_SetupScriptPassedToRegister(t *testing.T) {
	sc := &repoAddStubClient{
		registered: &pb.Repo{Id: "repo-1"},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.fd.localPath = "/my/repo"
	m.fd.name = "test"
	m.fd.setup = "./setup.sh"
	m.fd.confirm = true

	_, cmd := m.handleFormCompleted()
	cmd()

	if sc.registerReq.SetupScript == nil || *sc.registerReq.SetupScript != "./setup.sh" {
		t.Fatalf("SetupScript = %v, want './setup.sh'", sc.registerReq.SetupScript)
	}
}

func TestRepoAdd_SetupScriptPassedToClone(t *testing.T) {
	sc := &repoAddStubClient{
		cloned: &pb.Repo{Id: "repo-1"},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeClone
	m.fd.gitURL = "https://github.com/owner/repo.git"
	m.fd.clonePath = "/home/user/Code/repo"
	m.fd.name = "repo"
	m.fd.setup = "make setup"
	m.fd.confirm = true

	_, cmd := m.handleFormCompleted()
	cmd()

	if sc.cloneReq.SetupScript == nil || *sc.cloneReq.SetupScript != "make setup" {
		t.Fatalf("SetupScript = %v, want 'make setup'", sc.cloneReq.SetupScript)
	}
}

func TestRepoAdd_DetailsForm_SeedsCreationDefaults(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	if m.fd.mergeStrategy != defaultRepoMergeStrategy {
		t.Fatalf("mergeStrategy = %q, want %q", m.fd.mergeStrategy, defaultRepoMergeStrategy)
	}
	want := defaultRepoAutomation()
	if len(m.fd.automation) != len(want) {
		t.Fatalf("automation = %v, want %v", m.fd.automation, want)
	}
	for _, v := range want {
		if !slices.Contains(m.fd.automation, v) {
			t.Fatalf("automation %v missing default %q", m.fd.automation, v)
		}
	}
	// The seeded defaults must equal the booleans bossd applies at creation, or
	// the WYSIWYG "only persist changes" logic would drift.
	if slices.Contains(want, automationAutoMerge) != defaultCanAutoMerge {
		t.Fatal("auto-merge default seed disagrees with defaultCanAutoMerge")
	}
	if slices.Contains(want, automationDependabot) != defaultCanAutoMergeDependabot {
		t.Fatal("dependabot default seed disagrees with defaultCanAutoMergeDependabot")
	}
	if slices.Contains(want, automationRepair) != defaultCanAutoRepair {
		t.Fatal("repair default seed disagrees with defaultCanAutoRepair")
	}
}

func TestRepoAdd_DetailsForm_RendersConfigFields(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	m.form.Init()

	view := m.View().Content
	for _, want := range []string{
		"Name", "Setup command", "Merge strategy", "Automation",
		"Add Repository", "Cancel",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("details form view missing %q; got:\n%s", want, view)
		}
	}
	// Integrations (Linear/Sentry) are configured on the repo-settings screen,
	// not the add form, so they must not appear here.
	for _, absent := range []string{"Linear API key", "Sentry API key", "Sentry organization"} {
		if strings.Contains(view, absent) {
			t.Fatalf("details form unexpectedly includes integration field %q; got:\n%s", absent, view)
		}
	}
}

// TestRepoAdd_DetailsForm_MarksExactlyOneFocusedField is the user-visible half
// of the focus affordance: on a 5-field form exactly one field may paint the
// focus gutter. A field spans several lines, so the invariant is one contiguous
// *run* of gutter lines, not one gutter character.
func TestRepoAdd_DetailsForm_MarksExactlyOneFocusedField(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = updated.(RepoAddModel)

	view := m.View().Content
	if runs := gutterRuns(view); runs != 1 {
		t.Fatalf("details form paints %d focus-gutter runs, want exactly 1 (one focused field at a time); view:\n%s", runs, view)
	}

	// Moving focus must move the gutter, not add a second one. huh advances the
	// field via a returned nextFieldMsg command, so the command has to be run
	// and fed back before the move is observable.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = updated.(RepoAddModel)
	if cmd == nil {
		t.Fatal("tab produced no command; huh did not schedule a field advance")
	}
	updated, _ = m.Update(cmd())
	m = updated.(RepoAddModel)

	moved := m.View().Content
	if runs := gutterRuns(moved); runs != 1 {
		t.Fatalf("after tab the details form paints %d focus-gutter runs, want exactly 1; view:\n%s", runs, moved)
	}
	if slices.Equal(gutterLines(moved), gutterLines(view)) {
		t.Errorf("the focus gutter did not move after tab; it sat on lines %v both before and after", gutterLines(view))
	}
}

// gutterLines reports the indexes of the rendered lines carrying the focus gutter.
func gutterLines(view string) []int {
	var lines []int
	for i, line := range strings.Split(view, "\n") {
		if strings.ContainsRune(line, focusGutterGlyph) {
			lines = append(lines, i)
		}
	}
	return lines
}

// gutterRuns counts contiguous groups of gutter lines — i.e. focused fields.
func gutterRuns(view string) int {
	runs, prev := 0, -2
	for _, i := range gutterLines(view) {
		if i != prev+1 {
			runs++
		}
		prev = i
	}
	return runs
}

func TestRepoConfigUpdate(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(fd *repoAddFormData)
		wantChanged bool
		check       func(t *testing.T, req *pb.UpdateRepoRequest)
	}{
		{
			name:        "defaults produce no update",
			mutate:      func(*repoAddFormData) {},
			wantChanged: false,
		},
		{
			name:        "changed merge strategy",
			mutate:      func(fd *repoAddFormData) { fd.mergeStrategy = "squash" },
			wantChanged: true,
			check: func(t *testing.T, req *pb.UpdateRepoRequest) {
				if req.MergeStrategy == nil || *req.MergeStrategy != "squash" {
					t.Fatalf("MergeStrategy = %v, want squash", req.MergeStrategy)
				}
			},
		},
		{
			name:        "enabling auto-merge",
			mutate:      func(fd *repoAddFormData) { fd.automation = append(fd.automation, automationAutoMerge) },
			wantChanged: true,
			check: func(t *testing.T, req *pb.UpdateRepoRequest) {
				if req.CanAutoMerge == nil || !*req.CanAutoMerge {
					t.Fatalf("CanAutoMerge = %v, want true", req.CanAutoMerge)
				}
			},
		},
		{
			name: "disabling automatic repair",
			mutate: func(fd *repoAddFormData) {
				fd.automation = slices.DeleteFunc(fd.automation, func(v string) bool { return v == automationRepair })
			},
			wantChanged: true,
			check: func(t *testing.T, req *pb.UpdateRepoRequest) {
				if req.CanAutoRepair == nil || *req.CanAutoRepair {
					t.Fatalf("CanAutoRepair = %v, want false", req.CanAutoRepair)
				}
			},
		},
		{
			name:        "disabling all automation",
			mutate:      func(fd *repoAddFormData) { fd.automation = nil },
			wantChanged: true,
			check: func(t *testing.T, req *pb.UpdateRepoRequest) {
				if req.CanAutoMergeDependabot == nil || *req.CanAutoMergeDependabot {
					t.Fatalf("CanAutoMergeDependabot = %v, want false", req.CanAutoMergeDependabot)
				}
				if req.CanAutoRepair == nil || *req.CanAutoRepair {
					t.Fatalf("CanAutoRepair = %v, want false", req.CanAutoRepair)
				}
			},
		},
		{
			name: "merge strategy plus automation change together",
			mutate: func(fd *repoAddFormData) {
				fd.mergeStrategy = "rebase"
				fd.automation = []string{automationDependabot} // drop repair
			},
			wantChanged: true,
			check: func(t *testing.T, req *pb.UpdateRepoRequest) {
				if req.MergeStrategy == nil || *req.MergeStrategy != "rebase" {
					t.Fatalf("MergeStrategy = %v, want rebase", req.MergeStrategy)
				}
				if req.CanAutoRepair == nil || *req.CanAutoRepair {
					t.Fatalf("CanAutoRepair = %v, want false", req.CanAutoRepair)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fd := &repoAddFormData{
				mergeStrategy: defaultRepoMergeStrategy,
				automation:    defaultRepoAutomation(),
			}
			tt.mutate(fd)

			req, changed := repoConfigUpdate("repo-1", fd.configSnapshot())
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v (req=%+v)", changed, tt.wantChanged, req)
			}
			if changed && req.Id != "repo-1" {
				t.Fatalf("req.Id = %q, want repo-1", req.Id)
			}
			if tt.check != nil {
				tt.check(t, req)
			}
		})
	}
}

// TestRepoConfigSnapshot_DecoupledFromLaterMutation guards the fix for the
// post-create config race: submitRepo snapshots the add-form options before the
// async create RPC, so edits made to the live form while the RPC is in flight
// must not leak into the values persisted for the new repo.
func TestRepoConfigSnapshot_DecoupledFromLaterMutation(t *testing.T) {
	fd := &repoAddFormData{
		mergeStrategy: "rebase",
		automation:    []string{automationAutoMerge, automationDependabot},
	}

	snap := fd.configSnapshot()

	// Simulate the view rebuilding form state while the create RPC is in flight.
	fd.mergeStrategy = "squash"
	fd.automation[0] = automationDependabot
	fd.automation = append(fd.automation, automationRepair)

	req, changed := repoConfigUpdate("repo-1", snap)
	if !changed {
		t.Fatal("expected changed = true")
	}
	if req.MergeStrategy == nil || *req.MergeStrategy != "rebase" {
		t.Fatalf("MergeStrategy = %v, want rebase (snapshot leaked later edit)", req.MergeStrategy)
	}
	if req.CanAutoMerge == nil || !*req.CanAutoMerge {
		t.Fatalf("CanAutoMerge = %v, want true (snapshot lost submitted value)", req.CanAutoMerge)
	}
	if req.CanAutoRepair == nil || *req.CanAutoRepair {
		t.Fatalf("CanAutoRepair = %v, want false (appended edit leaked)", req.CanAutoRepair)
	}
}

func TestRepoAdd_DetailsPhase_ChangedConfigUpdatesRepoAfterRegister(t *testing.T) {
	sc := &repoAddStubClient{
		registered: &pb.Repo{Id: "repo-1", DisplayName: "test-repo"},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.fd.localPath = "/my/repo"
	m.fd.name = "test-repo"
	m.fd.confirm = true
	m.detectedBaseBranch = "main"
	// User changed merge strategy and enabled auto-merge on the add form.
	m.fd.mergeStrategy = "rebase"
	m.fd.automation = append(m.fd.automation, automationAutoMerge)

	_, cmd := m.handleFormCompleted()
	if cmd == nil {
		t.Fatal("expected registration command")
	}
	msg := cmd()
	regMsg, ok := msg.(repoRegisteredMsg)
	if !ok {
		t.Fatalf("expected repoRegisteredMsg, got %T", msg)
	}
	if regMsg.err != nil || regMsg.configErr != nil {
		t.Fatalf("unexpected errors: err=%v configErr=%v", regMsg.err, regMsg.configErr)
	}
	if sc.registerReq == nil {
		t.Fatal("RegisterRepo was not called")
	}
	if sc.updateReq == nil {
		t.Fatal("UpdateRepo was not called for changed config")
	}
	if sc.updateReq.Id != "repo-1" {
		t.Fatalf("UpdateRepo.Id = %q, want repo-1", sc.updateReq.Id)
	}
	if sc.updateReq.MergeStrategy == nil || *sc.updateReq.MergeStrategy != "rebase" {
		t.Fatalf("UpdateRepo.MergeStrategy = %v, want rebase", sc.updateReq.MergeStrategy)
	}
	if sc.updateReq.CanAutoMerge == nil || !*sc.updateReq.CanAutoMerge {
		t.Fatalf("UpdateRepo.CanAutoMerge = %v, want true", sc.updateReq.CanAutoMerge)
	}
}

func TestRepoAdd_DetailsPhase_DefaultConfigSkipsUpdate(t *testing.T) {
	sc := &repoAddStubClient{
		registered: &pb.Repo{Id: "repo-1", DisplayName: "test-repo"},
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.fd.localPath = "/my/repo"
	m.fd.name = "test-repo"
	m.fd.confirm = true

	_, cmd := m.handleFormCompleted()
	msg := cmd()
	if _, ok := msg.(repoRegisteredMsg); !ok {
		t.Fatalf("expected repoRegisteredMsg, got %T", msg)
	}
	if sc.updateReq != nil {
		t.Fatalf("UpdateRepo should not be called when config matches defaults; got %+v", sc.updateReq)
	}
}

func TestRepoAdd_ConfigUpdateFailure_ShowsNonFatalNotice(t *testing.T) {
	sc := &repoAddStubClient{
		registered: &pb.Repo{Id: "repo-1", DisplayName: "test-repo"},
		updateErr:  fmt.Errorf("settings write failed"),
	}
	m := NewRepoAddModel(sc, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.fd.localPath = "/my/repo"
	m.fd.name = "test-repo"
	m.fd.confirm = true
	m.fd.mergeStrategy = "squash" // force an UpdateRepo call

	_, cmd := m.handleFormCompleted()
	msg := cmd()
	regMsg := msg.(repoRegisteredMsg)
	if regMsg.err != nil {
		t.Fatalf("register err should be nil (repo created); got %v", regMsg.err)
	}
	if regMsg.configErr == nil {
		t.Fatal("expected configErr when UpdateRepo fails")
	}

	// Feeding the message in must not mark the wizard done, and must surface the
	// notice. esc then completes the add (the repo exists).
	m = sendRepoAddMsg(t, m, regMsg)
	if m.Done() {
		t.Fatal("config failure should not auto-complete the wizard")
	}
	if view := m.View().Content; !strings.Contains(view, "saving its options failed") ||
		!strings.Contains(view, "settings write failed") {
		t.Fatalf("expected non-fatal config notice; got:\n%s", view)
	}
	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.Done() {
		t.Fatal("esc on the config notice should complete the add")
	}
}

func TestRepoAdd_DetailsPhase_EscReturnsToInput(t *testing.T) {
	sc := &repoAddStubClient{}
	m := NewRepoAddModel(sc, context.Background())

	// Advance to details phase.
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()

	// Press esc — should go back to input, not cancel.
	m = sendRepoAddMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if m.phase != repoAddPhaseInput {
		t.Fatalf("phase = %d, want repoAddPhaseInput (%d)", m.phase, repoAddPhaseInput)
	}
	if m.Cancelled() {
		t.Error("expected not cancelled — should return to input, not exit")
	}
	if m.form == nil {
		t.Error("expected form to be rebuilt for input phase")
	}
}
