package loginshell

import (
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

func assertExactCapacity(t *testing.T, got []string) {
	t.Helper()
	if cap(got) != len(got) {
		t.Fatalf("wrapped argv capacity = %d, want exact length %d", cap(got), len(got))
	}
}
