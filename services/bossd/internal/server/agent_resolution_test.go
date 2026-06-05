package server

import "testing"

func TestResolveDefaultAgentName(t *testing.T) {
	tests := []struct {
		name         string
		requested    string
		loaded       []string
		defaultAgent string
		want         string
	}{
		{
			name:         "explicit request wins over everything",
			requested:    "codex",
			loaded:       []string{"claude"},
			defaultAgent: "claude",
			want:         "codex",
		},
		{
			name:         "whitespace request is treated as blank",
			requested:    "   ",
			loaded:       []string{"opencode"},
			defaultAgent: "claude",
			want:         "opencode",
		},
		{
			name:         "blank with single loaded runner picks that runner",
			requested:    "",
			loaded:       []string{"codex"},
			defaultAgent: "claude",
			want:         "codex",
		},
		{
			name:         "blank with no runners falls back to default",
			requested:    "",
			loaded:       nil,
			defaultAgent: "claude",
			want:         "claude",
		},
		{
			name:         "blank with several runners falls back to default",
			requested:    "",
			loaded:       []string{"claude", "codex"},
			defaultAgent: "codex",
			want:         "codex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveDefaultAgentName(tt.requested, tt.loaded, tt.defaultAgent); got != tt.want {
				t.Fatalf("resolveDefaultAgentName(%q, %v, %q) = %q, want %q",
					tt.requested, tt.loaded, tt.defaultAgent, got, tt.want)
			}
		})
	}
}
