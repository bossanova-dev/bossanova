package repos

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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
	cursorToRow(t, &m, repoSettingsRowLinearAPIKey)
	updatedModel, _ = m.activateRow()
	m = updatedModel.(RepoSettingsModel)

	// Verify input is empty (not pre-filled)
	if m.linearAPIKeyInput.Value() != "" {
		t.Errorf("linearAPIKeyInput.Value() = %q, want empty string (full replace)", m.linearAPIKeyInput.Value())
	}

	// Verify editing field is set
	if m.editingField != repoSettingsRowLinearAPIKey {
		t.Errorf("editingField = %d, want %d", m.editingField, repoSettingsRowLinearAPIKey)
	}
}

// cursorToRow positions the cursor on the given rowID, expanding the owning
// integration first if the row is an integration child, then failing the test if
// the row is still not visible.
func cursorToRow(t *testing.T, m *RepoSettingsModel, row rowID) {
	t.Helper()
	switch row {
	case repoSettingsRowLinearAPIKey:
		m.linearExpanded = true
	case repoSettingsRowSentryAPIKey, repoSettingsRowSentryOrg:
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
		repoSettingsRowIsTrusted,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowShouldArchiveSessionsAfterMerge,
		repoSettingsRowCanAutoDeleteBranches,
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
		repoSettingsRowIsTrusted,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowShouldArchiveSessionsAfterMerge,
		repoSettingsRowCanAutoDeleteBranches,
		repoSettingsRowLinearHeader,
		repoSettingsRowLinearAPIKey,
		repoSettingsRowSentryHeader,
		repoSettingsRowSentryAPIKey,
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

	var openedURL string
	openURL := func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetGitHubAppInstall(gh)
	m.SetGitHubActions(fakeGitHubActions{open: openURL})
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

	var openedURL string
	openURL := func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetGitHubAppInstall(gh)
	m.SetGitHubActions(fakeGitHubActions{open: openURL})
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
	updated, _ := m.Update(RepoSettingsLoadedMsg{Err: errors.New("load failed")})
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
	archiveIdx := strings.Index(out, "Archive sessions after merging PRs")
	deleteBranchesIdx := strings.Index(out, "Delete branches after archiving")
	integrationsIdx := strings.Index(out, "Integrations")

	if automationsIdx < 0 || markReadyIdx < 0 || depIdx < 0 || repairIdx < 0 || archiveIdx < 0 || deleteBranchesIdx < 0 || integrationsIdx < 0 {
		t.Fatalf("missing expected labels: automations=%d markReady=%d dep=%d repair=%d archive=%d deleteBranches=%d integrations=%d\n%s",
			automationsIdx, markReadyIdx, depIdx, repairIdx, archiveIdx, deleteBranchesIdx, integrationsIdx, out)
	}
	// Under "Automations", in order: Mark ready, Auto-merge Dependabot, Automatic repair, Archive sessions, Delete branches.
	if automationsIdx >= markReadyIdx || markReadyIdx >= depIdx || depIdx >= repairIdx || repairIdx >= archiveIdx || archiveIdx >= deleteBranchesIdx {
		t.Errorf("expected order Automations < markReady < dependabot < repair < archive < deleteBranches; got %d < %d < %d < %d < %d < %d",
			automationsIdx, markReadyIdx, depIdx, repairIdx, archiveIdx, deleteBranchesIdx)
	}
	// All automation toggles render above the Integrations heading.
	if deleteBranchesIdx >= integrationsIdx {
		t.Errorf("automation toggles should render above the Integrations heading; deleteBranches=%d integrations=%d",
			deleteBranchesIdx, integrationsIdx)
	}
}

// TestRepoSettings_ArchiveSessionsAfterMergeToggle verifies the new Automations
// checkbox renders (default-checked) and that activating its row emits an
// UpdateRepoRequest carrying the negated ShouldArchiveSessionsAfterMerge value.
func TestRepoSettings_ArchiveSessionsAfterMergeToggle(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:                              "repo-1",
		DisplayName:                     "Test Repo",
		ShouldArchiveSessionsAfterMerge: true, // default on
	}}}
	m := initSettings(t, stub)

	// Rendered under Automations, default-checked ([x]).
	out := m.View().Content
	labelIdx := strings.Index(out, "Archive sessions after merging PRs")
	if labelIdx < 0 {
		t.Fatalf("expected the archive-after-merge checkbox label, not found in:\n%s", out)
	}
	if checked := strings.LastIndex(out[:labelIdx], "[x]"); checked < 0 {
		t.Errorf("expected the archive-after-merge checkbox to render checked by default:\n%s", out)
	}

	// Navigate to the row and toggle it off.
	cursorToRow(t, &m, repoSettingsRowShouldArchiveSessionsAfterMerge)
	updatedModel, cmd := m.activateRow()
	m = updatedModel.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("activateRow returned a nil cmd; expected an UpdateRepo save")
	}
	cmd() // executes saveSettings, capturing the request on the stub

	if stub.updateReq == nil {
		t.Fatal("no UpdateRepoRequest was captured")
	}
	if stub.updateReq.ShouldArchiveSessionsAfterMerge == nil {
		t.Fatal("UpdateRepoRequest.ShouldArchiveSessionsAfterMerge is nil, want a value")
	}
	if *stub.updateReq.ShouldArchiveSessionsAfterMerge {
		t.Errorf("toggled request ShouldArchiveSessionsAfterMerge = true, want false (negation of the prior true)")
	}
}

// TestRepoSettings_MissingWebhooksHint pins where an operator wondering why a
// merge did not archive finds the answer: directly under the archive toggle,
// and only for a repo the daemon flags.
func TestRepoSettings_MissingWebhooksHint(t *testing.T) {
	render := func(missing bool) []string {
		t.Helper()
		stub := &stubRepoClient{repos: []*pb.Repo{{
			Id:                              "repo-1",
			DisplayName:                     "gigantic",
			ShouldArchiveSessionsAfterMerge: true,
			IsMissingWebhooks:               missing,
		}}}
		m := initSettings(t, stub)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		return strings.Split(stripANSI(updated.(RepoSettingsModel).View().Content), "\n")
	}

	lines := render(true)
	archive := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "Archive sessions after merging PRs") })
	if archive < 0 || archive+1 >= len(lines) {
		t.Fatalf("archive toggle not rendered:\n%s", strings.Join(lines, "\n"))
	}
	if got := strings.TrimSpace(lines[archive+1]); got != missingWebhooksHint {
		t.Fatalf("line under the archive toggle = %q, want the missing-webhooks hint:\n%s", got, strings.Join(lines, "\n"))
	}

	for _, l := range render(false) {
		if strings.Contains(l, "webhooks") {
			t.Fatalf("hint rendered for a repo the daemon did not flag: %q", l)
		}
	}
}

func TestRepoSettings_UnverifiedInstallationHint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		origin       string
		account      string
		wantReverify bool
	}{
		{"https", "https://github.com/freshclaim/widgets.git", "freshclaim", true},
		{"scp case insensitive", "git@github.com:FreshClaim/widgets.git", "freshclaim", true},
		{"different owner", "https://github.com/other/widgets.git", "freshclaim", false},
		{"other host", "https://gitlab.com/freshclaim/widgets.git", "freshclaim", false},
		{"no visible installation", "https://github.com/freshclaim/widgets.git", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1", OriginUrl: tc.origin, IsMissingWebhooks: true}}}
			gh := &fakeRepoSettingsGitHubAppClient{}
			if tc.account != "" {
				gh.unverifiedInstallations = []*pb.GitHubAppUnverifiedInstallation{{InstallationId: 123, AccountLogin: tc.account}}
			}
			m := initSettings(t, stub)
			m.SetGitHubAppInstall(gh)
			if cmd := m.loadGitHubAppStatus(); cmd != nil {
				updated, _ := m.Update(cmd())
				m = updated.(RepoSettingsModel)
			}
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			m = updated.(RepoSettingsModel)
			out := stripANSI(m.View().Content)
			hint := fmt.Sprintf(unverifiedInstallationHint, tc.account)
			if tc.wantReverify {
				if !strings.Contains(out, hint) || strings.Contains(out, missingWebhooksHint) {
					t.Fatalf("expected matching installation hint instead of generic hint:\n%s", out)
				}
				if m.githubAppActionLabel() != reverifyGitHubAppAction || !strings.Contains(out, reverifyGitHubAppAction) {
					t.Fatalf("expected re-verification action:\n%s", out)
				}
				updated, cmd := m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
				m = updated.(RepoSettingsModel)
				if cmd != nil || m.githubAppMode != repoSettingsGitHubModePrompt {
					t.Fatal("re-verification should confirm before connecting")
				}
				updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m = updated.(RepoSettingsModel)
				if cmd == nil || m.githubAppMode != repoSettingsGitHubModeOpening {
					t.Fatal("confirmed re-verification should start the existing setup flow")
				}
				updated, _ = m.Update(repoSettingsGitHubStatusMsg{})
				m = updated.(RepoSettingsModel)
				if m.githubAppActionLabel() == reverifyGitHubAppAction {
					t.Fatal("installation no longer in response should clear re-verification state")
				}
			} else if !strings.Contains(out, missingWebhooksHint) || strings.Contains(out, hint) || m.githubAppActionLabel() == reverifyGitHubAppAction {
				t.Fatalf("nonmatching installation should keep the generic hint and action:\n%s", out)
			}
		})
	}
}

// TestRepoSettings_MissingWebhooksHintSurvivesAToggle pins that flipping the
// archive toggle, the first thing an operator tries, keeps the hint: the view
// adopts the saved repo the daemon returns, which carries the live flag.
func TestRepoSettings_MissingWebhooksHintSurvivesAToggle(t *testing.T) {
	flagged := func(archive bool) *pb.Repo {
		return &pb.Repo{Id: "repo-1", DisplayName: "gigantic", ShouldArchiveSessionsAfterMerge: archive, IsMissingWebhooks: true}
	}
	stub := &stubRepoClient{repos: []*pb.Repo{flagged(true)}, updated: flagged(false)}
	m := initSettings(t, stub)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(RepoSettingsModel)

	cursorToRow(t, &m, repoSettingsRowShouldArchiveSessionsAfterMerge)
	updated, cmd := m.activateRow()
	m = updated.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("activateRow returned a nil cmd; expected an UpdateRepo save")
	}
	updated, _ = m.Update(cmd())
	m = updated.(RepoSettingsModel)

	if out := stripANSI(m.View().Content); !strings.Contains(out, missingWebhooksHint) {
		t.Fatalf("hint gone after toggling archive-after-merge:\n%s", out)
	}
}

// TestRepoSettings_CanAutoDeleteBranchesToggle verifies the new Automations
// checkbox renders (default-checked) and that activating its row emits an
// UpdateRepoRequest carrying the negated CanAutoDeleteBranches value.
func TestRepoSettings_CanAutoDeleteBranchesToggle(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:                    "repo-1",
		DisplayName:           "Test Repo",
		CanAutoDeleteBranches: true, // default on
	}}}
	m := initSettings(t, stub)

	// Rendered under Automations, default-checked ([x]).
	out := m.View().Content
	labelIdx := strings.Index(out, "Delete branches after archiving")
	if labelIdx < 0 {
		t.Fatalf("expected the delete-branches checkbox label, not found in:\n%s", out)
	}
	if checked := strings.LastIndex(out[:labelIdx], "[x]"); checked < 0 {
		t.Errorf("expected the delete-branches checkbox to render checked by default:\n%s", out)
	}

	// Navigate to the row and toggle it off.
	cursorToRow(t, &m, repoSettingsRowCanAutoDeleteBranches)
	updatedModel, cmd := m.activateRow()
	m = updatedModel.(RepoSettingsModel)
	if cmd == nil {
		t.Fatal("activateRow returned a nil cmd; expected an UpdateRepo save")
	}
	cmd() // executes saveSettings, capturing the request on the stub

	if stub.updateReq == nil {
		t.Fatal("no UpdateRepoRequest was captured")
	}
	if stub.updateReq.CanAutoDeleteBranches == nil {
		t.Fatal("UpdateRepoRequest.CanAutoDeleteBranches is nil, want a value")
	}
	if *stub.updateReq.CanAutoDeleteBranches {
		t.Errorf("toggled request CanAutoDeleteBranches = true, want false (negation of the prior true)")
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
		repoSettingsRowIsTrusted,
		repoSettingsRowCanAutoMerge,
		repoSettingsRowCanAutoMergeDependabot,
		repoSettingsRowCanAutoRepair,
		repoSettingsRowShouldArchiveSessionsAfterMerge,
		repoSettingsRowCanAutoDeleteBranches,
		repoSettingsRowLinearHeader,
		repoSettingsRowSentryHeader,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visibleRows order mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// stubRepoClient implements SettingsClient for testing RepoSettingsModel.
type stubRepoClient struct {
	repos     []*pb.Repo
	reposErr  error
	updated   *pb.Repo
	updateErr error
	updateReq *pb.UpdateRepoRequest // captures the last UpdateRepo request
}

type fakeRepoSettingsGitHubAppClient struct {
	installURL              string
	installErr              error
	installURLCalls         int
	repos                   []*pb.GitHubAppRepoStatus
	unverifiedInstallations []*pb.GitHubAppUnverifiedInstallation
}

func (f *fakeRepoSettingsGitHubAppClient) GetGitHubAppInstallURL(context.Context, string) (string, error) {
	f.installURLCalls++
	return f.installURL, f.installErr
}

func (f *fakeRepoSettingsGitHubAppClient) ListGitHubAppRepos(context.Context) (*pb.ListGitHubAppReposResponse, error) {
	return &pb.ListGitHubAppReposResponse{Repos: f.repos, UnverifiedInstallations: f.unverifiedInstallations}, nil
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

func TestRepoSettings_TrustControl(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'y', Text: "y"}, {Code: 'Y', Text: "Y"}} {
		t.Run(key.String(), func(t *testing.T) {
			stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1"}}, updated: &pb.Repo{Id: "repo-1", IsTrusted: true}}
			m := initSettings(t, stub)
			if !strings.Contains(m.View().Content, "Trust this repository") {
				t.Fatal("repository settings has no trust control")
			}
			cursorToRow(t, &m, repoSettingsRowIsTrusted)
			model, cmd := m.activateRow()
			m = model.(RepoSettingsModel)
			if cmd != nil || stub.updateReq != nil || m.repo.IsTrusted {
				t.Fatal("enabling trust wrote before confirmation")
			}
			if !strings.Contains(m.View().Content, "run commands on this machine") {
				t.Fatal("confirmation must explain command execution")
			}
			model, cmd = m.Update(key)
			m = model.(RepoSettingsModel)
			if cmd == nil {
				t.Fatal("confirming trust did not save")
			}
			model, _ = m.Update(cmd())
			m = model.(RepoSettingsModel)
			if stub.updateReq.IsTrusted == nil || !*stub.updateReq.IsTrusted || !m.repo.IsTrusted {
				t.Fatal("confirmation did not persist trust")
			}
			model, cmd = m.activateRow()
			m = model.(RepoSettingsModel)
			if cmd == nil || m.trustConfirm {
				t.Fatal("disabling trust should save immediately")
			}
			cmd()
			if stub.updateReq.IsTrusted == nil || *stub.updateReq.IsTrusted {
				t.Fatal("disabling trust did not send false")
			}
		})
	}
}

func TestRepoSettings_TrustCancel(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: 'n', Text: "n"}, {Code: 'N', Text: "N"}} {
		t.Run(key.String(), func(t *testing.T) {
			stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1"}}}
			m := initSettings(t, stub)
			cursorToRow(t, &m, repoSettingsRowIsTrusted)
			model, _ := m.activateRow()
			m = model.(RepoSettingsModel)
			model, cmd := m.Update(key)
			m = model.(RepoSettingsModel)
			if cmd != nil || stub.updateReq != nil || m.repo.IsTrusted || m.trustConfirm || m.Cancelled() {
				t.Fatal("cancel should leave trust unchanged and return to settings")
			}
		})
	}
}
