package loginshell

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWrap_FishUsesArgvNoLabel(t *testing.T) {
	// fish puts ALL args after the -c body into $argv -- NO $0 label slot.
	got := Wrap("/opt/homebrew/bin/fish", []string{"-l", "-c"}, []string{"codex", "--flag", "a prompt"})
	want := []string{"/opt/homebrew/bin/fish", "-l", "-c", "exec $argv", "codex", "--flag", "a prompt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fish wrap:\n got=%#v\nwant=%#v", got, want)
	}
	assertExactCapacity(t, got)
}

func TestWrap_PosixUsesAtAt(t *testing.T) {
	got := Wrap("/bin/bash", []string{"-l", "-c"}, []string{"claude", "--print"})
	wantBody := `if [ -r "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi; exec "$@"`
	if len(got) < 4 {
		t.Fatalf("bash wrap too short: got=%#v", got)
	}
	if got[3] != wantBody {
		t.Fatalf("bash command body:\n got=%q\nwant=%q", got[3], wantBody)
	}
	if strings.Contains(got[3], `\`) {
		t.Fatalf("bash command body must not contain literal backslashes: %q bytes=%v", got[3], []byte(got[3]))
	}
	want := []string{"/bin/bash", "-l", "-c", wantBody, "bash", "claude", "--print"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bash wrap:\n got=%#v\nwant=%#v", got, want)
	}
	assertExactCapacity(t, got)
}

func TestWrap_FishManyArgsPreservesEveryArg(t *testing.T) {
	// A command with several arguments must round-trip into $argv in order.
	// This also pins the fish branch's slice sizing: an undersized (or
	// negative) capacity hint would either misbuild or panic on longer argv.
	argv := []string{"codex", "--model", "gpt", "--flag", "a prompt"}
	got := Wrap("/opt/homebrew/bin/fish", []string{"-l", "-c"}, argv)
	want := []string{"/opt/homebrew/bin/fish", "-l", "-c", "exec $argv", "codex", "--model", "gpt", "--flag", "a prompt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fish wrap (many args):\n got=%#v\nwant=%#v", got, want)
	}
	assertExactCapacity(t, got)
}

func TestWrap_PosixManyArgsPreservesEveryArg(t *testing.T) {
	// POSIX sh keeps the throwaway $0 label plus every argv entry. A longer
	// command pins the branch's slice sizing the same way the fish case does.
	argv := []string{"claude", "--print", "--model", "opus", "--setting", "x"}
	got := Wrap("/bin/sh", []string{"-l", "-c"}, argv)
	want := []string{"/bin/sh", "-l", "-c", `exec "$@"`, "sh", "claude", "--print", "--model", "opus", "--setting", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sh wrap (many args):\n got=%#v\nwant=%#v", got, want)
	}
	assertExactCapacity(t, got)
}

func TestFlags_ZshUsesInteractiveLoginShell(t *testing.T) {
	got := Flags("/bin/zsh")
	want := []string{"-l", "-i", "-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zsh flags:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestFlags_FishUsesInteractiveLoginShell(t *testing.T) {
	// fish loads version managers (nodenv/asdf/mise) only under
	// `status --is-interactive`, so the launch wrap must pass -i or the agent
	// shims never land on PATH and the pane dies with exit 127.
	got := Flags("/opt/homebrew/bin/fish")
	want := []string{"-l", "-i", "-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fish flags:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestFlags_BashUsesNonInteractiveLoginShell(t *testing.T) {
	// bash sources ~/.bashrc explicitly via CommandLine, so it does not need -i.
	got := Flags("/bin/bash")
	want := []string{"-l", "-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bash flags:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestCommandLine_BashSourcesBashrc(t *testing.T) {
	got := CommandLine("/bin/bash", "command -v codex")
	want := `if [ -r "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi; command -v codex`
	if got != want {
		t.Fatalf("bash command line:\n got=%q\nwant=%q", got, want)
	}
}

func TestWrap_EmptyShellReturnsArgvUnchanged(t *testing.T) {
	argv := []string{"codex", "--flag"}
	got := Wrap("", []string{"-l", "-c"}, argv)
	if !reflect.DeepEqual(got, argv) {
		t.Fatalf("empty shell must passthrough: got=%#v", got)
	}
}

func TestWrap_UnsupportedShellReturnsArgvUnchanged(t *testing.T) {
	for _, shell := range []string{"/bin/csh", "/bin/tcsh"} {
		t.Run(shell, func(t *testing.T) {
			argv := []string{"codex", "--flag"}
			got := Wrap(shell, []string{"-l", "-c"}, argv)
			if !reflect.DeepEqual(got, argv) {
				t.Fatalf("unsupported shell must passthrough: got=%#v", got)
			}
		})
	}
}

func TestWrap_EmptyArgvReturnsNil(t *testing.T) {
	if got := Wrap("/bin/bash", []string{"-l", "-c"}, nil); got != nil {
		t.Fatalf("empty argv must return nil, got=%#v", got)
	}
}

func TestWrapReadingFile_PosixPassesPathAsFirstPositional(t *testing.T) {
	got, ok := WrapReadingFile("/bin/zsh", []string{"-l", "-i", "-c"}, []string{"claude", "--"}, "/logs/x.input")
	if !ok {
		t.Fatal("zsh must support WrapReadingFile")
	}
	if len(got) != 9 {
		t.Fatalf("zsh wrap length: got=%#v", got)
	}
	if got[5] != "zsh" || got[6] != "/logs/x.input" || got[7] != "claude" {
		t.Fatalf("zsh positional layout: got=%#v", got)
	}
	body := got[4]
	for _, want := range []string{`shift`, `command cat -- "$__boss_input_file"`, `exec "$@" "$__boss_input"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("zsh body missing %q: %q", want, body)
		}
	}
	assertExactCapacity(t, got)
}

func TestWrapReadingFile_BashSourcesBashrcFirst(t *testing.T) {
	got, ok := WrapReadingFile("/bin/bash", []string{"-l", "-c"}, []string{"claude"}, "/p")
	if !ok {
		t.Fatal("bash must support WrapReadingFile")
	}
	if !strings.HasPrefix(got[3], `if [ -r "$HOME/.bashrc" ]`) {
		t.Fatalf("bash body must source .bashrc first: %q", got[3])
	}
	assertExactCapacity(t, got)
}

func TestWrapReadingFile_FishErasesPathFromArgv(t *testing.T) {
	got, ok := WrapReadingFile("/opt/homebrew/bin/fish", []string{"-l", "-i", "-c"}, []string{"codex", "--"}, "/p")
	if !ok {
		t.Fatal("fish must support WrapReadingFile")
	}
	if got[5] != "/p" || got[6] != "codex" || got[7] != "--" {
		t.Fatalf("fish positional layout: got=%#v", got)
	}
	for _, want := range []string{`set -e argv[1]`, `command cat -- $__boss_input_file | string collect`, `exec $argv $__boss_input`} {
		if !strings.Contains(got[4], want) {
			t.Fatalf("fish body missing %q: %q", want, got[4])
		}
	}
	assertExactCapacity(t, got)
}

func TestWrapReadingFile_UnsupportedReportsNotOK(t *testing.T) {
	for name, tc := range map[string]struct {
		shell, path string
		argv        []string
	}{
		"empty shell": {"", "/p", []string{"claude"}},
		"csh":         {"/bin/csh", "/p", []string{"claude"}},
		"empty path":  {"/bin/bash", "", []string{"claude"}},
		"empty argv":  {"/bin/bash", "/p", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := WrapReadingFile(tc.shell, []string{"-l", "-c"}, tc.argv, tc.path); ok || got != nil {
				t.Fatalf("want (nil,false), got (%#v,%v)", got, ok)
			}
		})
	}
}

func TestWrapInitialInput_WritesPrivateFileAndKeepsPayloadOutOfArgv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	// A pre-existing world-readable file must still end up 0600.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := WrapInitialInput("/bin/sh", []string{"-l", "-c"}, []string{"claude", "--"}, "secret prompt", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if strings.Contains(a, "secret prompt") {
			t.Fatalf("payload inlined into argv: %#v", got)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("input file mode = %o, want 600", perm)
	}
	if b, _ := os.ReadFile(path); string(b) != "secret prompt" {
		t.Fatalf("input file content = %q", b)
	}
}

func TestWrapInitialInput_UnsupportedShellInlinesWithoutFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	got, err := WrapInitialInput("", nil, []string{"claude", "--"}, "hi", path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude", "--", "hi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inline fallback: got=%#v want=%#v", got, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("inline fallback must not write the file; stat err=%v", err)
	}
}

func TestWrapInitialInput_EmptyInputIsPlainWrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	got, err := WrapInitialInput("/bin/sh", []string{"-l", "-c"}, []string{"claude"}, "", path)
	if err != nil {
		t.Fatal(err)
	}
	if want := Wrap("/bin/sh", []string{"-l", "-c"}, []string{"claude"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty input: got=%#v want=%#v", got, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty input must not write the file; stat err=%v", err)
	}
}

func assertExactCapacity(t *testing.T, got []string) {
	t.Helper()
	if cap(got) != len(got) {
		t.Fatalf("wrapped argv capacity = %d, want exact length %d", cap(got), len(got))
	}
}
