package setupscript

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	_, err := s.Execute(context.Background(), ExecuteOpts{
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
	if _, err := s.Execute(context.Background(), ExecuteOpts{
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
	if _, err := s.Execute(context.Background(), ExecuteOpts{
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
	if _, err := s.Execute(context.Background(), ExecuteOpts{
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
	if _, err := s.Execute(context.Background(), ExecuteOpts{
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
	if _, err := s.Execute(context.Background(), ExecuteOpts{
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
	_, err := s.Execute(context.Background(), ExecuteOpts{
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
	_, err := s.Execute(context.Background(), ExecuteOpts{
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
	_, err := s.Execute(context.Background(), ExecuteOpts{
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

	_, err := s.Execute(context.Background(), ExecuteOpts{
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
	_, err := s.Execute(context.Background(), ExecuteOpts{
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
	if _, err := s.Execute(context.Background(), ExecuteOpts{
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

// TestExecute_LogOutput_WritesTailToBothSinks pins the dual-sink contract: when
// a caller claims the live stream (the TUI create-session stream does), the
// daemon log sink must still receive the script's output. Before LogOutput
// existed, a streamed setup ran with no trace whatsoever in the daemon log.
// Note the asymmetry the name reflects: Output gets the live stream, LogOutput
// gets the bounded tail once the run ends (see TestExecute_LogOutput_BoundedByTail).
func TestExecute_LogOutput_WritesTailToBothSinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX echo behavior assumed")
	}
	wt := t.TempDir()

	var stream, logSink bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"echo", "both-sinks"}}
	if _, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &stream,
		LogOutput:    &logSink,
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stream.String(), "both-sinks") {
		t.Fatalf("Output missed the stream: %q", stream.String())
	}
	if !strings.Contains(logSink.String(), "both-sinks") {
		t.Fatalf("LogOutput missed the stream: %q", logSink.String())
	}
}

// TestExecute_LogOutput_ReceivesTailOnFailure covers the path that matters most
// for diagnosis: a setup script that fails must leave its output in the log
// sink too, not just in the returned error.
func TestExecute_LogOutput_ReceivesTailOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	var logSink bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", "echo boom-marker >&2; exit 2"}}
	if _, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &bytes.Buffer{},
		LogOutput:    &logSink,
		Timeout:      5 * time.Second,
	}); err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
	if !strings.Contains(logSink.String(), "boom-marker") {
		t.Fatalf("LogOutput should carry the failure tail, got %q", logSink.String())
	}
}

// TestExecute_NilLogOutput_LeavesOutputUnchanged guards the no-regression
// promise for the existing callers that never set LogOutput.
func TestExecute_NilLogOutput_LeavesOutputUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX echo behavior assumed")
	}
	wt := t.TempDir()

	var stream bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"echo", "single-sink"}}
	if _, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &stream,
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimSpace(stream.String()); got != "single-sink" {
		t.Fatalf("Output = %q, want %q", got, "single-sink")
	}
}

// TestExecute_NoSinks_FallsBackToStderr keeps the historical fallback honest:
// with neither sink supplied the output still lands on os.Stderr (the daemon
// log), which is how every non-stream caller gets its record today. os.Stderr
// is swapped for a pipe so the assertion doesn't depend on polluting the test
// binary's own stderr.
//
// os.Stderr is process-global: this test (and therefore this file) must never
// call t.Parallel(), or the swap would capture — and corrupt — unrelated tests.
// The script writes a few bytes, far under the pipe buffer, so the run cannot
// block on an undrained reader.
func TestExecute_NoSinks_FallsBackToStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX echo behavior assumed")
	}
	wt := t.TempDir()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	orig := os.Stderr
	os.Stderr = w
	defer func() {
		os.Stderr = orig
		_ = w.Close() // no-op after the explicit close below; covers early t.Fatal
	}()

	s := Spec{Type: TypeCommand, Argv: []string{"echo", "stderr-fallback"}}
	if _, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if !strings.Contains(string(got), "stderr-fallback") {
		t.Fatalf("os.Stderr fallback lost the output, got %q", got)
	}
}

// TestExecute_ReturnsMeasuredDuration proves the returned duration is a real
// measurement of cmd.Run rather than a zero value. The floor is deliberately
// below the sleep so scheduling jitter can't flake it, while still being far
// above the few-millisecond cost of a command that never slept.
func TestExecute_ReturnsMeasuredDuration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep binary assumed")
	}
	wt := t.TempDir()

	s := Spec{Type: TypeCommand, Argv: []string{"sleep", "0.05"}}
	got, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Timeout:      30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got < 40*time.Millisecond {
		t.Fatalf("duration = %v, want at least 40ms for a 50ms sleep", got)
	}
}

// TestExecute_ReturnsDurationOnFailure pins the error path too: a script that
// burned real time before failing must not report a zero duration, or the log
// would attribute none of the create's cost to it.
func TestExecute_ReturnsDurationOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", "sleep 0.05; exit 3"}}
	got, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &bytes.Buffer{},
		Timeout:      30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
	if got < 40*time.Millisecond {
		t.Fatalf("duration on failure = %v, want at least 40ms for a 50ms sleep", got)
	}
}

// TestExecute_LogOutput_BoundedByTail is the log-volume guard: a chatty setup
// script (a real `yarn install` emits thousands of lines) must not flood the
// daemon log. The live stream stays unbounded; only the log sink is capped.
func TestExecute_LogOutput_BoundedByTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	var stream, logSink bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{
		"sh", "-c", `i=0; while [ $i -lt 2000 ]; do echo "line-$i-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; i=$((i+1)); done`,
	}}
	if _, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &stream,
		LogOutput:    &logSink,
		Timeout:      30 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stream.Len() <= setupOutputTailBytes {
		t.Fatalf("test script emitted only %d bytes — not enough to exercise the bound", stream.Len())
	}
	if logSink.Len() > setupOutputTailBytes {
		t.Fatalf("LogOutput = %d bytes, want at most %d", logSink.Len(), setupOutputTailBytes)
	}
}

// errWriter fails every write, standing in for a log sink whose backing file
// has gone away.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("sink is gone") }

// TestExecute_LogOutput_WriteErrorDoesNotFailTheRun pins the deliberate
// swallow: LogOutput is diagnostics, so a broken sink must never turn a working
// setup script into a failed one.
func TestExecute_LogOutput_WriteErrorDoesNotFailTheRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX echo behavior assumed")
	}
	wt := t.TempDir()

	var stream bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"echo", "sink-error"}}
	got, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &stream,
		LogOutput:    errWriter{},
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute: %v — a failing log sink must not fail the run", err)
	}
	if got <= 0 {
		t.Fatalf("duration = %v, want a positive measurement", got)
	}
	if !strings.Contains(stream.String(), "sink-error") {
		t.Fatalf("Output lost the stream: %q", stream.String())
	}
}

// TestExecute_LogOutput_BlankOutputWritesNothing pins the blank-only guard: a
// script whose entire output is whitespace must not produce an empty log event.
func TestExecute_LogOutput_BlankOutputWritesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	var logSink bytes.Buffer
	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", `printf "  \n\n"`}}
	if _, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &bytes.Buffer{},
		LogOutput:    &logSink,
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if logSink.Len() != 0 {
		t.Fatalf("LogOutput = %q, want no write at all for whitespace-only output", logSink.String())
	}
}

// TestExecute_Timeout_ErrorWrapsDeadlineExceeded pins the timeout error's
// contract: callers must be able to errors.Is it, and the tail that names the
// step that hung must survive into the message.
func TestExecute_Timeout_ErrorWrapsDeadlineExceeded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	wt := t.TempDir()

	s := Spec{Type: TypeCommand, Argv: []string{"sh", "-c", "echo hang-marker; sleep 5"}}
	_, err := s.Execute(context.Background(), ExecuteOpts{
		WorktreePath: wt,
		Output:       &bytes.Buffer{},
		Timeout:      200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "hang-marker") {
		t.Fatalf("timeout error dropped the output tail: %v", err)
	}
}

// TestExecute_CallerDeadline_ErrorNamesCaller pins the zero-timeout boundary
// in the timeout diagnostic. When Execute adds no timeout of its own, the
// message must attribute cancellation to the caller rather than claiming a
// misleading zero-second configured limit.
func TestExecute_CallerDeadline_ErrorNamesCaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sleep behavior assumed")
	}
	wt := t.TempDir()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	s := Spec{Type: TypeCommand, Argv: []string{"sleep", "5"}}
	_, err := s.Execute(ctx, ExecuteOpts{
		WorktreePath: wt,
		Timeout:      0,
	})
	if err == nil {
		t.Fatal("expected caller deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "timed out after the caller's deadline") {
		t.Fatalf("error misidentified the timeout source: %v", err)
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

	_, err := s.Execute(context.Background(), ExecuteOpts{
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
