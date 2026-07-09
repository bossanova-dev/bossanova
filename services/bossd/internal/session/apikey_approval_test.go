package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSeedAPIKeyApproval_MergesAndIsIdempotent proves seeding adds the sentinel
// suffix to customApiKeyResponses.approved while preserving every other key in
// ~/.claude.json, and that a second call is a no-op.
func TestSeedAPIKeyApproval_MergesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")
	// Pre-existing file with unrelated state + an existing rejected entry.
	if err := os.WriteFile(p, []byte(`{"someOtherKey":123,"customApiKeyResponses":{"rejected":["xxxxxxxxxxxxxxxxxxxx"]}}`), 0o600); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	if err := SeedAPIKeyApproval(p, "00000000000000000000"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Idempotent second call.
	if err := SeedAPIKeyApproval(p, "00000000000000000000"); err != nil {
		t.Fatalf("seed idempotent: %v", err)
	}

	var m map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if m["someOtherKey"] == nil {
		t.Error("unrelated key was dropped")
	}
	resp, ok := m["customApiKeyResponses"].(map[string]any)
	if !ok {
		t.Fatalf("customApiKeyResponses missing/wrong type: %v", m["customApiKeyResponses"])
	}
	approved, ok := resp["approved"].([]any)
	if !ok || len(approved) != 1 || approved[0] != "00000000000000000000" {
		t.Errorf("approved = %v, want single suffix", resp["approved"])
	}
	if resp["rejected"] == nil {
		t.Error("existing rejected list was dropped")
	}
}

// TestSeedAPIKeyApproval_CreatesMissingFile proves an absent ~/.claude.json is
// created with just the approval, and that parse-broken JSON returns an error
// (the caller logs + ignores it — fail-soft).
func TestSeedAPIKeyApproval_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")

	if err := SeedAPIKeyApproval(p, "aaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatalf("seed missing: %v", err)
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	resp := m["customApiKeyResponses"].(map[string]any)
	approved := resp["approved"].([]any)
	if len(approved) != 1 || approved[0] != "aaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("approved = %v, want single suffix", approved)
	}

	bad := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}
	if err := SeedAPIKeyApproval(bad, "aaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Error("expected a parse error for broken JSON")
	}
}

// TestSeedAPIKeyApproval_PreservesExistingApprovals proves a distinct suffix is
// appended without dropping approvals already present.
func TestSeedAPIKeyApproval_PreservesExistingApprovals(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(p, []byte(`{"customApiKeyResponses":{"approved":["11111111111111111111"]}}`), 0o600); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	if err := SeedAPIKeyApproval(p, "22222222222222222222"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	_ = json.Unmarshal(b, &m)
	approved := m["customApiKeyResponses"].(map[string]any)["approved"].([]any)
	if len(approved) != 2 || approved[0] != "11111111111111111111" || approved[1] != "22222222222222222222" {
		t.Errorf("approved = %v, want both suffixes in order", approved)
	}
}
