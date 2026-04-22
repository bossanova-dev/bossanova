package views

import (
	"errors"
	"strings"
	"testing"
)

func envFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestNewTabCmd(t *testing.T) {
	t.Parallel()

	const cwd = "/Users/dave/worktrees/foo"

	tests := []struct {
		name     string
		env      map[string]string
		goos     string
		wantBase string
		wantArgs []string // all must appear in cmd.Args (order not enforced)
		wantErr  bool
		checkScr func(t *testing.T, script string)
	}{
		{
			name:     "tmux takes priority over iTerm",
			env:      map[string]string{"TMUX": "/tmp/tmux-501/default,1234,0", "TERM_PROGRAM": "iTerm.app"},
			goos:     "darwin",
			wantBase: "tmux",
			wantArgs: []string{"new-window", "-c", cwd},
		},
		{
			name:     "iTerm via TERM_PROGRAM",
			env:      map[string]string{"TERM_PROGRAM": "iTerm.app"},
			goos:     "darwin",
			wantBase: "osascript",
			checkScr: func(t *testing.T, script string) {
				if !strings.Contains(script, `tell application "iTerm"`) {
					t.Errorf("script missing iTerm tell: %q", script)
				}
				if !strings.Contains(script, cwd) {
					t.Errorf("script missing cwd %q: %q", cwd, script)
				}
			},
		},
		{
			name:     "iTerm via ITERM_SESSION_ID",
			env:      map[string]string{"ITERM_SESSION_ID": "w0t0p0:1234"},
			goos:     "darwin",
			wantBase: "osascript",
		},
		{
			name:     "Ghostty via GHOSTTY_RESOURCES_DIR on darwin",
			env:      map[string]string{"GHOSTTY_RESOURCES_DIR": "/Applications/Ghostty.app/Contents/Resources/ghostty"},
			goos:     "darwin",
			wantBase: "open",
			wantArgs: []string{"-a", "Ghostty", cwd},
		},
		{
			name:     "Ghostty via TERM_PROGRAM case-insensitive",
			env:      map[string]string{"TERM_PROGRAM": "Ghostty"},
			goos:     "darwin",
			wantBase: "open",
			wantArgs: []string{"-a", "Ghostty", cwd},
		},
		{
			name:    "Ghostty on linux is unsupported (no CLI yet)",
			env:     map[string]string{"GHOSTTY_RESOURCES_DIR": "/foo"},
			goos:    "linux",
			wantErr: true,
		},
		{
			name:    "unknown terminal errors",
			env:     map[string]string{"TERM_PROGRAM": "Apple_Terminal"},
			goos:    "darwin",
			wantErr: true,
		},
		{
			name:    "empty env errors",
			env:     map[string]string{},
			goos:    "darwin",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := newTabCmd(envFromMap(tt.env), tt.goos, cwd)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("wanted error, got cmd=%v", cmd)
				}
				var ute *unsupportedTerminalError
				if !errors.As(err, &ute) {
					t.Errorf("err should be *unsupportedTerminalError, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasSuffix(cmd.Path, "/"+tt.wantBase) && cmd.Path != tt.wantBase {
				// cmd.Path is resolved via LookPath; accept either bare or absolute
				if cmd.Args[0] != tt.wantBase {
					t.Errorf("cmd.Args[0] = %q, want %q", cmd.Args[0], tt.wantBase)
				}
			}
			for _, want := range tt.wantArgs {
				found := false
				for _, a := range cmd.Args {
					if a == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("cmd.Args missing %q: got %v", want, cmd.Args)
				}
			}
			if tt.checkScr != nil {
				// osascript -e <script>
				if len(cmd.Args) < 3 {
					t.Fatalf("osascript cmd has too few args: %v", cmd.Args)
				}
				tt.checkScr(t, cmd.Args[2])
			}
		})
	}
}

func TestEscapeForAppleScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{`/plain/path`, `/plain/path`},
		{`/with spaces/ok`, `/with spaces/ok`},
		{`/has"quote/x`, `/has\"quote/x`},
		{`/has\back/x`, `/has\\back/x`},
		{`/both"and\back`, `/both\"and\\back`},
	}
	for _, tt := range tests {
		if got := escapeForAppleScript(tt.in); got != tt.want {
			t.Errorf("escape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildITermScript(t *testing.T) {
	t.Parallel()

	script := buildITermScript("/Users/dave/repo with space")
	for _, want := range []string{
		`tell application "iTerm"`,
		`create tab with default profile`,
		`/Users/dave/repo with space`,
		`cd '`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
}

func TestBuildITermScriptShellSafe(t *testing.T) {
	t.Parallel()

	// Paths with shell metacharacters must be single-quoted so the
	// shell does not expand them. The backticks, $, and $(...) below
	// should all appear literally, wrapped in a single-quoted cd arg.
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{
			name: "dollar and parens are not expanded",
			cwd:  "/tmp/$(whoami)",
			want: `cd '/tmp/$(whoami)'`,
		},
		{
			name: "backticks are not expanded",
			cwd:  "/tmp/`id`",
			want: "cd '/tmp/`id`'",
		},
		{
			name: "single quote breaks and rejoins the quoted run",
			cwd:  `/tmp/it's`,
			// AppleScript escapes the \ in the shell's '\'' sequence,
			// so the rendered script shows '\\'' — AppleScript parses
			// that back to '\'' before writing it to the shell.
			want: `cd '/tmp/it'\\''s'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script := buildITermScript(tt.cwd)
			if !strings.Contains(script, tt.want) {
				t.Errorf("script missing %q\n---\n%s", tt.want, script)
			}
		})
	}
}

func TestEscapeForShellSingleQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{`/plain/path`, `/plain/path`},
		{`/with spaces/ok`, `/with spaces/ok`},
		{`/has$dollar`, `/has$dollar`},
		{"/has`back`tick", "/has`back`tick"},
		{`/has$(cmd)`, `/has$(cmd)`},
		{`/has'quote`, `/has'\''quote`},
		{`/many'q'u'otes`, `/many'\''q'\''u'\''otes`},
	}
	for _, tt := range tests {
		if got := escapeForShellSingleQuote(tt.in); got != tt.want {
			t.Errorf("escape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
