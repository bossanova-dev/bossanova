package loginshell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realShellPayload carries every byte class a shell could mangle: both quote
// kinds, $ expansions, backticks, a backslash, internal newlines and tabs, a
// leading '-' (option-looking), and trailing newlines that command
// substitution is documented to strip.
const realShellPayload = "-n leading dash 'single' \"double\" $HOME ${HOME} `echo pwned` $(echo sub) \\back\\slash\n" +
	"second line\twith tab\n\n  indented third ; | & * ? [x] ~\n\n"

const hijackMarker = "HIJACKED-BY-RC"

type rcVariant struct {
	name  string
	posix string // ~/.profile, ~/.bash_profile, ~/.bashrc, $ZDOTDIR/.zshrc
	fish  string // $XDG_CONFIG_HOME/fish/config.fish
}

var rcVariants = []rcVariant{
	{
		name:  "function",
		posix: "cat() { echo " + hijackMarker + "; }\n",
		fish:  "function cat; echo " + hijackMarker + "; end\n",
	},
	{
		name:  "alias",
		posix: "alias cat='echo " + hijackMarker + "'\n",
		fish:  "alias cat 'echo " + hijackMarker + "'\n",
	},
}

// shellHome builds an isolated HOME whose rc files (for every supported shell)
// define cat according to v, and returns the environment to run shells in.
func shellHome(t *testing.T, v rcVariant) []string {
	t.Helper()
	home := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".profile", v.posix)
	write(".bash_profile", v.posix)
	write(".bashrc", "shopt -s expand_aliases\n"+v.posix)
	write(".zshrc", v.posix)
	write(".zprofile", "")
	write(".config/fish/config.fish", v.fish)
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"ZDOTDIR=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"TERM=dumb",
	}
}

func lookShell(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed: %v", name, err)
	}
	return p
}

var realShells = []string{"sh", "bash", "zsh", "fish"}

// recorderArgv is a command that writes its final argument, byte-for-byte, to
// out. It uses /bin/sh's printf builtin with no rc loading, so nothing the
// user's shell defines can touch the recording.
func recorderArgv(out string) []string {
	return []string{"/bin/sh", "-c", `printf '%s' "$1" > "$0"`, out}
}

func runShell(t *testing.T, env, argv []string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run %v: %v\noutput:\n%s", argv[:2], err, out)
	}
}

func TestWrapInitialInput_RealShellsDeliverPayloadByteForByte(t *testing.T) {
	want := strings.TrimRight(realShellPayload, "\n")
	for _, name := range realShells {
		for _, v := range rcVariants {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				shell := lookShell(t, name)
				env := shellHome(t, v)
				dir := t.TempDir()
				out := filepath.Join(dir, "out")
				inputPath := filepath.Join(dir, "initial-input")

				argv, err := WrapInitialInput(shell, Flags(shell), recorderArgv(out), realShellPayload, inputPath)
				if err != nil {
					t.Fatalf("WrapInitialInput: %v", err)
				}
				for _, a := range argv {
					if strings.Contains(a, "second line") {
						t.Fatalf("payload leaked into argv: %q", a)
					}
				}
				runShell(t, env, argv)

				got, err := os.ReadFile(out)
				if err != nil {
					t.Fatalf("read recorded argument: %v", err)
				}
				if string(got) != want {
					t.Fatalf("delivered payload mismatch\n got=%q\nwant=%q", got, want)
				}
				if _, err := os.Stat(inputPath); !os.IsNotExist(err) {
					t.Fatalf("initial input file must be removed once read, stat err=%v", err)
				}
			})
		}
	}
}

// TestWrapInitialInput_RealShellsRCDefinedCatIsActive proves the rc hijack the
// payload test defends against is real: the same shells, flags and rc files
// route a bare `cat` to the user's function. Without this control the
// byte-for-byte test could pass simply because no rc was loaded.
func TestWrapInitialInput_RealShellsRCDefinedCatIsActive(t *testing.T) {
	v := rcVariants[0] // function: honoured by every shell in a -c body
	for _, name := range realShells {
		t.Run(name, func(t *testing.T) {
			shell := lookShell(t, name)
			env := shellHome(t, v)
			argv := append([]string{shell}, Flags(shell)...)
			argv = append(argv, CommandLine(shell, "cat /dev/null"))
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = env
			cmd.Dir = t.TempDir()
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), hijackMarker) {
				t.Fatalf("control: rc-defined cat was not active in %s; output=%q", name, out)
			}
		})
	}
}

func TestWrapInitialInput_RealShellLargePayloadThroughFile(t *testing.T) {
	shell := lookShell(t, "bash")
	env := shellHome(t, rcVariant{name: "none"})
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	inputPath := filepath.Join(dir, "initial-input")
	payload := strings.Repeat("0123456789abcdef", 40*1024/16) // 40 KiB, one line

	argv, err := WrapInitialInput(shell, Flags(shell), recorderArgv(out), payload, inputPath)
	if err != nil {
		t.Fatalf("WrapInitialInput: %v", err)
	}
	size := 0
	for _, a := range argv {
		size += len(a) + 1
	}
	if size > 2048 {
		t.Fatalf("argv carries %d bytes; the payload must stay in the file", size)
	}
	runShell(t, env, argv)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("40 KiB payload not delivered intact: got %d bytes", len(got))
	}
}

func TestWrapInitialInput_RealShellMissingFileFailsInsteadOfLaunching(t *testing.T) {
	for _, name := range []string{"bash", "fish"} {
		t.Run(name, func(t *testing.T) {
			shell := lookShell(t, name)
			env := shellHome(t, rcVariant{name: "none"})
			dir := t.TempDir()
			out := filepath.Join(dir, "out")
			argv, ok := WrapReadingFile(shell, Flags(shell), recorderArgv(out), filepath.Join(dir, "missing"))
			if !ok {
				t.Fatal("WrapReadingFile refused a supported shell")
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = env
			if err := cmd.Run(); err == nil {
				t.Fatal("a missing input file must fail the shell, not launch with an empty argument")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("command must not have run; stat err=%v", err)
			}
		})
	}
}
