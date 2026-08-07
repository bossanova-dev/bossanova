package remotehost

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// remoteCommand returns the single remote command string of the one recorded
// invocation: the upload helpers send `-o BatchMode=yes`, the destination (see
// the package doc), and exactly one remaining element — the shell snippet the
// remote shell re-parses. Asserting on that one string is what makes the quoting
// boundary testable.
//
// BatchMode is asserted here rather than in one test because every upload helper
// runs while an attached agent owns the terminal in raw mode: an ssh credential
// prompt there reads /dev/tty, paints over the agent, and cannot be answered.
// Routing every caller through this helper means a new one cannot forget it.
func remoteCommand(t *testing.T, rec []recordedCmd) string {
	t.Helper()
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if rec[0].name != "ssh" {
		t.Fatalf("binary = %q, want ssh", rec[0].name)
	}
	wantOptions := strings.Join(batchModeOptions, " ")
	if len(rec[0].args) != len(batchModeOptions)+2 {
		t.Fatalf("argv = %q, want [%s, destination, remote-command]", rec[0].args, wantOptions)
	}
	if got := strings.Join(rec[0].args[:len(batchModeOptions)], " "); got != wantOptions {
		t.Fatalf("argv = %q leads with %q, want %q: an ssh credential prompt behind an "+
			"attached pane reads /dev/tty and cannot be answered", rec[0].args, got, wantOptions)
	}
	return rec[0].args[len(batchModeOptions)+1]
}

// remoteDestination returns the destination element of the one recorded
// invocation — the element after the ssh options, which is where ssh stops
// parsing them.
func remoteDestination(t *testing.T, rec []recordedCmd) string {
	t.Helper()
	if len(rec) != 1 || len(rec[0].args) < len(batchModeOptions)+1 {
		t.Fatalf("argv = %q, want options + destination + remote-command", rec)
	}
	return rec[0].args[len(batchModeOptions)]
}

// remoteSnippet asserts the emitted remote command is a posixShell wrapper and
// returns the POSIX body inside it, exactly inverting shellQuote. Going through
// this rather than matching on the raw string means every caller also proves the
// outer layer is well-formed, which is the half the fish bug broke.
func remoteSnippet(t *testing.T, rec []recordedCmd) string {
	t.Helper()
	cmd := remoteCommand(t, rec)
	const prefix = "sh -c "
	if !strings.HasPrefix(cmd, prefix) {
		t.Fatalf("remote command %q is not routed through an explicit POSIX shell; "+
			"the remote LOGIN shell may be fish, which cannot parse the body", cmd)
	}
	body := strings.TrimPrefix(cmd, prefix)
	if len(body) < 2 || !strings.HasPrefix(body, "'") || !strings.HasSuffix(body, "'") {
		t.Fatalf("remote command body %q is not a single-quoted word", body)
	}
	return strings.ReplaceAll(body[1:len(body)-1], `'\''`, "'")
}

// shellsUnderTest returns the shells present on this machine to run an emitted
// command through. /bin/sh is mandatory; the others are opportunistic, and a
// missing one is reported rather than silently passing.
func shellsUnderTest(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{"sh": "/bin/sh"}
	for _, name := range []string{"bash", "zsh", "fish"} {
		if p, err := exec.LookPath(name); err == nil {
			found[name] = p
		} else {
			t.Logf("%s is not installed here; skipping it (CI coverage for it is not claimed)", name)
		}
	}
	return found
}

// runEmitted runs an emitted remote command under the given shell with a
// throwaway $HOME, the way sshd would hand it to the remote login shell.
func runEmitted(t *testing.T, shell, command, home string, stdin io.Reader) (string, error) {
	t.Helper()
	cmd := exec.Command(shell, "-c", command) // #nosec G204 -- the command under test; owner=@recurser review-by=2027-02-07 issue=BOS-715
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// writeTempFile creates a local file with the given basename and contents and
// returns its absolute path.
func writeTempFile(t *testing.T, name, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestEnsureUploadDirReturnsTheRemotePathFromOneInvocation(t *testing.T) {
	const dest = "deploy@bastion.example.com"
	var rec []recordedCmd
	got, err := EnsureUploadDir(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "echo /home/dev/.cache/boss/uploads/abc", &rec),
	}, "abc")
	if err != nil {
		t.Fatalf("EnsureUploadDir: %v", err)
	}
	if got != "/home/dev/.cache/boss/uploads/abc" {
		t.Fatalf("dir = %q", got)
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if got := remoteDestination(t, rec); got != dest {
		t.Fatalf("destination = %q, want %q verbatim", got, dest)
	}
	snippet := remoteSnippet(t, rec)
	// The remote side must resolve $HOME itself — the caller needs the literal
	// path to hand to the agent, so the snippet has to print it back.
	for _, want := range []string{"XDG_CACHE_HOME", "$HOME/.cache", "mkdir -p -m 700", "printf"} {
		if !strings.Contains(snippet, want) {
			t.Fatalf("remote snippet %q omits %q", snippet, want)
		}
	}
}

func TestEnsureUploadDirTakesTheLastLinePastALoginBanner(t *testing.T) {
	// A chatty /etc/motd or a shell rc that prints on every non-interactive
	// login lands on stdout ahead of our printf.
	var rec []recordedCmd
	got, err := EnsureUploadDir(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "printf 'Welcome to prod\\n/home/dev/.cache/boss/uploads/abc\\n'", &rec),
	}, "abc")
	if err != nil {
		t.Fatalf("EnsureUploadDir: %v", err)
	}
	if got != "/home/dev/.cache/boss/uploads/abc" {
		t.Fatalf("dir = %q, want the banner stripped", got)
	}
}

func TestEnsureUploadDirRejectsAPathThatIsNotAbsolute(t *testing.T) {
	// Anything that is not an absolute path means the snippet did not run: an
	// empty $HOME, a banner-only stdout, or a wrapper that ate the printf.
	// Returning it anyway would inject a relative path into the agent's prompt.
	cases := map[string]string{
		"empty":       "true",
		"blankLines":  "printf '\\n\\n'",
		"bannerOnly":  "echo 'Welcome to prod'",
		"relative":    "echo .cache/boss/uploads/abc",
		"tildeUnexpd": "echo '~/.cache/boss/uploads/abc'",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			const dest = "user@host"
			var rec []recordedCmd
			got, err := EnsureUploadDir(context.Background(), Options{
				Destination:    dest,
				CommandFactory: fakeSSH(t, script, &rec),
			}, "abc")
			if err == nil {
				t.Fatalf("want an error, got dir %q", got)
			}
			if !strings.Contains(err.Error(), dest) {
				t.Fatalf("err %q does not name the destination", err)
			}
		})
	}
}

func TestEnsureUploadDirQuotesTheSessionKey(t *testing.T) {
	// The session key is data inside a command string TWO shells re-parse (the
	// remote login shell, then the sh -c body), so it is both reduced to a safe
	// character class and single-quoted. Either alone is not enough: quoting
	// alone does not survive fish, and the class alone would not survive a
	// future snippet that stopped quoting.
	const hostile = `a'; rm -rf ~; echo '`
	var rec []recordedCmd
	if _, err := EnsureUploadDir(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "echo /home/dev/.cache/boss/uploads/"+sanitizeSessionKey(hostile), &rec),
	}, hostile); err != nil {
		t.Fatalf("EnsureUploadDir: %v", err)
	}
	cmd := remoteCommand(t, rec)
	snippet := remoteSnippet(t, rec)
	if !strings.Contains(snippet, shellQuote(sanitizeSessionKey(hostile))) {
		t.Fatalf("remote snippet %q does not carry the sanitised, shell-quoted session key", snippet)
	}
	// The falsifiable half: no part of the key may survive as syntax, in either
	// the outer command or the inner snippet.
	for _, danger := range []string{"rm -rf", ";", "'", `\`} {
		if strings.Contains(strings.TrimPrefix(cmd, "sh -c "), "uploads/"+danger) {
			t.Fatalf("remote command %q splices session-key syntax after the prefix", cmd)
		}
	}
	if strings.Contains(cmd, "rm -rf ~") {
		t.Fatalf("remote command %q still carries the key's command substring", cmd)
	}
}

// TestEnsureUploadDirRunsUnderEveryRemoteLoginShell runs the EXACT emitted
// command through each shell installed here, with a throwaway $HOME, the way
// sshd hands a command to the remote user's login shell.
//
// This is the test that catches the whole class. Two real bugs were only visible
// by executing the command rather than matching its text: BSD/macOS `chmod`
// rejects `--`, and fish parses neither `dir=value` ("Unsupported use of '='")
// nor `${VAR:-default}` — it exits 127 on both, so an unwrapped snippet failed
// on every fish remote. Worse, an unsanitised key that closed a quote actually
// EXECUTED `rm -rf ~` under a fish outer shell, because POSIX's `'\”` escape
// does not round-trip through fish's single-quote rules. Hence the canary.
func TestEnsureUploadDirRunsUnderEveryRemoteLoginShell(t *testing.T) {
	keys := map[string]struct{ in, wantComponent string }{
		"plain":     {"sess-abc123", "sess-abc123"},
		"quoteBomb": {`a'; rm -rf ~; echo '`, "a___rm_-rf____echo__"},
		"backslash": {`back\slash`, "back_slash"},
		"bang":      {"!bang", "_bang"},
		"dollar":    {"$(rm -rf ~)", "__rm_-rf___"},
	}
	for shellName, shellPath := range shellsUnderTest(t) {
		for keyName, key := range keys {
			t.Run(shellName+"/"+keyName, func(t *testing.T) {
				var rec []recordedCmd
				// The fake must echo the dir the real snippet would print:
				// EnsureUploadDir now pins the reported path to the tail it
				// asked for, so an implausible stub is no longer accepted.
				if _, err := EnsureUploadDir(context.Background(), Options{
					Destination:    "user@host",
					CommandFactory: fakeSSH(t, "echo /home/dev"+uploadDirSuffix+key.wantComponent, &rec),
				}, key.in); err != nil {
					t.Fatalf("EnsureUploadDir: %v", err)
				}
				command := remoteCommand(t, rec)

				home := t.TempDir()
				canary := filepath.Join(home, "precious.txt")
				if err := os.WriteFile(canary, []byte("do not delete"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}

				out, err := runEmitted(t, shellPath, command, home, nil)
				if err != nil {
					t.Fatalf("%s could not run the emitted command: %v (%s)", shellName, err, out)
				}
				if _, statErr := os.Stat(canary); statErr != nil {
					t.Fatalf("the session key EXECUTED under %s: canary gone (%v)", shellName, statErr)
				}
				want := filepath.Join(home, ".cache", "boss", "uploads", key.wantComponent)
				if out != want {
					t.Fatalf("%s printed %q, want %q", shellName, out, want)
				}
				info, err := os.Stat(want)
				if err != nil {
					t.Fatalf("%s did not create the upload dir: %v", shellName, err)
				}
				if perm := info.Mode().Perm(); perm != 0o700 {
					t.Fatalf("upload dir mode = %o, want 700 (another user on that host could read it)", perm)
				}
			})
		}
	}
}

// TestUploadFileRunsUnderEveryRemoteLoginShell proves the file's bytes still
// reach the inner `cat` through the `sh -c` layer — sh inherits the ssh
// channel's stdin — under every login shell installed here, and that a hostile
// basename cannot execute in either parse.
func TestUploadFileRunsUnderEveryRemoteLoginShell(t *testing.T) {
	const contents = "\x89PNG\r\n\x1a\n binary payload \x00\xff"
	names := []string{"screenshot.png", `a'; rm -rf ~; echo '.png`, "Screen Shot 2026.png"}
	for shellName, shellPath := range shellsUnderTest(t) {
		for _, name := range names {
			t.Run(shellName+"/"+strings.NewReplacer("'", "q", " ", "_", ";", "s").Replace(name), func(t *testing.T) {
				local := writeTempFile(t, "screenshot.png", contents)
				home := t.TempDir()
				canary := filepath.Join(home, "precious.txt")
				if err := os.WriteFile(canary, []byte("do not delete"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				// The remote dir is a real local directory here, so the emitted
				// command's effects are observable. It carries a space because a
				// remote $HOME may: validateRemoteDir allows one, so the mkdir
				// and the redirect have to survive it in every dialect.
				remoteDir := filepath.Join(home, "Dave Perrett", "uploads", "sess")
				if err := os.MkdirAll(remoteDir, 0o700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}

				var rec []recordedCmd
				remotePath, err := UploadFile(context.Background(), Options{
					Destination:    "user@host",
					CommandFactory: fakeSSH(t, "true", &rec),
				}, local, remoteDir)
				if err != nil {
					t.Fatalf("UploadFile: %v", err)
				}
				command := remoteCommand(t, rec)

				file, err := os.Open(local)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer func() { _ = file.Close() }()
				if out, runErr := runEmitted(t, shellPath, command, home, file); runErr != nil {
					t.Fatalf("%s could not run the emitted command: %v (%s)", shellName, runErr, out)
				}

				if _, statErr := os.Stat(canary); statErr != nil {
					t.Fatalf("the basename EXECUTED under %s: canary gone (%v)", shellName, statErr)
				}
				landed, err := os.ReadFile(remotePath) // #nosec G304 -- path produced by the code under test; owner=@recurser review-by=2027-02-07 issue=BOS-715
				if err != nil {
					t.Fatalf("%s did not write the upload to the returned path %q: %v", shellName, remotePath, err)
				}
				if string(landed) != contents {
					t.Fatalf("%s landed %d bytes, want the file's %d (stdin did not reach the inner cat)",
						shellName, len(landed), len(contents))
				}
			})
		}
	}
}

// TestRemoveUploadDirRunsUnderEveryRemoteLoginShell closes the loop: the dir the
// other two create must actually be removable by the emitted command.
func TestRemoveUploadDirRunsUnderEveryRemoteLoginShell(t *testing.T) {
	for shellName, shellPath := range shellsUnderTest(t) {
		t.Run(shellName, func(t *testing.T) {
			home := t.TempDir()
			// A space in the path is the one metacharacter validateRemoteDir
			// still allows, and remote home directories really do contain one,
			// so the emitted command has to survive it in every dialect.
			remoteDir := filepath.Join(home, "Dave Perrett", "uploads", "sess")
			if err := os.MkdirAll(filepath.Join(remoteDir, "abc123"), 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			var rec []recordedCmd
			if err := RemoveUploadDir(context.Background(), Options{
				Destination:    "user@host",
				CommandFactory: fakeSSH(t, "true", &rec),
			}, remoteDir); err != nil {
				t.Fatalf("RemoveUploadDir: %v", err)
			}
			out, err := runEmitted(t, shellPath, remoteCommand(t, rec), home, nil)
			if err != nil {
				t.Fatalf("%s could not run the emitted command: %v (%s)", shellName, err, out)
			}
			if _, statErr := os.Stat(remoteDir); !os.IsNotExist(statErr) {
				t.Fatalf("%s left the upload dir behind (%v)", shellName, statErr)
			}
		})
	}
}

func TestEnsureUploadDirEmptyDestination(t *testing.T) {
	var rec []recordedCmd
	_, err := EnsureUploadDir(context.Background(), Options{
		CommandFactory: fakeSSH(t, "echo /home/dev/.cache/boss/uploads/abc", &rec),
	}, "abc")
	if !errors.Is(err, ErrNoDestination) {
		t.Fatalf("err = %v, want ErrNoDestination", err)
	}
	if len(rec) != 0 {
		t.Fatalf("ran %d commands, want 0", len(rec))
	}
}

func TestUploadFileStreamsTheFileOnStdin(t *testing.T) {
	const contents = "\x89PNG\r\n\x1a\n not really a png but binary enough\x00\xff"
	local := writeTempFile(t, "screenshot.png", contents)
	capture := filepath.Join(t.TempDir(), "stdin.bin")

	const dest = "user@host"
	const remoteDir = "/home/dev/.cache/boss/uploads/sess"
	var rec []recordedCmd
	got, err := UploadFile(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "cat > "+shellQuote(capture), &rec),
	}, local, remoteDir)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if got := remoteDestination(t, rec); got != dest {
		t.Fatalf("destination = %q, want %q verbatim", got, dest)
	}
	sent, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("reading the captured stdin: %v", err)
	}
	if string(sent) != contents {
		t.Fatalf("stdin carried %d bytes, want the file's %d", len(sent), len(contents))
	}
	if !strings.HasPrefix(got, remoteDir+"/") {
		t.Fatalf("remote path %q is not under %q", got, remoteDir)
	}
	if path.Base(got) != "screenshot.png" {
		t.Fatalf("remote path %q lost the recognisable basename", got)
	}
	snippet := remoteSnippet(t, rec)
	if !strings.Contains(snippet, shellQuote(got)) {
		t.Fatalf("remote snippet %q does not write to the returned path", snippet)
	}
	if !strings.Contains(snippet, "mkdir -p -m 700") {
		t.Fatalf("remote snippet %q does not create a private per-upload dir", snippet)
	}
}

func TestUploadFileGivesEachUploadItsOwnSubdirectory(t *testing.T) {
	// Two pastes of the same filename must not clobber each other, so the
	// uniqueness lives in a subdirectory rather than in the basename.
	const remoteDir = "/home/dev/.cache/boss/uploads/sess"
	local := writeTempFile(t, "screenshot.png", "one")
	opts := Options{Destination: "user@host"}
	var rec []recordedCmd
	opts.CommandFactory = fakeSSH(t, "true", &rec)

	first, err := UploadFile(context.Background(), opts, local, remoteDir)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	second, err := UploadFile(context.Background(), opts, local, remoteDir)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if first == second {
		t.Fatalf("both uploads landed on %q; the second would clobber the first", first)
	}
	if path.Base(first) != path.Base(second) {
		t.Fatalf("basenames differ (%q vs %q); the agent should see the original name", first, second)
	}
	for _, p := range []string{first, second} {
		if path.Dir(path.Dir(p)) != remoteDir {
			t.Fatalf("remote path %q is not <remoteDir>/<unique>/<name>", p)
		}
	}
}

func TestUploadFileHostileBasenameStaysUnderRemoteDir(t *testing.T) {
	// Names a local filesystem will actually accept; the ones it will not
	// (a separator, "..", a NUL) are covered by TestSanitizeRemoteBasename.
	names := []string{
		"...",
		"..hidden",
		"-rf",
		"--checkpoint-action=exec=sh",
		"a\nb.png",
		"a\tb.png",
	}
	const remoteDir = "/home/dev/.cache/boss/uploads/sess"
	for _, name := range names {
		t.Run(strings.ReplaceAll(name, "\n", `\n`), func(t *testing.T) {
			local := writeTempFile(t, name, "payload")
			var rec []recordedCmd
			got, err := UploadFile(context.Background(), Options{
				Destination:    "user@host",
				CommandFactory: fakeSSH(t, "true", &rec),
			}, local, remoteDir)
			if err != nil {
				t.Fatalf("UploadFile(%q): %v", name, err)
			}
			if !strings.HasPrefix(got, remoteDir+"/") {
				t.Fatalf("remote path %q escaped %q", got, remoteDir)
			}
			if got != path.Clean(got) {
				t.Fatalf("remote path %q is not already clean, so it can traverse", got)
			}
			if strings.ContainsAny(got, "\n\t") {
				t.Fatalf("remote path %q carries a control character", got)
			}
		})
	}
}

func TestSanitizeRemoteBasename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"screenshot.png", "screenshot.png"},
		{"/etc/authorized_keys", "authorized_keys"},
		{"../../etc/authorized_keys", "authorized_keys"},
		{`..\..\windows\system32`, "system32"},
		{"..", "upload"},
		{".", "upload"},
		{"", "upload"},
		{"   ", "upload"},
		{"a/b.png", "b.png"},
		{"a\nb.png", "a_b.png"},
		{"a\x00b.png", "a_b.png"},
		{"-rf", "_rf"},
		{"--checkpoint", "_-checkpoint"},
		// A space becomes "_": the name is injected into the agent's PROMPT as
		// bare text, where a space is a token break that truncates the path.
		{"Screen Shot 2026-08-06 at 10.11.12.png", "Screen_Shot_2026-08-06_at_10.11.12.png"},
		// Shell syntax is replaced, not quoted away: the name crosses TWO shell
		// parses of an unknown dialect, and POSIX quoting does not survive fish.
		{"a'b.png", "a_b.png"},
		{`a"b.png`, "a_b.png"},
		{"$(rm -rf ~).png", "__rm_-rf___.png"},
		{"`id`.png", "_id_.png"},
		{"!bang.png", "_bang.png"},
		{"a*b?.png", "a_b_.png"},
	}
	for _, tc := range cases {
		if got := sanitizeRemoteBasename(tc.in); got != tc.want {
			t.Fatalf("sanitizeRemoteBasename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Whatever comes back must never traverse, start a flag, or carry a
	// character with meaning to any shell.
	for _, tc := range cases {
		assertSafeComponent(t, "sanitizeRemoteBasename", tc.in, sanitizeRemoteBasename(tc.in))
	}
}

func TestSanitizeSessionKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sess-abc123", "sess-abc123"},
		{"a4f2.9c", "a4f2.9c"},
		{`a'; rm -rf ~; echo '`, "a___rm_-rf____echo__"},
		{`back\slash`, "back_slash"},
		{"!bang", "_bang"},
		{"$(rm -rf ~)", "__rm_-rf___"},
		{"with space", "with_space"},
		{"/etc/passwd", "_etc_passwd"},
		{"..", "upload"},
		{"-lead", "_lead"},
	}
	for _, tc := range cases {
		if got := sanitizeSessionKey(tc.in); got != tc.want {
			t.Fatalf("sanitizeSessionKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
		assertSafeComponent(t, "sanitizeSessionKey", tc.in, sanitizeSessionKey(tc.in))
	}
}

// assertSafeComponent is the invariant both sanitisers must hold: a single path
// component made only of characters that are inert in every shell AND
// self-delimiting in the agent's prompt, which is the other place the resulting
// path is read. No space: the pty layer injects the remote path as bare text, so
// a space there truncates it at the first token break.
func assertSafeComponent(t *testing.T, fn, in, got string) {
	t.Helper()
	if got == "" || got == "." || got == ".." || strings.HasPrefix(got, "-") {
		t.Fatalf("%s(%q) = %q is not a usable component", fn, in, got)
	}
	for _, r := range got {
		if safeRemoteNameRune(r) {
			continue
		}
		if r == ' ' {
			t.Fatalf("%s(%q) = %q keeps a space, which is a token break in the "+
				"agent's prompt — the injected path would truncate there", fn, in, got)
		}
		t.Fatalf("%s(%q) = %q keeps %q, which is not inert across two shell parses", fn, in, got, r)
	}
}

func TestUploadFileRejectsAFileOverTheCap(t *testing.T) {
	local := filepath.Join(t.TempDir(), "huge.png")
	if err := os.WriteFile(local, make([]byte, MaxUploadBytes+1), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var rec []recordedCmd
	_, err := UploadFile(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "true", &rec),
	}, local, "/home/dev/.cache/boss/uploads/sess")
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if len(rec) != 0 {
		t.Fatalf("ran %d commands, want 0 (the cap must be checked before ssh)", len(rec))
	}
}

// TestUploadFileRejectsAFileThatGrowsDuringTheUpload covers the window the
// one-time Stat cannot. A screenshot tool is routinely still writing when the
// user drags the file in, so a size sampled before the stream starts is a fast
// reject, not the enforcement — the stream is what the cap exists to bound.
//
// The factory does the growing because that is precisely when the real race
// happens: after UploadFile has stat-ed and before os/exec copies the bytes.
// Truncate-and-rewrite through the same path keeps the inode, so the descriptor
// UploadFile already holds sees the larger file.
func TestUploadFileRejectsAFileThatGrowsDuringTheUpload(t *testing.T) {
	local := filepath.Join(t.TempDir(), "screenshot.png")
	if err := os.WriteFile(local, make([]byte, MaxUploadBytes), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var rec []recordedCmd
	grow := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec = append(rec, recordedCmd{name: name, args: args, ctx: ctx})
		if err := os.WriteFile(local, make([]byte, MaxUploadBytes+1), 0o600); err != nil {
			t.Fatalf("growing the file mid-upload: %v", err)
		}
		// Drains stdin so the copy completes and Run reports success: the
		// overshoot must be caught on the path where ssh itself said nothing
		// went wrong.
		return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null")
	}

	_, err := UploadFile(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: grow,
	}, local, "/home/dev/.cache/boss/uploads/sess")
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge: a file that outgrew the cap mid-stream "+
			"leaves a truncated remote copy that must not reach the agent", err)
	}
	if len(rec) != 1 {
		t.Fatalf("ran %d commands, want 1", len(rec))
	}
}

func TestUploadFileRejectsBadInputWithoutRunningSSH(t *testing.T) {
	dir := t.TempDir()
	good := writeTempFile(t, "screenshot.png", "payload")
	const remoteDir = "/home/dev/.cache/boss/uploads/sess"

	cases := map[string]struct {
		dest      string
		local     string
		remoteDir string
		wantErrIs error
	}{
		"emptyDestination":   {dest: "", local: good, remoteDir: remoteDir, wantErrIs: ErrNoDestination},
		"missingLocalFile":   {dest: "user@host", local: filepath.Join(dir, "nope.png"), remoteDir: remoteDir},
		"localIsADirectory":  {dest: "user@host", local: dir, remoteDir: remoteDir},
		"emptyRemoteDir":     {dest: "user@host", local: good, remoteDir: ""},
		"relativeRemoteDir":  {dest: "user@host", local: good, remoteDir: "uploads/sess"},
		"tildeRemoteDir":     {dest: "user@host", local: good, remoteDir: "~/.cache/boss/uploads/sess"},
		"emptyLocalPath":     {dest: "user@host", local: "", remoteDir: remoteDir},
		"traversalRemoteDir": {dest: "user@host", local: good, remoteDir: "/home/dev/../../etc"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var rec []recordedCmd
			_, err := UploadFile(context.Background(), Options{
				Destination:    tc.dest,
				CommandFactory: fakeSSH(t, "true", &rec),
			}, tc.local, tc.remoteDir)
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
			}
			if len(rec) != 0 {
				t.Fatalf("ran %d commands, want 0", len(rec))
			}
		})
	}
}

func TestUploadFileFailureSurfacesStderrWithoutLeakingAToken(t *testing.T) {
	local := writeTempFile(t, "screenshot.png", "payload")
	const dest = "user@host"
	var rec []recordedCmd
	_, err := UploadFile(context.Background(), Options{
		Destination: dest,
		// A remote wrapper that traces, so a token-shaped run lands on stderr.
		CommandFactory: fakeSSH(t, `echo "+ cat: no space left on device `+validToken+`" >&2; exit 1`, &rec),
	}, local, "/home/dev/.cache/boss/uploads/sess")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), validToken) {
		t.Fatalf("error leaked a token that arrived on stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err %q should mark where the token was removed", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("err %q omits stderr, making the failure undiagnosable", err)
	}
	if !strings.Contains(err.Error(), dest) {
		t.Fatalf("err %q does not name the destination", err)
	}
}

func TestRemoveUploadDirRemovesTheQuotedDir(t *testing.T) {
	const dest = "user@host"
	// A space is the one metacharacter a remote dir may legitimately carry: it
	// is inert inside single quotes in every shell, and remote home directories
	// really do contain them. A quote or a backslash is a different matter —
	// see TestUploadHelpersRefuseADirTheNestedQuotingCannotCarry.
	const remoteDir = "/Users/Dave Perrett/.cache/boss/uploads/sess"
	var rec []recordedCmd
	if err := RemoveUploadDir(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "true", &rec),
	}, remoteDir); err != nil {
		t.Fatalf("RemoveUploadDir: %v", err)
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if got := remoteDestination(t, rec); got != dest {
		t.Fatalf("destination = %q, want %q verbatim", got, dest)
	}
	snippet := remoteSnippet(t, rec)
	if !strings.HasPrefix(snippet, "rm -r") {
		t.Fatalf("remote snippet %q is not an rm -r", snippet)
	}
	if !strings.Contains(snippet, shellQuote(remoteDir)) {
		t.Fatalf("remote snippet %q does not name the shell-quoted dir", snippet)
	}
}

func TestRemoveUploadDirRefusesADirItCannotVouchFor(t *testing.T) {
	// A bug here runs `rm -rf` on someone else's machine, so the guard must
	// reject before any ssh call rather than "best-effort" its way to a wipe.
	dirs := map[string]string{
		"empty":     "",
		"blank":     "   ",
		"relative":  "uploads/sess",
		"tilde":     "~/.cache/boss/uploads/sess",
		"root":      "/",
		"traversal": "/home/dev/../../",
		"shallow":   "/home",
		"untrimmed": " /home/dev/.cache/boss/uploads/sess ",
		"quote":     "/home/o'brien/.cache/boss/uploads/sess",
		"backslash": `/home/dev\x/.cache/boss/uploads/sess`,
		"newline":   "/home/dev/.cache/boss/uploads/sess\nrm -rf ~",
		// C1: U+009B is CSI in an 8-bit locale, so this drives the terminal with
		// no ESC in the string. The dir is injected into the agent's composer
		// with no sanitiser of its own, so rejecting it here is where it closes.
		"c1": "/home/dev\u009b/.cache/boss/uploads/sess",
		// Undecodable: IndexFunc reads this as RuneError, which is >= 0x20 and
		// would otherwise pass every other check.
		"invalidUTF8": "/home/dev\xff/.cache/boss/uploads/sess",
	}
	for name, dir := range dirs {
		t.Run(name, func(t *testing.T) {
			var rec []recordedCmd
			err := RemoveUploadDir(context.Background(), Options{
				Destination:    "user@host",
				CommandFactory: fakeSSH(t, "true", &rec),
			}, dir)
			if err == nil {
				t.Fatalf("want an error for %q", dir)
			}
			if len(rec) != 0 {
				t.Fatalf("ran %d commands for %q, want 0", len(rec), dir)
			}
		})
	}
}

// TestUploadFileRefusesADirTheNestedQuotingCannotCarry is the other half of
// TestRemoveUploadDirRefusesADirItCannotVouchFor: BOTH callers that splice a
// remote dir into a snippet must reject the class, because both re-quote it
// through posixShell's second layer.
//
// The dir is the only value in this package that reaches a snippet without
// having passed a sanitiser — it is whatever the remote shell printed. A quote
// or a backslash in it breaks the nested single-quoting: POSIX's `'\”` escape
// does not round-trip through fish, which reads the backslash inside the quotes
// as an escape and never closes the string, so a remote $HOME of `/home/o'brien`
// makes every emitted command exit 127 there.
func TestUploadFileRefusesADirTheNestedQuotingCannotCarry(t *testing.T) {
	local := writeTempFile(t, "screenshot.png", "payload")
	dirs := map[string]string{
		"quote":       "/home/o'brien/.cache/boss/uploads/sess",
		"backslash":   `/home/dev\x/.cache/boss/uploads/sess`,
		"newline":     "/home/dev/.cache/boss/uploads/sess\nrm -rf ~",
		"untrimmed":   " /home/dev/.cache/boss/uploads/sess ",
		"c1":          "/home/dev\u009b/.cache/boss/uploads/sess",
		"invalidUTF8": "/home/dev\xff/.cache/boss/uploads/sess",
	}
	for name, dir := range dirs {
		t.Run(name, func(t *testing.T) {
			var rec []recordedCmd
			got, err := UploadFile(context.Background(), Options{
				Destination:    "user@host",
				CommandFactory: fakeSSH(t, "true", &rec),
			}, local, dir)
			if err == nil {
				t.Fatalf("want an error for %q, got remote path %q", dir, got)
			}
			if len(rec) != 0 {
				t.Fatalf("ran %d commands for %q, want 0 — the guard must reject before any ssh", len(rec), dir)
			}
		})
	}
}

// TestEnsureUploadDirRejectsADirItDidNotAskFor pins the reported path to the
// tail the snippet built. Everything downstream re-quotes this value and runs
// it — UploadFile's `mkdir`, RemoveUploadDir's `rm -rf` — so a remote that
// prints something else is a remote whose word we would otherwise take for the
// target of a recursive delete. The tail is fully known to us, so there is no
// reason to accept anything else.
func TestEnsureUploadDirRejectsADirItDidNotAskFor(t *testing.T) {
	const key = "sess-abc123"
	// The PREFIX is deliberately not pinned: only the remote knows its own
	// $HOME/$XDG_CACHE_HOME, which is the whole reason the snippet prints the
	// path back. The tail is what we chose, so the tail is what is checked.
	scripts := map[string]string{
		"wrongKey":     "echo /home/dev/.cache/boss/uploads/other-session",
		"aTopLevelDir": "echo /home/dev",
		"rootAdjacent": "echo /etc",
		"quoteInHome":  "echo \"/home/o'brien/.cache/boss/uploads/" + key + "\"",
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			var rec []recordedCmd
			got, err := EnsureUploadDir(context.Background(), Options{
				Destination:    "user@host",
				CommandFactory: fakeSSH(t, script, &rec),
			}, key)
			if err == nil {
				t.Fatalf("want an error, got dir %q — it would become an `rm -rf` target", got)
			}
		})
	}
}

// TestEnsureUploadDirAcceptsARemoteHomeWithASpace is the falsifiable other side:
// the guard above must not have been tightened into "reject anything unusual".
// A space in $HOME is common and survives single quoting in every shell.
func TestEnsureUploadDirAcceptsARemoteHomeWithASpace(t *testing.T) {
	const key = "sess-abc123"
	const want = "/Users/Dave Perrett/.cache/boss/uploads/" + key
	var rec []recordedCmd
	got, err := EnsureUploadDir(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "echo \""+want+"\"", &rec),
	}, key)
	if err != nil {
		t.Fatalf("EnsureUploadDir: %v", err)
	}
	if got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
}

// TestEnsureUploadDirCleansADoubledSlash covers a remote $XDG_CACHE_HOME with a
// trailing slash: the same directory, but an unclean path, which
// validateRemoteDir would (rightly) refuse on the very next call.
func TestEnsureUploadDirCleansADoubledSlash(t *testing.T) {
	var rec []recordedCmd
	got, err := EnsureUploadDir(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "echo /home/dev/cache//boss/uploads/abc", &rec),
	}, "abc")
	if err != nil {
		t.Fatalf("EnsureUploadDir: %v", err)
	}
	if got != "/home/dev/cache/boss/uploads/abc" {
		t.Fatalf("dir = %q, want the cleaned path", got)
	}
	if err := validateRemoteDir(got); err != nil {
		t.Fatalf("EnsureUploadDir returned a dir its own callers reject: %v", err)
	}
}

func TestRemoveUploadDirEmptyDestination(t *testing.T) {
	var rec []recordedCmd
	err := RemoveUploadDir(context.Background(), Options{
		CommandFactory: fakeSSH(t, "true", &rec),
	}, "/home/dev/.cache/boss/uploads/sess")
	if !errors.Is(err, ErrNoDestination) {
		t.Fatalf("err = %v, want ErrNoDestination", err)
	}
	if len(rec) != 0 {
		t.Fatalf("ran %d commands, want 0", len(rec))
	}
}

func TestUploadHelpersHonourContext(t *testing.T) {
	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "carried")
	ctx, cancel := context.WithCancel(base)
	cancel()

	local := writeTempFile(t, "screenshot.png", "payload")
	const remoteDir = "/home/dev/.cache/boss/uploads/sess"
	opts := func(rec *[]recordedCmd) Options {
		return Options{Destination: "user@host", CommandFactory: fakeSSH(t, "sleep 30", rec)}
	}

	var ensureRec, uploadRec, removeRec []recordedCmd
	if _, err := EnsureUploadDir(ctx, opts(&ensureRec), "sess"); err == nil {
		t.Fatal("EnsureUploadDir: want an error from the cancelled context")
	}
	if _, err := UploadFile(ctx, opts(&uploadRec), local, remoteDir); err == nil {
		t.Fatal("UploadFile: want an error from the cancelled context")
	}
	if err := RemoveUploadDir(ctx, opts(&removeRec), remoteDir); err == nil {
		t.Fatal("RemoveUploadDir: want an error from the cancelled context")
	}
	for name, rec := range map[string][]recordedCmd{
		"EnsureUploadDir": ensureRec,
		"UploadFile":      uploadRec,
		"RemoveUploadDir": removeRec,
	} {
		if len(rec) != 1 {
			t.Fatalf("%s ran %d commands, want 1", name, len(rec))
		}
		if got := rec[0].ctx.Value(ctxKey{}); got != "carried" {
			t.Fatalf("%s factory received ctx value %v, want the caller's context", name, got)
		}
	}
}

// TestUploadHelpersBoundWaitAfterCancellation pins the WaitDelay every upload
// invocation carries. Honouring the context (above) is only half of it: a
// cancelled context kills ssh, but Run can still block afterwards.
//
// Each helper captures stdout and stderr into buffers, so os/exec gives the
// child pipes and copying goroutines, and Run waits until every process holding
// a write end closes it. A descendant that inherited those descriptors — a
// ProxyCommand, a multiplexer — need not exit when its ssh is killed, so
// without this bound Run outlives the cancellation for as long as that
// descendant lives. The callers have no timeout of their own to fall back on:
// PTYCommand.awaitPasteUploads joins its upload goroutines unconditionally,
// deliberately relying on cancellation alone to unwind them, so an unbounded
// Run here freezes the Ctrl+X detach that did the cancelling.
func TestUploadHelpersBoundWaitAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	local := writeTempFile(t, "screenshot.png", "payload")
	const remoteDir = "/home/dev/.cache/boss/uploads/sess"

	// Captures the *exec.Cmd itself rather than the argv: WaitDelay is set on
	// the command the factory handed back, which recordedCmd does not carry.
	capture := func(cmds *[]*exec.Cmd) CommandFactory {
		return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
			*cmds = append(*cmds, cmd)
			return cmd
		}
	}
	optsFor := func(cmds *[]*exec.Cmd) Options {
		return Options{Destination: "user@host", CommandFactory: capture(cmds)}
	}

	var ensureCmds, uploadCmds, removeCmds []*exec.Cmd
	if _, err := EnsureUploadDir(ctx, optsFor(&ensureCmds), "sess"); err == nil {
		t.Fatal("EnsureUploadDir: want an error from the cancelled context")
	}
	if _, err := UploadFile(ctx, optsFor(&uploadCmds), local, remoteDir); err == nil {
		t.Fatal("UploadFile: want an error from the cancelled context")
	}
	if err := RemoveUploadDir(ctx, optsFor(&removeCmds), remoteDir); err == nil {
		t.Fatal("RemoveUploadDir: want an error from the cancelled context")
	}
	for name, cmds := range map[string][]*exec.Cmd{
		"EnsureUploadDir": ensureCmds,
		"UploadFile":      uploadCmds,
		"RemoveUploadDir": removeCmds,
	} {
		if len(cmds) != 1 {
			t.Fatalf("%s built %d commands, want 1", name, len(cmds))
		}
		if cmds[0].WaitDelay != waitDelay {
			t.Errorf("%s WaitDelay = %v, want %v: a descendant of the killed ssh can hold "+
				"the captured pipes open and block Run indefinitely",
				name, cmds[0].WaitDelay, waitDelay)
		}
	}
}
