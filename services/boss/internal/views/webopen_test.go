package views

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestOpenURLCmd(t *testing.T) {
	t.Parallel()

	const repoURL = "https://github.com/owner/repo"
	tests := []struct {
		name        string
		goos        string
		env         map[string]string
		available   map[string]bool
		wantBase    string
		wantArgs    []string
		wantErrPart string
	}{
		{
			name:     "darwin uses open",
			goos:     "darwin",
			wantBase: "open",
			wantArgs: []string{repoURL},
		},
		{
			name:      "linux uses xdg-open",
			goos:      "linux",
			available: map[string]bool{"xdg-open": true},
			wantBase:  "xdg-open",
			wantArgs:  []string{repoURL},
		},
		{
			name:      "wsl prefers wslview",
			goos:      "linux",
			env:       map[string]string{"WSL_DISTRO_NAME": "Ubuntu"},
			available: map[string]bool{"wslview": true, "cmd.exe": true, "xdg-open": true},
			wantBase:  "wslview",
			wantArgs:  []string{repoURL},
		},
		{
			name:      "wsl falls back to cmd start",
			goos:      "linux",
			env:       map[string]string{"WSL_INTEROP": "/run/WSL/123_interop"},
			available: map[string]bool{"cmd.exe": true, "xdg-open": true},
			wantBase:  "cmd.exe",
			wantArgs:  []string{"/c", "start", "", repoURL},
		},
		{
			name:     "windows uses file protocol handler",
			goos:     "windows",
			wantBase: "rundll32",
			wantArgs: []string{"url.dll,FileProtocolHandler", repoURL},
		},
		{
			name:        "rejects unsupported schemes",
			goos:        "darwin",
			wantErrPart: "unsupported URL scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rawURL := repoURL
			if tt.wantErrPart != "" {
				rawURL = "file:///tmp/repo"
			}

			cmd, err := openURLCmd(envFromMap(tt.env), fakeLookPath(tt.available), tt.goos, rawURL)
			if tt.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("openURLCmd error = %v, want containing %q", err, tt.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("openURLCmd returned error: %v", err)
			}
			if cmd.Args[0] != tt.wantBase {
				t.Fatalf("cmd.Args[0] = %q, want %q", cmd.Args[0], tt.wantBase)
			}
			wantArgs := append([]string{tt.wantBase}, tt.wantArgs...)
			if !reflect.DeepEqual(cmd.Args, wantArgs) {
				t.Fatalf("cmd.Args = %v, want %v", cmd.Args, wantArgs)
			}
		})
	}
}

func fakeLookPath(available map[string]bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if available[name] {
			return name, nil
		}
		return "", errors.New("not found")
	}
}
