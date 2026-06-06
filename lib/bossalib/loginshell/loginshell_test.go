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
}

func TestFlags_ZshUsesInteractiveLoginShell(t *testing.T) {
	got := Flags("/bin/zsh")
	want := []string{"-l", "-i", "-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zsh flags:\n got=%#v\nwant=%#v", got, want)
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
