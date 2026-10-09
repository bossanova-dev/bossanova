package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func intPtr(v int) *int { return &v }

// TestNotesConfig_Accessors pins the retention-knob semantics (BOS-1384):
// unset and negative fall back to the shipped defaults, 0 is the explicit
// "unlimited" opt-out, and a positive value is taken as-is.
func TestNotesConfig_Accessors(t *testing.T) {
	tests := []struct {
		name          string
		cfg           NotesConfig
		wantRetention int
		wantMax       int
	}{
		{name: "unset uses defaults", cfg: NotesConfig{}, wantRetention: 180, wantMax: 10000},
		{
			name:          "zero means unlimited",
			cfg:           NotesConfig{RetentionDays: intPtr(0), MaxPerRepo: intPtr(0)},
			wantRetention: 0,
			wantMax:       0,
		},
		{
			// A hand-edit typo must not disable retention.
			name:          "negative falls back to defaults",
			cfg:           NotesConfig{RetentionDays: intPtr(-5), MaxPerRepo: intPtr(-1)},
			wantRetention: 180,
			wantMax:       10000,
		},
		{
			name:          "positive values are honoured",
			cfg:           NotesConfig{RetentionDays: intPtr(30), MaxPerRepo: intPtr(50)},
			wantRetention: 30,
			wantMax:       50,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.RetentionDaysOrDefault(); got != tt.wantRetention {
				t.Errorf("RetentionDaysOrDefault() = %d, want %d", got, tt.wantRetention)
			}
			if got := tt.cfg.MaxPerRepoOrDefault(); got != tt.wantMax {
				t.Errorf("MaxPerRepoOrDefault() = %d, want %d", got, tt.wantMax)
			}
		})
	}
}

// TestSettings_NotesExplicitZeroRoundTrips is the *int + omitempty contract
// UpdateSettings relies on: an explicit 0 ("unlimited") read from disk must
// decode as a non-nil 0 and survive a save/load cycle, or the next settings
// write would silently re-enable pruning.
func TestSettings_NotesExplicitZeroRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	raw := `{"worktree_base_dir":"/tmp/wt","notes":{"retention_days":0,"max_per_repo":50}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if s.Notes.RetentionDays == nil || *s.Notes.RetentionDays != 0 {
		t.Fatalf("RetentionDays = %v, want non-nil 0", s.Notes.RetentionDays)
	}
	if got := s.Notes.MaxPerRepoOrDefault(); got != 50 {
		t.Fatalf("MaxPerRepoOrDefault() = %d, want 50", got)
	}

	roundPath := filepath.Join(dir, "round.json")
	if err := SaveTo(roundPath, s); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	round, err := LoadFrom(roundPath)
	if err != nil {
		t.Fatalf("LoadFrom round trip: %v", err)
	}
	if round.Notes.RetentionDays == nil || *round.Notes.RetentionDays != 0 {
		t.Fatalf("round-trip RetentionDays = %v, want non-nil 0", round.Notes.RetentionDays)
	}
	if got := round.Notes.RetentionDaysOrDefault(); got != 0 {
		t.Errorf("round-trip RetentionDaysOrDefault() = %d, want 0 (unlimited)", got)
	}
	if got := round.Notes.MaxPerRepoOrDefault(); got != 50 {
		t.Errorf("round-trip MaxPerRepoOrDefault() = %d, want 50", got)
	}
}

// TestSettings_NotesOmittedWhenUnset keeps a fresh settings.json free of the
// notes block: both knobs are defaulted, so writing them out would only freeze
// today's defaults into every install's config file.
func TestSettings_NotesOmittedWhenUnset(t *testing.T) {
	out, err := json.Marshal(DefaultSettings())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := round["notes"]; ok {
		t.Errorf("notes present in marshalled defaults: %s", out)
	}
}
