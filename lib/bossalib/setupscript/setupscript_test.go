package setupscript

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParse_JSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Spec
	}{
		{
			name:  "make",
			input: `{"type":"make","target":"setup"}`,
			want:  Spec{Type: TypeMake, Target: "setup"},
		},
		{
			name:  "script",
			input: `{"type":"script","path":".boss/setup.sh"}`,
			want:  Spec{Type: TypeScript, Path: ".boss/setup.sh"},
		},
		{
			name:  "command",
			input: `{"type":"command","argv":["pnpm","install"]}`,
			want:  Spec{Type: TypeCommand, Argv: []string{"pnpm", "install"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != tt.want.Type || got.Target != tt.want.Target ||
				got.Path != tt.want.Path || !strSliceEq(got.Argv, tt.want.Argv) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParse_BareString_IsLegacy(t *testing.T) {
	got, err := Parse("pnpm install && pnpm build")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeLegacy {
		t.Fatalf("got type %q, want %q", got.Type, TypeLegacy)
	}
	if got.LegacyScript != "pnpm install && pnpm build" {
		t.Fatalf("LegacyScript = %q", got.LegacyScript)
	}
}

func TestParse_Empty_Errors(t *testing.T) {
	if _, err := Parse(""); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
	if _, err := Parse("   "); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestParse_InvalidJSON_Errors(t *testing.T) {
	if _, err := Parse(`{"type":"make"`); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestValidate_RejectsPathTraversal(t *testing.T) {
	specs := []Spec{
		{Type: TypeScript, Path: "../../../etc/shadow"},
		{Type: TypeScript, Path: ".."},
		// From the plan's post-flight check — a traversal attempt
		// disguised behind a legitimate-looking prefix.
		{Type: TypeScript, Path: ".boss/../../../bin/evil"},
	}
	for _, s := range specs {
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("spec %+v: want ErrInvalidSpec, got %v", s, err)
		}
	}
}

func TestValidate_RejectsAbsolutePath(t *testing.T) {
	s := Spec{Type: TypeScript, Path: "/etc/shadow"}
	if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestValidate_RejectsEmptyArgv(t *testing.T) {
	s := Spec{Type: TypeCommand, Argv: nil}
	if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestValidate_RejectsMakeTargetMetacharacters(t *testing.T) {
	s := Spec{Type: TypeMake, Target: "setup; rm -rf /"}
	if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

// TestValidate_MakeTarget covers both sides of the empty-target check at
// setupscript.go:87. An empty/whitespace target must be rejected; a
// non-empty target must pass validation. This kills the CONDITIONALS_NEGATION
// mutant (== "" flipped to != "").
func TestValidate_MakeTarget(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "empty rejected", target: "", wantErr: true},
		{name: "whitespace rejected", target: "   ", wantErr: true},
		{name: "non-empty accepted", target: "setup", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Spec{Type: TypeMake, Target: tt.target}
			err := s.Validate()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("want ErrInvalidSpec, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil error for target %q, got %v", tt.target, err)
			}
		})
	}
}

func TestValidate_UnknownType_Errors(t *testing.T) {
	s := Spec{Type: "rogue"}
	if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestExecute_ScriptPathTraversal_FailsBeforeExec(t *testing.T) {
	wt := t.TempDir()

	// Even though Validate rejects ".." already, confirm Execute also
	// refuses to build the command for a traversal attempt — defense
	// in depth in case Validate is skipped somewhere.
	s := Spec{Type: TypeScript, Path: "../escaped.sh"}
	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Timeout:      5 * time.Second,
	})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestExecute_Command_PassesArgvLiterally(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX echo behavior assumed")
	}
	wt := t.TempDir()

	var buf bytes.Buffer
	// Shell metachars in argv stay literal: echo never sees them as
	// shell syntax, just as a second argument.
	s := Spec{Type: TypeCommand, Argv: []string{"echo", "; rm -rf /"}}
	if err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &buf,
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "; rm -rf /" {
		t.Fatalf("argv treated as shell — got %q, want %q", got, "; rm -rf /")
	}
}

func TestExecute_LoginShell_WrapsButPreservesEnvAndCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login shell assumed")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	wt := t.TempDir()
	// The command runs through the login shell (bash -l -c 'exec "$@"' …). It
	// writes the injected WORKTREE_DIR to a RELATIVE path, so a correct run
	// proves three things survive the wrap: execution itself, cmd.Dir (the file
	// lands in wt), and the REPO_DIR/WORKTREE_DIR env. Output goes to a file, not
	// stdout, so login-shell rc chatter can't corrupt the assertion.
	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", `printf %s "$WORKTREE_DIR" > marker`}}
	if err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		LoginShell:   bash,
		Timeout:      30 * time.Second,
	}); err != nil {
		t.Fatalf("Execute via login shell: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "marker"))
	if err != nil {
		t.Fatalf("marker not written — command did not run through the login shell: %v", err)
	}
	if string(got) != wt {
		t.Fatalf("WORKTREE_DIR through login-shell wrap = %q, want %q", got, wt)
	}
}

// TestExecute_LoginShell_InvokesSupportedWrapper proves Execute takes the
// supported-shell branch at setupscript.go:206. A direct command can still
// preserve argv, cwd, and environment, so it would not distinguish the
// CONDITIONALS_NEGATION mutant there. Only the configured shell creates this
// marker; direct execution cannot.
func TestExecute_LoginShell_InvokesSupportedWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login shell assumed")
	}
	wt := t.TempDir()
	// A small executable named "bash" is sufficient for loginshell's supported
	// shell check. It writes a marker only when Execute invokes it as the shell;
	// the command below runs directly under the mutant and never reaches this
	// executable.
	bash := filepath.Join(t.TempDir(), "bash")
	if err := os.WriteFile(bash, []byte("#!/bin/sh\nprintf wrapped > \"$WORKTREE_DIR/login-shell-marker\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	s := Spec{Type: TypeCommand, Argv: []string{"true"}}
	if err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		LoginShell:   bash,
		Timeout:      10 * time.Second,
	}); err != nil {
		t.Fatalf("Execute via login shell: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "login-shell-marker"))
	if err != nil || string(got) != "wrapped" {
		t.Fatalf("bashrc marker missing: got %q err %v", got, err)
	}
}

func TestExecute_LoginShell_UnsupportedFallsBackToDirectExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command assumed")
	}
	wt := t.TempDir()
	// An unsupported shell name must not be exec'd as a wrapper — Execute falls
	// through to running the argv directly, so setup still works.
	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", `printf ok > marker`}}
	if err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		LoginShell:   "/usr/bin/definitely-not-a-shell",
		Timeout:      10 * time.Second,
	}); err != nil {
		t.Fatalf("Execute with unsupported login shell: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "marker"))
	if err != nil || string(got) != "ok" {
		t.Fatalf("direct-exec fallback failed: got %q err %v", got, err)
	}
}

func TestExecute_Script_RunsFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang assumed")
	}
	wt := t.TempDir()
	scriptPath := filepath.Join(wt, "setup.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hello\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	s := Spec{Type: TypeScript, Path: "setup.sh"}
	if err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &buf,
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "hello" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestExecute_Command_NonZeroExit_WrapsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", "exit 7"}}
	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &bytes.Buffer{},
		Timeout:      5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
	// The returned error must identify the setup script as its source rather
	// than surfacing a bare "exit status 7" with no provenance.
	if !strings.Contains(err.Error(), "setup script") {
		t.Fatalf("error missing setup-script context: %v", err)
	}
	// Wrapping must preserve the underlying exec error so callers that inspect
	// the exit code via errors.As still can.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("want wrapped *exec.ExitError, got %v", err)
	}
}

// TestExecute_NonZeroExit_ErrorIncludesOutputTail confirms a failing setup
// script's actual output is folded into the returned error, so callers (and
// users) see *why* it failed instead of a bare "exit status N". The full
// stream must still reach opts.Output.
func TestExecute_NonZeroExit_ErrorIncludesOutputTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	var buf bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", "echo boom-marker >&2; exit 2"}}
	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &buf,
		Timeout:      5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "boom-marker") {
		t.Fatalf("error should include the script's output tail, got: %v", err)
	}
	// The live stream must still receive the output unchanged.
	if !strings.Contains(buf.String(), "boom-marker") {
		t.Fatalf("opts.Output should still receive the full stream, got: %q", buf.String())
	}
}

// TestTailWriter_RetainsOnlyLastBytes pins the ring-buffer behaviour so the
// error tail stays bounded even for chatty scripts.
func TestTailWriter_RetainsOnlyLastBytes(t *testing.T) {
	w := &tailWriter{max: 8}
	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	if got := w.String(); got != "89ABCDEF" {
		t.Fatalf("tail = %q, want %q", got, "89ABCDEF")
	}
}

func TestExecute_Make_RequiresMakefile(t *testing.T) {
	wt := t.TempDir()

	s := Spec{Type: TypeMake, Target: "setup"}
	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Timeout:      5 * time.Second,
	})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestExecute_Legacy_WritesSetupShAndRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang assumed")
	}
	wt := t.TempDir()

	var warnMsg string
	var buf bytes.Buffer
	s := Spec{Type: TypeLegacy, LegacyScript: "echo legacy-ran"}

	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &buf,
		Timeout:      5 * time.Second,
		Warn:         func(msg string) { warnMsg = msg },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "legacy-ran") {
		t.Fatalf("legacy script didn't run: %q", buf.String())
	}
	if !strings.Contains(warnMsg, "re-run 'boss repo settings'") {
		t.Fatalf("expected reconfiguration hint, got %q", warnMsg)
	}
	// Confirm the materialized file exists.
	if _, err := os.Stat(filepath.Join(wt, ".boss", "setup.sh")); err != nil {
		t.Fatalf("expected .boss/setup.sh to exist: %v", err)
	}
}

func TestExecute_Timeout_PreservesDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep binary assumed")
	}
	wt := t.TempDir()

	s := Spec{Type: TypeCommand, Argv: []string{"sleep", "5"}}
	start := time.Now()
	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Timeout:      200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout not enforced: elapsed %v", elapsed)
	}
}

// TestExecute_ZeroTimeout_NoDeadline pins the boundary at setupscript.go:153
// (opts.Timeout > 0). With a zero timeout no deadline is installed, so a
// fast command must succeed. The CONDITIONALS_BOUNDARY mutant (> 0 → >= 0)
// would call context.WithTimeout(ctx, 0), yielding an already-expired
// context and a DeadlineExceeded failure — making this test fail.
func TestExecute_ZeroTimeout_NoDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX echo behavior assumed")
	}
	wt := t.TempDir()

	var buf bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"echo", "ran"}}
	if err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &buf,
		Timeout:      0, // exact boundary: no additional deadline
	}); err != nil {
		t.Fatalf("Execute with zero timeout should succeed, got %v", err)
	}
	if strings.TrimSpace(buf.String()) != "ran" {
		t.Fatalf("command did not run cleanly: %q", buf.String())
	}
}

func TestResolveInsideWorktree_RejectsTraversal(t *testing.T) {
	wt := t.TempDir()
	if _, err := resolveInsideWorktree(wt, "../escape.sh"); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestResolveInsideWorktree_AllowsSubdirs(t *testing.T) {
	wt := t.TempDir()
	got, err := resolveInsideWorktree(wt, ".boss/setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wt, ".boss", "setup.sh")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExecute_Legacy_ContentShebangRunsUnderSh pins the G306 invocation
// contract for a legacy string that carries its OWN shebang: because the
// materialized script is run via `sh <path>` (not direct-exec), the embedded
// shebang is treated as a comment and the body runs under /bin/sh — matching
// the documented historical `sh -c` semantics. The shebang here names a
// non-existent interpreter, so a direct-exec that honored it would fail;
// running successfully proves the shebang is ignored and sh interprets the body.
func TestExecute_Legacy_ContentShebangRunsUnderSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang assumed")
	}
	wt := t.TempDir()

	var buf bytes.Buffer
	s := Spec{Type: TypeLegacy, LegacyScript: "#!/nonexistent/interpreter\necho shebang-ignored"}

	err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &buf,
		Timeout:      5 * time.Second,
		Warn:         func(string) {},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "shebang-ignored") {
		t.Fatalf("legacy script with foreign shebang didn't run under sh: %q", buf.String())
	}
	// The stored shebang is preserved verbatim (writeLegacyScript does not
	// prepend #!/bin/sh when content already begins with #!), confirming it is
	// the `sh <path>` invocation — not a rewrite — that neutralizes it.
	body, err := os.ReadFile(filepath.Join(wt, ".boss", "setup.sh"))
	if err != nil {
		t.Fatalf("read setup.sh: %v", err)
	}
	if !strings.HasPrefix(string(body), "#!/nonexistent/interpreter\n") {
		t.Fatalf("expected content shebang preserved, got %q", string(body))
	}
}
