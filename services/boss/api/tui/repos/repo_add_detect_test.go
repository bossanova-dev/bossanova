package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const testPnpmSetupSpec = `{"type":"command","argv":["pnpm","install","--frozen-lockfile"]}`

func testPnpmDetection() *pb.DetectSetupScriptResponse {
	return &pb.DetectSetupScriptResponse{
		SetupScript:    testPnpmSetupSpec,
		DisplayCommand: "pnpm install --frozen-lockfile",
		Source:         "pnpm-lock.yaml",
	}
}

const testPnpmRepoPath = "/my/pnpm-repo"

func advanceOpenValidation(t *testing.T, sc *repoAddStubClient) RepoAddModel {
	t.Helper()
	m := NewRepoAddModel(sc, context.Background())
	m.SetGitHubActions(fakeGitHubActions{})
	m.phase = repoAddPhaseInput
	m.sourceMode = sourceModeOpen
	m.fd.localPath = testPnpmRepoPath

	result, cmd := m.handleFormCompleted()
	m = result.(RepoAddModel)
	if cmd == nil {
		t.Fatal("expected non-nil validation command")
	}
	msg := cmd()
	updated, initCmd := m.Update(msg)
	m = updated.(RepoAddModel)
	if initCmd != nil {
		initCmd()
	}
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return sized.(RepoAddModel)
}

func TestRepoAdd_DetectOpenPrefill_RepoSettings_AC271301(t *testing.T) {
	sc := &repoAddStubClient{
		validateResp: &pb.ValidateRepoPathResponse{
			IsValid:       true,
			IsGithub:      true,
			DefaultBranch: "main",
		},
		detectResp: testPnpmDetection(),
	}

	m := advanceOpenValidation(t, sc)

	if sc.validatePath != "/my/pnpm-repo" {
		t.Fatalf("ValidateRepoPath path = %q, want %q", sc.validatePath, "/my/pnpm-repo")
	}
	if sc.detectPath != "/my/pnpm-repo" {
		t.Fatalf("DetectSetupScript path = %q, want %q", sc.detectPath, "/my/pnpm-repo")
	}
	if m.phase != repoAddPhaseDetails {
		t.Fatalf("phase = %d, want repoAddPhaseDetails (%d)", m.phase, repoAddPhaseDetails)
	}
	if got, want := m.fd.setup, "pnpm install --frozen-lockfile"; got != want {
		t.Fatalf("fd.setup = %q, want %q", got, want)
	}

	view := m.View().Content
	if !strings.Contains(view, "pnpm install --frozen-lockfile") {
		t.Errorf("details view missing pre-filled setup command; view:\n%s", view)
	}
	wantDesc := "Detected from pnpm-lock.yaml. Clear it to skip setup."
	if !strings.Contains(view, wantDesc) {
		t.Errorf("details view missing %q; view:\n%s", wantDesc, view)
	}

	// Re-validating a second repository with no detection must clear the stale pre-fill.
	sc.detectResp = &pb.DetectSetupScriptResponse{}
	m.phase = repoAddPhaseInput
	m.fd.localPath = "/my/plain-repo"
	result, cmd := m.handleFormCompleted()
	m = result.(RepoAddModel)
	updated, initCmd := m.Update(cmd())
	m = updated.(RepoAddModel)
	if initCmd != nil {
		initCmd()
	}
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = sized.(RepoAddModel)
	if m.fd.setup != "" {
		t.Errorf("fd.setup after plain repo validation = %q, want empty", m.fd.setup)
	}
	if strings.Contains(m.View().Content, "Detected from") {
		t.Errorf("details view still shows stale 'Detected from' line; view:\n%s", m.View().Content)
	}
}

func TestRepoAdd_DetectAcceptOrEdit_RepoSettings_AC271302(t *testing.T) {
	t.Run("accept unchanged sends structured spec JSON", func(t *testing.T) {
		sc := &repoAddStubClient{
			validateResp: &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"},
			detectResp:   testPnpmDetection(),
			registered:   &pb.Repo{Id: "repo-1", DisplayName: "pnpm-repo"},
		}
		m := advanceOpenValidation(t, sc)
		m.fd.confirm = true

		_, cmd := m.handleFormCompleted()
		if cmd == nil {
			t.Fatal("expected non-nil register cmd")
		}
		cmd()

		if sc.registerReq == nil {
			t.Fatal("RegisterRepo was not called")
		}
		if got := sc.registerReq.GetSetupScript(); got != testPnpmSetupSpec {
			t.Fatalf("RegisterRepo.SetupScript = %q, want %q", got, testPnpmSetupSpec)
		}
		if sc.registerReq.ShouldDetectSetupScript != nil {
			t.Fatalf("RegisterRepo.ShouldDetectSetupScript = %v, want nil", *sc.registerReq.ShouldDetectSetupScript)
		}
	})

	t.Run("editing pre-filled command sends typed text", func(t *testing.T) {
		sc := &repoAddStubClient{
			validateResp: &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"},
			detectResp:   testPnpmDetection(),
			registered:   &pb.Repo{Id: "repo-1", DisplayName: "pnpm-repo"},
		}
		m := advanceOpenValidation(t, sc)
		m.fd.setup = "make custom-setup"
		m.fd.confirm = true

		_, cmd := m.handleFormCompleted()
		cmd()

		if sc.registerReq == nil {
			t.Fatal("RegisterRepo was not called")
		}
		if sc.registerReq.SetupScript == nil || *sc.registerReq.SetupScript != "make custom-setup" {
			t.Fatalf("RegisterRepo.SetupScript = %v, want %q", sc.registerReq.SetupScript, "make custom-setup")
		}
		if sc.registerReq.ShouldDetectSetupScript != nil {
			t.Fatalf("RegisterRepo.ShouldDetectSetupScript = %v, want nil", *sc.registerReq.ShouldDetectSetupScript)
		}
	})

	t.Run("revalidating open flow preserves custom command", func(t *testing.T) {
		sc := &repoAddStubClient{
			validateResp: &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"},
			detectResp:   testPnpmDetection(),
			registered:   &pb.Repo{Id: "repo-1", DisplayName: "pnpm-repo"},
		}
		m := advanceOpenValidation(t, sc)
		m.fd.setup = "make custom-setup"

		m.phase = repoAddPhaseInput
		m.buildInputForm()
		result, cmd := m.handleFormCompleted()
		m = result.(RepoAddModel)
		if cmd == nil {
			t.Fatal("expected non-nil validation command")
		}
		updated, initCmd := m.Update(cmd())
		m = updated.(RepoAddModel)
		if initCmd != nil {
			initCmd()
		}
		sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		m = sized.(RepoAddModel)

		if got, want := m.fd.setup, "make custom-setup"; got != want {
			t.Fatalf("fd.setup after revalidation = %q, want %q", got, want)
		}
		if strings.Contains(m.View().Content, "Detected from") {
			t.Errorf("details view should not show 'Detected from' over custom command; view:\n%s", m.View().Content)
		}

		m.fd.confirm = true
		_, regCmd := m.handleFormCompleted()
		regCmd()
		if sc.registerReq == nil {
			t.Fatal("RegisterRepo was not called")
		}
		if sc.registerReq.SetupScript == nil || *sc.registerReq.SetupScript != "make custom-setup" {
			t.Fatalf("RegisterRepo.SetupScript = %v, want %q", sc.registerReq.SetupScript, "make custom-setup")
		}
	})

	t.Run("detection error still advances without pre-fill", func(t *testing.T) {
		sc := &repoAddStubClient{
			validateResp: &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"},
			detectErr:    errors.New("detector unavailable"),
		}
		m := advanceOpenValidation(t, sc)

		if m.phase != repoAddPhaseDetails {
			t.Fatalf("phase = %d, want repoAddPhaseDetails (%d)", m.phase, repoAddPhaseDetails)
		}
		if m.err != nil {
			t.Fatalf("unexpected error after failed detection: %v", m.err)
		}
		if m.fd.setup != "" {
			t.Fatalf("fd.setup = %q, want empty on detection error", m.fd.setup)
		}
		if strings.Contains(m.View().Content, "Detected from") {
			t.Errorf("view should not show 'Detected from' on detection error; view:\n%s", m.View().Content)
		}
	})
}

func TestRepoAdd_DetectCleared_RepoSettings_AC271303(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace only", value: "   \t  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &repoAddStubClient{
				validateResp: &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"},
				detectResp:   testPnpmDetection(),
				registered:   &pb.Repo{Id: "repo-1", DisplayName: "pnpm-repo"},
			}
			m := advanceOpenValidation(t, sc)
			if m.fd.setup != "pnpm install --frozen-lockfile" {
				t.Fatalf("precondition failed: fd.setup = %q", m.fd.setup)
			}

			m.fd.setup = tc.value
			m.fd.confirm = true

			_, cmd := m.handleFormCompleted()
			cmd()

			if sc.registerReq == nil {
				t.Fatal("RegisterRepo was not called")
			}
			if sc.registerReq.SetupScript != nil {
				t.Fatalf("RegisterRepo.SetupScript = %q, want nil", *sc.registerReq.SetupScript)
			}
			if sc.registerReq.ShouldDetectSetupScript != nil {
				t.Fatalf("RegisterRepo.ShouldDetectSetupScript = %v, want nil", *sc.registerReq.ShouldDetectSetupScript)
			}
		})
	}
}

func TestRepoAdd_DetectCloneAdopts_RepoSettings_AC271304(t *testing.T) {
	t.Run("empty field shows clone description and adopts detection", func(t *testing.T) {
		sc := &repoAddStubClient{
			cloned: &pb.Repo{Id: "repo-1", DisplayName: "@owner/my-repo"},
		}
		m := NewRepoAddModel(sc, context.Background())
		m.SetGitHubActions(fakeGitHubActions{})
		m.phase = repoAddPhaseInput
		m.sourceMode = sourceModeClone
		m.fd.gitURL = "https://github.com/owner/my-repo.git"
		m.fd.clonePath = "/tmp/my-repo"

		result, initCmd := m.handleFormCompleted()
		m = result.(RepoAddModel)
		if initCmd != nil {
			initCmd()
		}
		sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		m = sized.(RepoAddModel)

		if m.phase != repoAddPhaseDetails {
			t.Fatalf("phase = %d, want repoAddPhaseDetails (%d)", m.phase, repoAddPhaseDetails)
		}
		wantDesc := "Leave empty to use one detected after cloning."
		if !strings.Contains(m.View().Content, wantDesc) {
			t.Errorf("clone details view missing %q; view:\n%s", wantDesc, m.View().Content)
		}

		m.fd.confirm = true
		_, cmd := m.handleFormCompleted()
		cmd()

		if sc.cloneReq == nil {
			t.Fatal("CloneAndRegisterRepo was not called")
		}
		if sc.cloneReq.SetupScript != nil {
			t.Fatalf("CloneAndRegisterRepo.SetupScript = %q, want nil", *sc.cloneReq.SetupScript)
		}
		if !sc.cloneReq.GetShouldDetectSetupScript() {
			t.Fatalf("CloneAndRegisterRepo.ShouldDetectSetupScript = %v, want true", sc.cloneReq.ShouldDetectSetupScript)
		}
	})

	t.Run("non-empty field sends typed text without auto-detect", func(t *testing.T) {
		sc := &repoAddStubClient{
			cloned: &pb.Repo{Id: "repo-1", DisplayName: "@owner/my-repo"},
		}
		m := NewRepoAddModel(sc, context.Background())
		m.SetGitHubActions(fakeGitHubActions{})
		m.phase = repoAddPhaseDetails
		m.sourceMode = sourceModeClone
		m.fd.gitURL = "https://github.com/owner/my-repo.git"
		m.fd.clonePath = "/tmp/my-repo"
		m.fd.name = "@owner/my-repo"
		m.fd.setup = "make bootstrap"
		m.fd.confirm = true

		_, cmd := m.handleFormCompleted()
		cmd()

		if sc.cloneReq == nil {
			t.Fatal("CloneAndRegisterRepo was not called")
		}
		if sc.cloneReq.SetupScript == nil || *sc.cloneReq.SetupScript != "make bootstrap" {
			t.Fatalf("CloneAndRegisterRepo.SetupScript = %v, want %q", sc.cloneReq.SetupScript, "make bootstrap")
		}
		if sc.cloneReq.ShouldDetectSetupScript != nil {
			t.Fatalf("CloneAndRegisterRepo.ShouldDetectSetupScript = %v, want nil", *sc.cloneReq.ShouldDetectSetupScript)
		}
	})

	t.Run("switching from open detection to clone clears stale pre-fill", func(t *testing.T) {
		sc := &repoAddStubClient{
			validateResp: &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"},
			detectResp:   testPnpmDetection(),
			cloned:       &pb.Repo{Id: "repo-1", DisplayName: "@owner/my-repo"},
		}
		m := advanceOpenValidation(t, sc)
		if m.fd.setup != "pnpm install --frozen-lockfile" {
			t.Fatalf("precondition failed: fd.setup = %q", m.fd.setup)
		}

		m.sourceMode = sourceModeClone
		m.phase = repoAddPhaseInput
		m.buildInputForm()
		m.fd.gitURL = "https://github.com/owner/my-repo.git"
		m.fd.clonePath = "/tmp/my-repo"

		result, _ := m.handleFormCompleted()
		m = result.(RepoAddModel)
		if m.fd.setup != "" {
			t.Fatalf("fd.setup after switching to clone = %q, want empty", m.fd.setup)
		}

		m.fd.confirm = true
		_, cmd := m.handleFormCompleted()
		cmd()
		if sc.cloneReq.SetupScript != nil {
			t.Fatalf("CloneAndRegisterRepo.SetupScript = %q, want nil", *sc.cloneReq.SetupScript)
		}
		if !sc.cloneReq.GetShouldDetectSetupScript() {
			t.Fatalf("CloneAndRegisterRepo.ShouldDetectSetupScript = %v, want true", sc.cloneReq.ShouldDetectSetupScript)
		}
	})
}
