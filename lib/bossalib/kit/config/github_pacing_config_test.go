package config

import (
	"encoding/json"
	"testing"
)

func TestGitHubPacingConfig_Rates(t *testing.T) {
	tests := []struct {
		name        string
		cfg         GitHubPacingConfig
		wantCore    int
		wantGraphQL int
	}{
		{name: "unset uses defaults", cfg: GitHubPacingConfig{}, wantCore: 300, wantGraphQL: 600},
		{
			name:        "zero and negative use defaults",
			cfg:         GitHubPacingConfig{CorePerMinute: 0, GraphQLPerMinute: -40},
			wantCore:    300,
			wantGraphQL: 600,
		},
		{
			name:        "in-range values are honoured",
			cfg:         GitHubPacingConfig{CorePerMinute: 120, GraphQLPerMinute: 1500},
			wantCore:    120,
			wantGraphQL: 1500,
		},
		{
			name:        "values above the ceilings clamp to them",
			cfg:         GitHubPacingConfig{CorePerMinute: 5000, GraphQLPerMinute: 2001},
			wantCore:    900,
			wantGraphQL: 2000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.CorePointsPerMinute(); got != tt.wantCore {
				t.Errorf("CorePointsPerMinute() = %d, want %d", got, tt.wantCore)
			}
			if got := tt.cfg.GraphQLPointsPerMinute(); got != tt.wantGraphQL {
				t.Errorf("GraphQLPointsPerMinute() = %d, want %d", got, tt.wantGraphQL)
			}
		})
	}
}

func TestGitHubPacingConfig_DefaultsSitBelowTheCeilings(t *testing.T) {
	if DefaultGitHubCorePointsPerMinute >= GitHubCorePointsPerMinuteMax {
		t.Errorf("core default %d is not below the ceiling %d", DefaultGitHubCorePointsPerMinute, GitHubCorePointsPerMinuteMax)
	}
	if DefaultGitHubGraphQLPointsPerMinute >= GitHubGraphQLPointsPerMinuteMax {
		t.Errorf("graphql default %d is not below the ceiling %d", DefaultGitHubGraphQLPointsPerMinute, GitHubGraphQLPointsPerMinuteMax)
	}
}

func TestSettings_GitHubPacingRoundTrip(t *testing.T) {
	raw := `{"worktree_base_dir":"/tmp/wt","github_pacing":{"core_points_per_minute":150,"graphql_points_per_minute":450}}`
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round Settings
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal round trip: %v", err)
	}
	if got := round.GitHubPacing.CorePointsPerMinute(); got != 150 {
		t.Errorf("CorePointsPerMinute() after round trip = %d, want 150", got)
	}
	if got := round.GitHubPacing.GraphQLPointsPerMinute(); got != 450 {
		t.Errorf("GraphQLPointsPerMinute() after round trip = %d, want 450", got)
	}
}

func TestSettings_GitHubPacingLegacyFileKeepsDefaults(t *testing.T) {
	var legacy Settings
	if err := json.Unmarshal([]byte(`{"worktree_base_dir":"/tmp/wt"}`), &legacy); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := legacy.GitHubPacing.CorePointsPerMinute(); got != DefaultGitHubCorePointsPerMinute {
		t.Errorf("CorePointsPerMinute() = %d, want %d", got, DefaultGitHubCorePointsPerMinute)
	}
	if got := legacy.GitHubPacing.GraphQLPointsPerMinute(); got != DefaultGitHubGraphQLPointsPerMinute {
		t.Errorf("GraphQLPointsPerMinute() = %d, want %d", got, DefaultGitHubGraphQLPointsPerMinute)
	}
	out, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("Unmarshal fields: %v", err)
	}
	if _, ok := fields["github_pacing"]; ok {
		t.Errorf("unset github_pacing was written: %s", out)
	}
}
