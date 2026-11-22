package server

import (
	"strings"
	"testing"
)

func TestResolveDefaultAgentName(t *testing.T) {
	tests := []struct {
		name         string
		requested    string
		loaded       []string
		defaultAgent string
		want         string
		wantErr      bool
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
		{
			name:         "blank with several runners and an unloaded default is refused",
			requested:    "",
			loaded:       []string{"claude", "codex"},
			defaultAgent: "antigravity",
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDefaultAgentName(tt.requested, tt.loaded, tt.defaultAgent)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveDefaultAgentName(%q, %v, %q) = %q, want error", tt.requested, tt.loaded, tt.defaultAgent, got)
				}
				for _, name := range append([]string{tt.defaultAgent}, tt.loaded...) {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("error %q does not name %q", err, name)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDefaultAgentName(%q, %v, %q): %v", tt.requested, tt.loaded, tt.defaultAgent, err)
			}
			if got != tt.want {
				t.Fatalf("resolveDefaultAgentName(%q, %v, %q) = %q, want %q",
					tt.requested, tt.loaded, tt.defaultAgent, got, tt.want)
			}
		})
	}
}
