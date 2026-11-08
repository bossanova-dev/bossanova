package tmux

import (
	"context"
	"testing"
)

func TestCurrentWorktreePaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script *scriptedTmux
		want   int
		failed bool
	}{
		{"paths", &scriptedTmux{stdout: "/one path\n/two\n"}, 2, false},
		{"absent", &scriptedTmux{exit: 1, stderr: "no server running on /tmp/default"}, 0, false},
		{"missing socket", &scriptedTmux{exit: 1, stderr: "error connecting to /tmp/default (No such file or directory)"}, 0, false},
		{"permission", &scriptedTmux{exit: 1, stderr: "Permission denied"}, 0, true},
		{"partial", &scriptedTmux{exit: 1, stdout: "/somewhere\n"}, 0, true},
		{"invalid", &scriptedTmux{stdout: "relative\n"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := NewClient(WithCommandFactory(tc.script.factory)).CurrentPaths(context.Background())
			if (err != nil) != tc.failed || len(paths) != tc.want {
				t.Fatalf("paths=%v err=%v", paths, err)
			}
		})
	}
}
