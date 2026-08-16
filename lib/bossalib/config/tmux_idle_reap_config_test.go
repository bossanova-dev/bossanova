package config

import (
	"encoding/json"
	"testing"
	"time"
)

// TestTmuxIdleReapConfig_Defaults pins the deliberately inverted posture of the
// idle-pane reaper (BOS-886) against its orphan-reaping sibling. An orphan reap
// destroys the only trace of that work, so TmuxReaperConfig ships off and
// dry-run; an idle reap keeps the chat row and everything attached to it, so
// this one ships enabled and armed. Flipping either default silently would turn
// a shipped feature inert (or arm an unshipped one), which is exactly the class
// of regression this table exists to catch.
func TestTmuxIdleReapConfig_Defaults(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name          string
		cfg           TmuxIdleReapConfig
		wantEnabled   bool
		wantDryRun    bool
		wantThreshold time.Duration
	}{
		{
			name:          "unset is on, live, 8h",
			cfg:           TmuxIdleReapConfig{},
			wantEnabled:   true,
			wantDryRun:    false,
			wantThreshold: 8 * time.Hour,
		},
		{
			// The whole point of the *bool tri-state on a default-on knob: an
			// operator who wrote "enabled": false must be distinguishable from
			// one who wrote nothing, or the feature could never be turned off.
			name:          "explicit false is not unset",
			cfg:           TmuxIdleReapConfig{Enabled: &no, DryRun: &no},
			wantEnabled:   false,
			wantDryRun:    false,
			wantThreshold: 8 * time.Hour,
		},
		{
			name:          "explicitly enabled matches the default",
			cfg:           TmuxIdleReapConfig{Enabled: &yes},
			wantEnabled:   true,
			wantDryRun:    false,
			wantThreshold: 8 * time.Hour,
		},
		{
			name:          "dry-run observes without killing",
			cfg:           TmuxIdleReapConfig{DryRun: &yes},
			wantEnabled:   true,
			wantDryRun:    true,
			wantThreshold: 8 * time.Hour,
		},
		{
			name:          "custom threshold",
			cfg:           TmuxIdleReapConfig{IdleThresholdSeconds: 3600},
			wantEnabled:   true,
			wantThreshold: time.Hour,
		},
		{
			// A hand-edited zero or negative must not collapse the window to
			// nothing: with the feature on by default, that would make every
			// idle chat instantly eligible on the next sweep.
			name:          "zero and negative thresholds fall back",
			cfg:           TmuxIdleReapConfig{IdleThresholdSeconds: -60},
			wantEnabled:   true,
			wantThreshold: 8 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabled(); got != tt.wantEnabled {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.wantEnabled)
			}
			if got := tt.cfg.IsDryRun(); got != tt.wantDryRun {
				t.Errorf("IsDryRun() = %v, want %v", got, tt.wantDryRun)
			}
			if got := tt.cfg.IdleThreshold(); got != tt.wantThreshold {
				t.Errorf("IdleThreshold() = %v, want %v", got, tt.wantThreshold)
			}
		})
	}
}

// TestSettings_TmuxIdleReapRoundTrip proves the block is reachable from
// settings.json and that an operator can actually turn the feature off —
// the case a plain bool would have made unrepresentable.
func TestSettings_TmuxIdleReapRoundTrip(t *testing.T) {
	var s Settings
	raw := `{"worktree_base_dir":"/tmp/wt","tmux_idle_reap":{"enabled":false,"dry_run":true,"idle_threshold_seconds":1800}}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.TmuxIdleReap.IsEnabled() {
		t.Error("IsEnabled() = true, want false — an explicit opt-out must be honoured")
	}
	if !s.TmuxIdleReap.IsDryRun() {
		t.Error("IsDryRun() = false, want true")
	}
	if got := s.TmuxIdleReap.IdleThreshold(); got != 30*time.Minute {
		t.Errorf("IdleThreshold() = %v, want 30m", got)
	}
}

// TestSettings_TmuxIdleReapLegacyFileIsEnabled is the acceptance criterion that
// stops the default-on decision from silently regressing to inert: a settings
// file written before this feature existed — no idle-reap block at all — must
// still come up enabled, live, and on the 8-hour window.
func TestSettings_TmuxIdleReapLegacyFileIsEnabled(t *testing.T) {
	var legacy Settings
	if err := json.Unmarshal([]byte(`{"worktree_base_dir":"/tmp/wt"}`), &legacy); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if !legacy.TmuxIdleReap.IsEnabled() {
		t.Error("legacy IsEnabled() = false, want true — the feature ships on")
	}
	if legacy.TmuxIdleReap.IsDryRun() {
		t.Error("legacy IsDryRun() = true, want false — shipping on but dry-run reclaims nothing")
	}
	if got := legacy.TmuxIdleReap.IdleThreshold(); got != 8*time.Hour {
		t.Errorf("legacy IdleThreshold() = %v, want 8h", got)
	}
}

// TestSettings_TmuxIdleReapOmittedWhenUnset keeps a fresh settings.json free of
// the block. Every knob here is defaulted, so writing them out would only
// freeze today's defaults into every install's config file.
func TestSettings_TmuxIdleReapOmittedWhenUnset(t *testing.T) {
	out, err := json.Marshal(Settings{WorktreeBaseDir: "/tmp/wt"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := round["tmux_idle_reap"]; ok {
		t.Errorf("tmux_idle_reap present in marshalled defaults: %s", out)
	}
}
