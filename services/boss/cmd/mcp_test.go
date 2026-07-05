package main

import (
	"strings"
	"testing"
)

func TestMcpBuildDriftLine(t *testing.T) {
	t.Parallel()

	const running = "v1.2.3 (aaa) built 2026-07-01"
	const onDisk = "v1.2.4 (bbb) built 2026-07-04"

	cases := []struct {
		name        string
		running     string
		onDisk      string
		wantSubstrs []string
		wantNoDrift bool // must NOT contain the drift marker
	}{
		{
			name:        "match",
			running:     running,
			onDisk:      running,
			wantSubstrs: []string{running, "running matches on-disk"},
			wantNoDrift: true,
		},
		{
			name:        "drift",
			running:     running,
			onDisk:      onDisk,
			wantSubstrs: []string{"drift", running, onDisk, "restart the MCP service"},
		},
		{
			name:        "running unavailable",
			running:     "",
			onDisk:      onDisk,
			wantSubstrs: []string{onDisk, "running build unavailable"},
			wantNoDrift: true,
		},
		{
			name:        "on-disk unavailable",
			running:     running,
			onDisk:      "",
			wantSubstrs: []string{running, "on-disk build unavailable"},
			wantNoDrift: true,
		},
		{
			name:        "both unavailable",
			running:     "",
			onDisk:      "",
			wantSubstrs: []string{"unavailable"},
			wantNoDrift: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpBuildDriftLine(tc.running, tc.onDisk)
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("mcpBuildDriftLine(%q, %q) = %q; missing %q", tc.running, tc.onDisk, got, want)
				}
			}
			if tc.wantNoDrift && strings.Contains(got, "⚠ drift") {
				t.Errorf("mcpBuildDriftLine(%q, %q) = %q; must not report drift", tc.running, tc.onDisk, got)
			}
		})
	}
}
