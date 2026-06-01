package auth

import (
	"errors"
	"os/exec"
	"testing"
)

func TestOpenBrowserCommandNativePlatforms(t *testing.T) {
	tests := []struct {
		name string
		goos string
		want string
	}{
		{name: "macOS", goos: "darwin", want: "open"},
		{name: "linux", goos: "linux", want: "xdg-open"},
		{name: "windows", goos: "windows", want: "rundll32"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := openBrowserCommand("https://example.test", tt.goos, false, func(name string) (string, error) {
				return name, nil
			})
			if err != nil {
				t.Fatalf("openBrowserCommand returned error: %v", err)
			}
			if got := cmd.Args[0]; got != tt.want {
				t.Fatalf("cmd.Args[0] = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenBrowserCommandWSLPrefersWslview(t *testing.T) {
	cmd, err := openBrowserCommand("https://example.test", "linux", true, func(name string) (string, error) {
		if name == "wslview" {
			return "wslview", nil
		}
		return "", errors.New("missing")
	})
	if err != nil {
		t.Fatalf("openBrowserCommand returned error: %v", err)
	}
	if got := cmd.Args[0]; got != "wslview" {
		t.Fatalf("cmd.Args[0] = %q, want wslview", got)
	}
}

func TestOpenBrowserCommandWSLFallsBackToCmdExe(t *testing.T) {
	rawURL := "https://example.test/path?code=abc&state=xyz"
	cmd, err := openBrowserCommand(rawURL, "linux", true, func(name string) (string, error) {
		if name == "cmd.exe" {
			return "cmd.exe", nil
		}
		return "", errors.New("missing")
	})
	if err != nil {
		t.Fatalf("openBrowserCommand returned error: %v", err)
	}
	if got := cmd.Args[0]; got != "cmd.exe" {
		t.Fatalf("cmd.Args[0] = %q, want cmd.exe", got)
	}
	if len(cmd.Args) != 5 || cmd.Args[1] != "/c" || cmd.Args[2] != "start" || cmd.Args[3] != "" || cmd.Args[4] != `"`+rawURL+`"` {
		t.Fatalf("cmd.Args = %#v, want cmd.exe /c start \"\" %q", cmd.Args, `"`+rawURL+`"`)
	}
}

func TestOpenBrowserCommandUnsupportedPlatform(t *testing.T) {
	_, err := openBrowserCommand("https://example.test", "plan9", false, exec.LookPath)
	if err == nil {
		t.Fatal("openBrowserCommand returned nil error for unsupported platform")
	}
}
