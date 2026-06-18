package views

import (
	"context"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// initSettings builds and initializes a RepoSettingsModel for the given repo.
func initSettings(t *testing.T, stub *stubRepoClient) RepoSettingsModel {
	t.Helper()
	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	updated, _ := m.Update(m.Init()())
	return updated.(RepoSettingsModel)
}

func TestRepoSettings_SentryApiKeyEditIsFullReplace(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:           "repo-1",
		DisplayName:  "Test Repo",
		SentryApiKey: "sntrys_existing_1234",
	}}}
	m := initSettings(t, stub)

	cursorToRow(t, &m, repoSettingsRowSentryApiKey)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)

	if m.sentryApiKeyInput.Value() != "" {
		t.Errorf("sentryApiKeyInput.Value() = %q, want empty (full replace)", m.sentryApiKeyInput.Value())
	}
	if m.editingField != repoSettingsRowSentryApiKey {
		t.Errorf("editingField = %d, want %d", m.editingField, repoSettingsRowSentryApiKey)
	}
}

func TestRepoSettings_SentryOrgEditPreFillsExisting(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:        "repo-1",
		SentryOrg: "acme",
	}}}
	m := initSettings(t, stub)

	cursorToRow(t, &m, repoSettingsRowSentryOrg)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)

	// The org slug is plain text — editing pre-fills the current value.
	if m.sentryOrgInput.Value() != "acme" {
		t.Errorf("sentryOrgInput.Value() = %q, want acme (edit in place)", m.sentryOrgInput.Value())
	}
}

func TestRepoSettings_SentryCommitsSendRightField(t *testing.T) {
	t.Run("api key", func(t *testing.T) {
		stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1"}}}
		m := initSettings(t, stub)
		cursorToRow(t, &m, repoSettingsRowSentryApiKey)
		updated, _ := m.activateRow()
		m = updated.(RepoSettingsModel)
		m.sentryApiKeyInput.SetValue("  sntrys_new  ")
		updated, cmd := m.commitEdit()
		m = updated.(RepoSettingsModel)
		if cmd != nil {
			cmd()
		}
		if stub.updateReq == nil || stub.updateReq.GetSentryApiKey() != "sntrys_new" {
			t.Errorf("SentryApiKey = %q, want sntrys_new (trimmed)", stub.updateReq.GetSentryApiKey())
		}
	})

	t.Run("org", func(t *testing.T) {
		stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1"}}}
		m := initSettings(t, stub)
		cursorToRow(t, &m, repoSettingsRowSentryOrg)
		updated, _ := m.activateRow()
		m = updated.(RepoSettingsModel)
		m.sentryOrgInput.SetValue("acme")
		updated, cmd := m.commitEdit()
		m = updated.(RepoSettingsModel)
		if cmd != nil {
			cmd()
		}
		if stub.updateReq == nil || stub.updateReq.GetSentryOrg() != "acme" {
			t.Errorf("SentryOrg = %q, want acme", stub.updateReq.GetSentryOrg())
		}
	})
}

// TestRepoSettings_InitialExpansionFromCredentials verifies that integration
// expansion is derived from credential presence when the repo loads.
func TestRepoSettings_InitialExpansionFromCredentials(t *testing.T) {
	tests := []struct {
		name       string
		repo       *pb.Repo
		wantLinear bool
		wantSentry bool
	}{
		{
			name:       "all empty collapses both",
			repo:       &pb.Repo{Id: "repo-1"},
			wantLinear: false,
			wantSentry: false,
		},
		{
			name:       "linear key expands linear only",
			repo:       &pb.Repo{Id: "repo-1", LinearApiKey: "lin_123"},
			wantLinear: true,
			wantSentry: false,
		},
		{
			name:       "sentry org alone expands sentry",
			repo:       &pb.Repo{Id: "repo-1", SentryOrg: "acme"},
			wantLinear: false,
			wantSentry: true,
		},
		{
			name:       "sentry key alone expands sentry",
			repo:       &pb.Repo{Id: "repo-1", SentryApiKey: "sntrys_1"},
			wantLinear: false,
			wantSentry: true,
		},
		{
			name:       "both configured expands both",
			repo:       &pb.Repo{Id: "repo-1", LinearApiKey: "lin_123", SentryApiKey: "sntrys_1"},
			wantLinear: true,
			wantSentry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubRepoClient{repos: []*pb.Repo{tt.repo}}
			m := initSettings(t, stub)
			if m.linearExpanded != tt.wantLinear {
				t.Errorf("linearExpanded = %v, want %v", m.linearExpanded, tt.wantLinear)
			}
			if m.sentryExpanded != tt.wantSentry {
				t.Errorf("sentryExpanded = %v, want %v", m.sentryExpanded, tt.wantSentry)
			}
		})
	}
}

// TestRepoSettings_SentryMissingFields verifies the partial-config red rule:
// nothing red when both empty or both set; the missing field red when partial.
func TestRepoSettings_SentryMissingFields(t *testing.T) {
	tests := []struct {
		name             string
		key, org         string
		wantKey, wantOrg bool
	}{
		{"both empty", "", "", false, false},
		{"both set", "k", "o", false, false},
		{"only key set", "k", "", false, true},
		{"only org set", "", "o", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := RepoSettingsModel{repo: &pb.Repo{
				SentryApiKey: tt.key,
				SentryOrg:    tt.org,
			}}
			gotKey, gotOrg := m.sentryMissingFields()
			if gotKey != tt.wantKey || gotOrg != tt.wantOrg {
				t.Errorf("sentryMissingFields() = (%v,%v), want (%v,%v)",
					gotKey, gotOrg, tt.wantKey, tt.wantOrg)
			}
		})
	}
}

// TestRepoSettings_PartialSentryRendersRed verifies the View renders missing
// Sentry fields in the danger color when partially configured, and renders no
// red when fully configured.
func TestRepoSettings_PartialSentryRendersRed(t *testing.T) {
	redNotSet := styleStatusDanger.Render("(not set)")

	t.Run("partial config reds missing fields", func(t *testing.T) {
		stub := &stubRepoClient{repos: []*pb.Repo{{
			Id:           "repo-1",
			SentryApiKey: "sntrys_1234", // set; org slug missing
		}}}
		m := initSettings(t, stub)
		view := m.View().Content
		if !strings.Contains(view, redNotSet) {
			t.Errorf("expected a red (not set) for a partial Sentry config; view:\n%s", view)
		}
	})

	t.Run("fully configured renders no red", func(t *testing.T) {
		stub := &stubRepoClient{repos: []*pb.Repo{{
			Id:           "repo-1",
			SentryApiKey: "sntrys_1234",
			SentryOrg:    "acme",
		}}}
		m := initSettings(t, stub)
		view := m.View().Content
		if strings.Contains(view, redNotSet) {
			t.Errorf("did not expect red (not set) for a complete Sentry config; view:\n%s", view)
		}
	})

	t.Run("all empty renders no red", func(t *testing.T) {
		stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1"}}}
		m := initSettings(t, stub)
		m.sentryExpanded = true // expand to render the child rows
		view := m.View().Content
		if strings.Contains(view, redNotSet) {
			t.Errorf("did not expect red for an all-empty Sentry config; view:\n%s", view)
		}
	})
}

// TestRepoSettings_ToggleHeaderExpandsWithoutEditing verifies that activating an
// integration header toggles expansion (UI only) and never enters edit mode.
func TestRepoSettings_ToggleHeaderExpandsWithoutEditing(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1"}}}
	m := initSettings(t, stub)

	// Both collapsed initially (no credentials).
	if m.sentryExpanded {
		t.Fatal("expected Sentry collapsed initially")
	}

	cursorToHeader(t, &m, repoSettingsRowSentryHeader)
	updated, cmd := m.activateRow()
	m = updated.(RepoSettingsModel)

	if !m.sentryExpanded {
		t.Error("activating Sentry header should expand it")
	}
	if m.editingField != repoSettingsRowNone {
		t.Errorf("editingField = %d, want none (toggling must not edit)", m.editingField)
	}
	if cmd != nil {
		t.Error("expected no command from a UI-only toggle")
	}
	// Cursor stays on the header.
	if m.currentRow() != repoSettingsRowSentryHeader {
		t.Errorf("currentRow = %d, want Sentry header after toggle", m.currentRow())
	}

	// Toggle again to collapse.
	updated, _ = m.activateRow()
	m = updated.(RepoSettingsModel)
	if m.sentryExpanded {
		t.Error("activating Sentry header again should collapse it")
	}
}

// TestRepoSettings_CollapseClampsCursor verifies that when a collapse shrinks the
// visible row set below the cursor's index, clampCursor pulls the cursor back
// onto the last visible row rather than leaving it dangling out of range.
func TestRepoSettings_CollapseClampsCursor(t *testing.T) {
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:           "repo-1",
		SentryApiKey: "sntrys_1234",
		SentryOrg:    "acme",
	}}}
	m := initSettings(t, stub) // Sentry expanded

	// Cursor on the last Sentry child row (the largest index).
	cursorToHeader(t, &m, repoSettingsRowSentryOrg)
	childIndex := m.cursor

	// Simulate the row set shrinking beneath the cursor (collapse), then clamp.
	m.sentryExpanded = false
	if childIndex < len(m.visibleRows()) {
		t.Fatalf("precondition: child index %d should exceed collapsed row count %d", childIndex, len(m.visibleRows()))
	}
	m.clampCursor()

	if m.cursor >= len(m.visibleRows()) {
		t.Errorf("cursor = %d not clamped into visibleRows (len %d)", m.cursor, len(m.visibleRows()))
	}
	// Lands on the now-last visible row, the Sentry header.
	if m.currentRow() != repoSettingsRowSentryHeader {
		t.Errorf("currentRow after clamp = %d, want Sentry header", m.currentRow())
	}
}

// cursorToHeader positions the cursor on the given visible rowID (no auto-expand),
// failing if the row is not currently visible.
func cursorToHeader(t *testing.T, m *RepoSettingsModel, row rowID) {
	t.Helper()
	for i, r := range m.visibleRows() {
		if r == row {
			m.cursor = i
			return
		}
	}
	t.Fatalf("row %d is not visible; visibleRows = %v", row, m.visibleRows())
}
