package config

import (
	"encoding/json"
	"testing"
	"time"
)

// TestTmuxReaperConfig_Defaults pins the safe-by-default posture of the
// orphaned-tmux reaper (BOS-846). The reaper kills processes, so an absent or
// half-written settings block must land on "off, and if you turn it on it only
// talks" rather than on "armed".
func TestTmuxReaperConfig_Defaults(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name              string
		cfg               TmuxReaperConfig
		wantEnabled       bool
		wantDryRun        bool
		wantReapUnstamped bool
		wantSweep         time.Duration
		wantGrace         time.Duration
	}{
		{
			name:        "unset is off, dry-run, unstamped-safe, 5m/10m",
			cfg:         TmuxReaperConfig{},
			wantEnabled: false,
			wantDryRun:  true,
			wantSweep:   5 * time.Minute,
			wantGrace:   10 * time.Minute,
		},
		{
			name:              "fully armed",
			cfg:               TmuxReaperConfig{Enabled: &yes, DryRun: &no, ReapUnstamped: &yes, SweepIntervalSeconds: 30, GracePeriodSeconds: 120},
			wantEnabled:       true,
			wantDryRun:        false,
			wantReapUnstamped: true,
			wantSweep:         30 * time.Second,
			wantGrace:         120 * time.Second,
		},
		{
			// The whole point of the *bool tri-state: an operator who wrote
			// "dry_run": false must be distinguishable from one who wrote
			// nothing, or arming the reaper would be impossible.
			name:        "explicit false is not unset",
			cfg:         TmuxReaperConfig{Enabled: &no, DryRun: &no, ReapUnstamped: &no},
			wantEnabled: false,
			wantDryRun:  false,
		},
		{
			name:        "enabled without dry_run still only talks",
			cfg:         TmuxReaperConfig{Enabled: &yes},
			wantEnabled: true,
			wantDryRun:  true,
			wantSweep:   5 * time.Minute,
			wantGrace:   10 * time.Minute,
		},
		{
			// A hand-edited zero or negative must not collapse the grace
			// window to nothing: that would make every freshly created pane
			// instantly old enough to reap.
			name:       "zero and negative durations fall back",
			cfg:        TmuxReaperConfig{SweepIntervalSeconds: 0, GracePeriodSeconds: -600},
			wantDryRun: true,
			wantSweep:  5 * time.Minute,
			wantGrace:  10 * time.Minute,
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
			if got := tt.cfg.ReapsUnstamped(); got != tt.wantReapUnstamped {
				t.Errorf("ReapsUnstamped() = %v, want %v", got, tt.wantReapUnstamped)
			}
			if tt.wantSweep != 0 {
				if got := tt.cfg.SweepInterval(); got != tt.wantSweep {
					t.Errorf("SweepInterval() = %v, want %v", got, tt.wantSweep)
				}
			}
			if tt.wantGrace != 0 {
				if got := tt.cfg.GracePeriod(); got != tt.wantGrace {
					t.Errorf("GracePeriod() = %v, want %v", got, tt.wantGrace)
				}
			}
		})
	}
}

// TestSettings_TmuxReaperRoundTrip proves the block is reachable from
// settings.json, and that a settings file predating it keeps the reaper off.
func TestSettings_TmuxReaperRoundTrip(t *testing.T) {
	var s Settings
	raw := `{"worktree_base_dir":"/tmp/wt","tmux_reaper":{"enabled":true,"dry_run":false,"reap_unstamped":true,"sweep_interval_seconds":60,"grace_period_seconds":900}}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !s.TmuxReaper.IsEnabled() {
		t.Error("IsEnabled() = false, want true")
	}
	if s.TmuxReaper.IsDryRun() {
		t.Error("IsDryRun() = true, want false")
	}
	if !s.TmuxReaper.ReapsUnstamped() {
		t.Error("ReapsUnstamped() = false, want true")
	}
	if got := s.TmuxReaper.SweepInterval(); got != time.Minute {
		t.Errorf("SweepInterval() = %v, want 1m", got)
	}
	if got := s.TmuxReaper.GracePeriod(); got != 15*time.Minute {
		t.Errorf("GracePeriod() = %v, want 15m", got)
	}

	var legacy Settings
	if err := json.Unmarshal([]byte(`{"worktree_base_dir":"/tmp/wt"}`), &legacy); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if legacy.TmuxReaper.IsEnabled() {
		t.Error("legacy IsEnabled() = true, want false — an upgrade must not arm a killer")
	}
	if !legacy.TmuxReaper.IsDryRun() {
		t.Error("legacy IsDryRun() = false, want true")
	}
	if legacy.TmuxReaper.ReapsUnstamped() {
		t.Error("legacy ReapsUnstamped() = true, want false")
	}
}

// TestSettings_TmuxReaperOmittedWhenUnset keeps a fresh settings.json free of
// the block. The key appearing on every install would read as an advertised
// feature and invite operators to arm it before it has run in dry-run.
func TestSettings_TmuxReaperOmittedWhenUnset(t *testing.T) {
	out, err := json.Marshal(Settings{WorktreeBaseDir: "/tmp/wt"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := round["tmux_reaper"]; ok {
		t.Errorf("tmux_reaper present in marshalled defaults: %s", out)
	}
}
