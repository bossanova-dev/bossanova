package views

import (
	"errors"
	"reflect"
	"testing"
)

func TestNotifyCmd(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		paths    map[string]string
		req      notifyRequest
		wantArgs []string
		wantNil  bool
	}{
		{
			// A terminal-notifier on PATH must NOT be preferred: it may be a
			// stale rbenv/asdf shim that satisfies lookPath but fails at runtime,
			// silently shadowing the always-present osascript path. macOS always
			// uses the zero-dependency osascript notification instead.
			name:  "darwin ignores terminal-notifier and uses osascript",
			goos:  "darwin",
			paths: map[string]string{"terminal-notifier": "/usr/local/bin/terminal-notifier"},
			req:   notifyRequest{Title: "Bossanova", Body: "review needed", Sound: true},
			wantArgs: []string{
				"osascript", "-e", `display notification "review needed" with title "Bossanova" sound name "default"`,
			},
		},
		{
			name: "darwin falls back to escaped osascript",
			goos: "darwin",
			req:  notifyRequest{Title: `Bo\\ss "nova"`, Body: `need "input" \\ now`},
			wantArgs: []string{
				"osascript", "-e", `display notification "need \"input\" \\\\ now" with title "Bo\\\\ss \"nova\""`,
			},
		},
		{
			name:  "linux uses notify-send when present",
			goos:  "linux",
			paths: map[string]string{"notify-send": "/usr/bin/notify-send"},
			req:   notifyRequest{Title: "Bossanova", Body: "review needed", Sound: true},
			wantArgs: []string{
				"/usr/bin/notify-send", "Bossanova", "review needed",
			},
		},
		{
			name:    "linux without notifier is silent",
			goos:    "linux",
			wantNil: true,
		},
		{
			name:    "unsupported platform is silent",
			goos:    "windows",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookPath := func(name string) (string, error) {
				if path, ok := tt.paths[name]; ok {
					return path, nil
				}
				return "", errors.New("not found")
			}

			cmd, err := notifyCmd(tt.goos, lookPath, tt.req)
			if err != nil {
				t.Fatalf("notifyCmd() error = %v", err)
			}
			if tt.wantNil {
				if cmd != nil {
					t.Fatalf("notifyCmd() = %#v, want nil", cmd)
				}
				return
			}
			if cmd == nil {
				t.Fatal("notifyCmd() = nil, want command")
			}
			if !reflect.DeepEqual(cmd.Args, tt.wantArgs) {
				t.Errorf("command args = %#v, want %#v", cmd.Args, tt.wantArgs)
			}
		})
	}
}
