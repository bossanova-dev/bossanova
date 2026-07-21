package agentruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
)

const fakeBin = `#!/usr/bin/env bash
echo "hello from $1"
echo "second line"
exit 0
`

const runnerExitTimeout = 10 * time.Second

func writeFakeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agent.sh")
	if err := os.WriteFile(bin, []byte(fakeBin), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// fakeCmd returns a CommandFactory that runs /bin/sh -c "$script" instead
// of the real agent binary. Used to test subprocess plumbing without
// requiring a real agent CLI to be installed.
func fakeCmd(t *testing.T, script string) agentruntime.CommandFactory {
	t.Helper()
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

// argvForFakeBin builds an argv pointing at a real fake-agent.sh binary.
func argvForFakeBin(bin string) func(agentruntime.BuildArgvInput) []string {
	return func(in agentruntime.BuildArgvInput) []string {
		return []string{bin, in.SessionID}
	}
}

// argvNoop returns a stub argv builder; the CommandFactory replaces the
// real exec call so the argv contents are irrelevant in fakeCmd-based tests.
func argvNoop() func(agentruntime.BuildArgvInput) []string {
	return func(in agentruntime.BuildArgvInput) []string {
		return []string{"agent"}
	}
}

func TestRunnerStartCapturesOutputToLog(t *testing.T) {
	bin := writeFakeBin(t)
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "run.log")

	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BuildArgv:  argvForFakeBin(bin),
		BinaryName: "fake",
	})

	sid, err := r.Start(context.Background(), t.TempDir(), "the plan", nil, "sess-1", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "sess-1" {
		t.Fatalf("session id = %q, want sess-1", sid)
	}

	deadline := time.After(runnerExitTimeout)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("runner never exited")
		case <-time.After(20 * time.Millisecond):
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if want := "hello from sess-1"; !strings.Contains(string(data), want) {
		t.Errorf("log missing first line; got %s", data)
	}
}

// TestRunnerStartRedactsPromptFromArgvLog proves the spawn preamble never
// persists the run prompt. An agent whose BuildArgv puts in.Plan on argv (as
// opencode must, having no stdin prompt path) has that element replaced with a
// length-only placeholder in the per-session log, while the non-prompt flags
// stay visible for debugging.
func TestRunnerStartRedactsPromptFromArgvLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	const secret = "SECRET_PROMPT_SENTINEL do not persist this task text"

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv: func(in agentruntime.BuildArgvInput) []string {
				return []string{"opencode", "run", "--auto", in.Plan}
			},
			BinaryName: "opencode",
		},
		agentruntime.WithCommandFactory(fakeCmd(t, "true")),
	)

	sid, err := r.Start(context.Background(), dir, secret, nil, "sess-redact", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.After(runnerExitTimeout)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("runner never exited")
		case <-time.After(20 * time.Millisecond):
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	body := string(data)
	if strings.Contains(body, secret) {
		t.Errorf("run prompt leaked into the per-session log: %s", body)
	}
	// The NDJSON writer HTML-escapes the angle brackets (`<` → `<`), so match
	// the escape-surviving core of the placeholder rather than the literal `<`.
	if !strings.Contains(body, "prompt redacted:") {
		t.Errorf("expected redaction placeholder in spawn preamble; log=%s", body)
	}
	// Non-prompt argv (the flags) must remain visible for debugging.
	if !strings.Contains(body, "--auto") {
		t.Errorf("expected non-prompt argv (--auto) to remain in the log; log=%s", body)
	}
}

// runnerLogTexts runs a fake command that echoes environment markers, waits
// for exit, and returns the parsed NDJSON "text" lines from the run log.
func runnerLogTexts(t *testing.T, r *agentruntime.Runner, dir, logPath, sid string, extraEnv map[string]string) []string {
	t.Helper()
	if _, err := r.Start(context.Background(), dir, "", nil, sid, logPath, "", extraEnv); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(runnerExitTimeout)
	for time.Now().Before(deadline) && r.IsRunning(sid) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.IsRunning(sid) {
		t.Fatalf("runner still alive after %s", runnerExitTimeout)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var texts []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var entry struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(line), &entry)
		texts = append(texts, entry.Text)
	}
	return texts
}

func TestRunnerStart_AppliesExtraEnvAndInherits(t *testing.T) {
	t.Setenv("PROOF_TEST_INHERITED", "inherited-value")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	const secret = "SECRET-proof-token-value"
	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{BuildArgv: argvNoop(), BinaryName: "fake"},
		agentruntime.WithCommandFactory(fakeCmd(t,
			`printf 'EXTRA=%s INHERITED=%s\n' "$PROOF_TEST_KEY" "$PROOF_TEST_INHERITED"`)),
	)

	texts := runnerLogTexts(t, r, dir, logPath, "env-on", map[string]string{"PROOF_TEST_KEY": secret})
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "EXTRA="+secret) {
		t.Errorf("extraEnv value not applied to child; log=%s", joined)
	}
	if !strings.Contains(joined, "INHERITED=inherited-value") {
		t.Errorf("child did not inherit os.Environ(); log=%s", joined)
	}
	// The spawn preamble logs argv/cwd/PATH only — the secret value must not
	// appear on any [runner] diagnostic line.
	for _, txt := range texts {
		if strings.HasPrefix(txt, "[runner] ") && strings.Contains(txt, secret) {
			t.Errorf("secret value leaked into runner preamble/diagnostic: %q", txt)
		}
	}
}

// TestRunnerStart_ExtraEnvOverridesInheritedKey pins the security-relevant
// precedence: when the overlay and the inherited environment share a key, the
// overlay must win. This is exactly the threat-model requirement — a
// proof-scoped CLOUDFLARE_API_TOKEN in extraEnv must override an account-wide
// one already exported into the daemon environment. The guarantee rests on
// Go's os/exec dedupEnv keeping the last duplicate (extraEnv is appended after
// os.Environ()); this test locks it against a future refactor/runtime change.
func TestRunnerStart_ExtraEnvOverridesInheritedKey(t *testing.T) {
	t.Setenv("PROOF_TEST_KEY", "inherited-loses")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{BuildArgv: argvNoop(), BinaryName: "fake"},
		agentruntime.WithCommandFactory(fakeCmd(t, `printf 'KEY=%s\n' "$PROOF_TEST_KEY"`)),
	)

	texts := runnerLogTexts(t, r, dir, logPath, "env-collision",
		map[string]string{"PROOF_TEST_KEY": "overlay-wins"})
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "KEY=overlay-wins") {
		t.Errorf("overlay must override the conflicting inherited key; log=%s", joined)
	}
	if strings.Contains(joined, "KEY=inherited-loses") {
		t.Errorf("inherited value must not win over the overlay; log=%s", joined)
	}
}

func TestRunnerStart_NoExtraEnvStillInherits(t *testing.T) {
	t.Setenv("PROOF_TEST_INHERITED", "still-here")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{BuildArgv: argvNoop(), BinaryName: "fake"},
		agentruntime.WithCommandFactory(fakeCmd(t, `printf 'INHERITED=%s\n' "$PROOF_TEST_INHERITED"`)),
	)

	// nil extraEnv leaves cmd.Env nil → child inherits the full environment.
	texts := runnerLogTexts(t, r, dir, logPath, "env-off", nil)
	if !strings.Contains(strings.Join(texts, "\n"), "INHERITED=still-here") {
		t.Errorf("child did not inherit environment with nil extraEnv; log=%v", texts)
	}
}

func TestRunnerStart_WritesNDJSON(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv:  argvNoop(),
			BinaryName: "fake",
		},
		agentruntime.WithCommandFactory(fakeCmd(t, "echo line-one; echo line-two")),
	)
	sid, err := r.Start(context.Background(), dir, "ignored-plan", nil, "test-session", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "test-session" {
		t.Errorf("session ID = %q, want test-session", sid)
	}

	// Wait for the fake to exit and lineWriter to flush.
	deadline := time.Now().Add(runnerExitTimeout)
	for time.Now().Before(deadline) {
		if !r.IsRunning(sid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.IsRunning(sid) {
		t.Fatalf("runner still alive after %s", runnerExitTimeout)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Every line must be valid NDJSON with non-empty Text and parseable TS.
	for i, line := range lines {
		var entry struct {
			TS   string `json:"ts"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("line %d not valid NDJSON: %q (%v)", i, line, err)
		}
		if entry.Text == "" {
			t.Errorf("line %d Text empty", i)
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.TS); err != nil {
			t.Errorf("line %d TS %q not RFC3339Nano (%v)", i, entry.TS, err)
		}
	}
	// The runner now wraps subprocess output with [runner] preamble + exit
	// markers so empty log files become impossible. Filter those out and
	// assert the subprocess output appears intact in order.
	var subprocessTexts []string
	var sawSpawn, sawExit bool
	for _, line := range lines {
		var entry struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(line), &entry)
		switch {
		case strings.HasPrefix(entry.Text, "[runner] spawning"):
			sawSpawn = true
		case strings.HasPrefix(entry.Text, "[runner] exited"):
			sawExit = true
		case strings.HasPrefix(entry.Text, "[runner] "):
			// Other runner diagnostics are not subprocess output. The echo
			// fake never reads stdin, so a "[runner] writing plan to stdin
			// failed" line races in nondeterministically when the process
			// exits before the plan write completes; filter it out too.
		default:
			subprocessTexts = append(subprocessTexts, entry.Text)
		}
	}
	if !sawSpawn {
		t.Errorf("expected [runner] spawning preamble line; lines=%v", lines)
	}
	if !sawExit {
		t.Errorf("expected [runner] exited trailer line; lines=%v", lines)
	}
	want := []string{"line-one", "line-two"}
	if len(subprocessTexts) != len(want) {
		t.Fatalf("expected %d subprocess lines, got %d: %v", len(want), len(subprocessTexts), subprocessTexts)
	}
	for i, got := range subprocessTexts {
		if got != want[i] {
			t.Errorf("subprocess line %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestRunnerStart_LogsCmdStartFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv:  argvNoop(),
			BinaryName: "fake",
		},
		agentruntime.WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// /no/such/binary so cmd.Start fails with an OS error before
			// the subprocess produces any output. Without diagnostic
			// capture this would leave a 0-byte log; the new code writes
			// the failure reason into the log file before closing it.
			return exec.CommandContext(ctx, "/no/such/binary")
		}),
	)
	_, err := r.Start(context.Background(), dir, "", nil, "fail-session", logPath, "", nil)
	if err == nil {
		t.Fatal("expected error from Start when cmd.Start fails")
	}

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if len(data) == 0 {
		t.Fatal("log file is empty; cmd.Start failure must be recorded in the log")
	}
	body := string(data)
	if !strings.Contains(body, "[runner] spawning") {
		t.Errorf("log missing spawning preamble: %s", body)
	}
	if !strings.Contains(body, "[runner] cmd.Start failed") {
		t.Errorf("log missing cmd.Start failure marker: %s", body)
	}
}

func TestRunnerStart_LogsStdinWriteFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	// A plan far larger than the OS pipe buffer. The fake agent exits
	// immediately without consuming stdin, so writing the plan fails once the
	// read end closes. The runner must record that failure rather than letting
	// the agent silently run without its instructions.
	plan := strings.Repeat("x", 1<<20) // 1 MiB

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv:  argvNoop(),
			BinaryName: "fake",
		},
		agentruntime.WithCommandFactory(fakeCmd(t, "exit 0")),
	)
	sid, err := r.Start(context.Background(), dir, plan, nil, "stdin-session", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(runnerExitTimeout)
	for time.Now().Before(deadline) {
		if !r.IsRunning(sid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.IsRunning(sid) {
		t.Fatalf("runner still alive after %s", runnerExitTimeout)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if want := "[runner] writing plan to stdin failed"; !strings.Contains(string(data), want) {
		t.Errorf("log missing stdin-write-failure marker; got:\n%s", data)
	}
}

func TestRunnerStart_LogsNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv:  argvNoop(),
			BinaryName: "fake",
		},
		agentruntime.WithCommandFactory(fakeCmd(t, "exit 7")),
	)
	sid, err := r.Start(context.Background(), dir, "", nil, "exit-session", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !r.IsRunning(sid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.IsRunning(sid) {
		t.Fatal("runner still alive after 2s")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "[runner] exited:") {
		t.Errorf("log missing exit marker for non-zero exit: %s", string(data))
	}
}

func TestRunnerStart_RefusesSymlinkLogPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	link := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv:  argvNoop(),
			BinaryName: "fake",
		},
		agentruntime.WithCommandFactory(fakeCmd(t, "true")),
	)
	_, err := r.Start(context.Background(), dir, "", nil, "sid", link, "", nil)
	if !errors.Is(err, agentruntime.ErrLogPathSymlink) {
		t.Errorf("Start with symlink: err = %v, want ErrLogPathSymlink", err)
	}
}

func TestRunnerStart_RejectsEmptyArgv(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BuildArgv: func(agentruntime.BuildArgvInput) []string { return nil },
	})
	_, err := r.Start(context.Background(), dir, "", nil, "sid", logPath, "", nil)
	if err == nil || !strings.Contains(err.Error(), "empty argv") {
		t.Errorf("Start with empty argv: err = %v, want empty argv error", err)
	}
}

// fakeBinPostExit emits an auth-failure marker on stderr then exits 1.
// Used by TestRunnerPostExitReplacesError to drive the PostExit hook.
const fakeBinPostExit = `#!/usr/bin/env bash
echo "ERROR: 401 Unauthorized: Missing bearer or basic authentication" >&2
exit 1
`

// errAuthRequired is a sentinel returned by the test PostExit hook so
// the assertion can verify the error was replaced (not just non-nil).
var errAuthRequired = errors.New("test: auth required")

// TestRunnerPostExitReplacesError verifies the PostExit hook can upgrade
// a generic non-zero exit into a typed error. The fake binary exits 1
// with an auth-failure marker on stderr; the hook recognizes the marker
// in the log tail and returns errAuthRequired; the runner reports that
// via ExitError instead of the original "exit status 1".
func TestRunnerPostExitReplacesError(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-auth.sh")
	if err := os.WriteFile(binPath, []byte(fakeBinPostExit), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "fake",
		BuildArgv: func(in agentruntime.BuildArgvInput) []string {
			return []string{binPath}
		},
		PostExit: func(orig error, tail []byte) error {
			if orig == nil {
				return nil
			}
			if strings.Contains(string(tail), "401 Unauthorized") {
				return errAuthRequired
			}
			return nil
		},
	})

	sid, err := r.Start(context.Background(), dir, "", nil, "sess-auth", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := r.Wait(waitCtx, sid)
	if !errors.Is(got, errAuthRequired) {
		t.Errorf("ExitError = %v, want errAuthRequired", got)
	}
}

// fakeBinThreadStarted emits a JSONL `thread.started` event with a
// known UUID, then exits cleanly. Used by TestRunnerSessionIDFromOutput
// to drive the SessionIDFromOutput hook.
const fakeBinThreadStarted = `#!/usr/bin/env bash
echo '{"type":"thread.started","thread_id":"abcd-1234"}'
echo '{"type":"event_msg","payload":{"type":"task_complete"}}'
exit 0
`

const fakeBinThreadStartedThenSleeps = `#!/usr/bin/env bash
echo '{"type":"thread.started","thread_id":"fast-1234"}'
sleep 10
exit 0
`

const fakeBinThreadStartedAfterSlowStartup = `#!/usr/bin/env bash
sleep 3
echo '{\"type\":\"thread.started\",\"thread_id\":\"slow-1234\"}'
sleep 1
exit 0
`

func threadIDFromOutput(buf []byte) string {
	output := string(buf)
	for _, marker := range []string{`"thread_id":"`, `\"thread_id\":\"`} {
		idx := strings.Index(output, marker)
		if idx < 0 {
			continue
		}
		rest := output[idx+len(marker):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ""
		}
		return strings.TrimSuffix(rest[:end], `\`)
	}
	return ""
}

// TestRunnerSessionIDFromOutput verifies the SessionIDFromOutput hook
// re-keys the runner's session ID to whatever the hook discovers in the
// early stdout — the caller's "ignored-hint" value must be replaced by
// "abcd-1234" parsed out of the fake binary's `thread.started` line.
func TestRunnerSessionIDFromOutput(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-thread.sh")
	if err := os.WriteFile(binPath, []byte(fakeBinThreadStarted), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "fake",
		BuildArgv: func(in agentruntime.BuildArgvInput) []string {
			return []string{binPath}
		},
		SessionIDFromOutput: threadIDFromOutput,
	})

	sid, err := r.Start(context.Background(), dir, "", nil, "ignored-hint", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "abcd-1234" {
		t.Errorf("Start returned sid=%q, want abcd-1234 (caller hint should be replaced by SessionIDFromOutput)", sid)
	}
	// Sanity: the runner should track the process under the discovered
	// ID. IsRunning("ignored-hint") must be false; IsRunning(sid) is
	// either still true or false (depending on scheduling), but it must
	// not panic and must be consistent with ExitError(sid).
	if r.IsRunning("ignored-hint") {
		t.Error("IsRunning still finds process under caller-supplied hint after SessionIDFromOutput re-keyed it")
	}
	deadline := time.After(2 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for fake-thread subprocess to exit")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestRunnerSessionIDFromOutputReturnsWhenIDArrives(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-thread-sleep.sh")
	if err := os.WriteFile(binPath, []byte(fakeBinThreadStartedThenSleeps), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "fake",
		BuildArgv: func(in agentruntime.BuildArgvInput) []string {
			return []string{binPath}
		},
		SessionIDFromOutput: threadIDFromOutput,
	})

	started := time.Now()
	sid, err := r.Start(context.Background(), dir, "", nil, "ignored-hint", logPath, "", nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "fast-1234" {
		t.Fatalf("Start returned sid=%q, want fast-1234", sid)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("Start took %s after session ID output; want under 6s", elapsed)
	}
	if !r.IsRunning(sid) {
		t.Fatal("Start returned after fake-thread subprocess exited; want return while process is still running")
	}

	if err := r.Stop(sid); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestRunnerSessionIDFromOutputToleratesSlowStartup(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-thread-slow-startup.sh")
	if err := os.WriteFile(binPath, []byte(fakeBinThreadStartedAfterSlowStartup), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "agent.log")

	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "fake",
		BuildArgv: func(in agentruntime.BuildArgvInput) []string {
			return []string{binPath}
		},
		SessionIDFromOutput: threadIDFromOutput,
	})

	started := time.Now()
	sid, err := r.Start(context.Background(), dir, "", nil, "ignored-hint", logPath, "", nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "slow-1234" {
		_ = r.Stop(sid)
		t.Fatalf("Start returned sid=%q after %s, want slow-1234", sid, elapsed)
	}

	if err := r.Stop(sid); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestRunnerGeneratesUUIDWhenNoSessionIDProvided is a regression guard for a
// headless-dispatch bug (boss-epic / cron sessions). When the caller passed no
// session ID, the runner minted "<bin>-<nanos>" (e.g.
// "claude-1783144936819808000") and marked it NOT provided, so the argv builder
// never injected it. claude then generated its own rollout UUID, the runner
// returned the non-UUID timestamp ID, and bossd persisted an ID that
// resume/attach and on-disk transcript lookup could never resolve — stranding
// the session behind a misleading "CLI could not be launched" error and reaping
// the chat as transcript-less. A runner-generated ID must be a real UUID, marked
// provided so id-injecting agents (claude) adopt it as their rollout ID.
func TestRunnerGeneratesUUIDWhenNoSessionIDProvided(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "run.log")

	var captured agentruntime.BuildArgvInput
	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "claude",
		BuildArgv: func(in agentruntime.BuildArgvInput) []string {
			captured = in
			return []string{"agent"}
		},
	}, agentruntime.WithCommandFactory(fakeCmd(t, "exit 0")))

	sid, err := r.Start(context.Background(), t.TempDir(), "", nil, "", logPath, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, perr := uuid.Parse(sid); perr != nil {
		t.Errorf("Start returned non-UUID session id %q (want a valid UUID, not \"<bin>-<nanos>\"): %v", sid, perr)
	}
	if strings.HasPrefix(sid, "claude-") {
		t.Errorf("Start returned timestamp-style session id %q; want a UUID", sid)
	}
	if captured.SessionID != sid {
		t.Errorf("BuildArgv saw SessionID=%q, want the returned id %q", captured.SessionID, sid)
	}
	if !captured.ProvidedSessionID {
		t.Error("BuildArgv saw ProvidedSessionID=false for a runner-generated id; " +
			"id-injecting agents (claude) then never receive --session-id and diverge from their rollout")
	}
}

// TestRunnerResumeWithEmptySessionIDDoesNotInjectID guards the boundary of the
// UUID-minting fix: on the resurrect path bossd calls Start with a resume
// target but an empty tracking ID. The runner must NOT mark that generated key
// as provided — otherwise the argv builder would emit both --resume and
// --session-id, which claude cannot accept together. Only --resume applies.
func TestRunnerResumeWithEmptySessionIDDoesNotInjectID(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "run.log")

	var captured agentruntime.BuildArgvInput
	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "claude",
		BuildArgv: func(in agentruntime.BuildArgvInput) []string {
			captured = in
			return []string{"agent"}
		},
	}, agentruntime.WithCommandFactory(fakeCmd(t, "exit 0")))

	resumeTarget := "7a0cca71-590a-46c0-957f-095461d7ec76"
	if _, err := r.Start(context.Background(), t.TempDir(), "", &resumeTarget, "", logPath, "", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if captured.ProvidedSessionID {
		t.Error("BuildArgv saw ProvidedSessionID=true while resuming; claude would then get both " +
			"--resume and --session-id, which it cannot accept together")
	}
	if captured.Resume == nil || *captured.Resume != resumeTarget {
		t.Errorf("BuildArgv saw Resume=%v, want the resume target %q", captured.Resume, resumeTarget)
	}
}

// TestRunnerStart_ConcurrentSameSessionIDIsSerialized is a regression
// guard for a TOCTOU bug fixed in the Start path. The previous shape
// acquired the runner mutex around the duplicate-session check, released
// it, ran ~75 lines of setup, then re-acquired the mutex to insert the
// process. Two concurrent Start calls with the same sessionID could both
// pass the check and both reach the insert; the second insertion would
// silently overwrite the first, orphaning that process — its cancel
// func was unreachable so Stop() could never reach it.
//
// The fix holds the runner mutex from the existence check through the
// insertion. This test asserts the new contract: when N goroutines race
// on Start with the same sessionID, exactly one succeeds and the rest
// receive "session ... already exists". After the test the runner must
// have at most one process under that sessionID and Stop must cleanly
// reach it.
func TestRunnerStart_ConcurrentSameSessionIDIsSerialized(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()

	// Sleeping fake widens the would-be TOCTOU window so a regression
	// would actually manifest under -race instead of silently passing.
	r := agentruntime.NewRunner(zerolog.Nop(), agentruntime.Options{
		BinaryName: "fake",
		BuildArgv:  argvNoop(),
		BufSize:    16,
	}, agentruntime.WithCommandFactory(fakeCmd(t, "sleep 1")))

	const goroutines = 16
	const sessionID = "race-target"

	var (
		wg          sync.WaitGroup
		successes   atomic.Int32
		alreadyErrs atomic.Int32
		otherErrs   atomic.Int32
		start       = make(chan struct{})
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start // align all goroutines on Start.
			logPath := filepath.Join(logDir, "run-")
			// Per-goroutine log path so the runner can't fail the second
			// caller for a non-race reason (file-already-open, etc).
			_, err := r.Start(context.Background(), dir, "plan", nil, sessionID,
				logPath+formatInt(i)+".log", "", nil)
			switch {
			case err == nil:
				successes.Add(1)
			case strings.Contains(err.Error(), "already exists"):
				alreadyErrs.Add(1)
			default:
				t.Errorf("unexpected error from Start: %v", err)
				otherErrs.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("successful Starts = %d, want exactly 1", got)
	}
	if got := alreadyErrs.Load(); got != goroutines-1 {
		t.Errorf(`"already exists" errors = %d, want %d`, got, goroutines-1)
	}
	if got := otherErrs.Load(); got != 0 {
		t.Errorf("unexpected non-conflict errors = %d, want 0", got)
	}

	// The single winner must still be tracked and Stop must reach it.
	if !r.IsRunning(sessionID) {
		t.Error("IsRunning(sessionID) = false after winning Start; the surviving process should be tracked")
	}
	if err := r.Stop(sessionID); err != nil {
		t.Errorf("Stop after winning Start: %v (a tracked process must be Stop-reachable)", err)
	}
}

// formatInt is a tiny strconv-free int-to-string helper so this test
// file doesn't reach for strconv just to label log files.
func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
