package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// hookEnv is the minimal ExtraEnv overlay that arms injection.
func hookEnv() map[string]string {
	return map[string]string{
		hookEnvPort:  "45678",
		hookEnvToken: "tok-secret",
	}
}

// TestInstallQuestionHookEnvGating is the table-driven contract for when the
// opencode event-hook asset is written. BOTH the loopback port and the bearer
// token must be present: with either missing the injected JS could not reach or
// authenticate to bossd's receiver, so writing it would only litter the
// worktree — and opencode must still run normally.
func TestInstallQuestionHookEnvGating(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		emptyWorkID bool
		wantWritten bool
	}{
		{name: "both env vars present", env: hookEnv(), wantWritten: true},
		{name: "nil env", env: nil},
		{name: "empty env", env: map[string]string{}},
		{name: "token missing", env: map[string]string{hookEnvPort: "45678"}},
		{name: "port missing", env: map[string]string{hookEnvToken: "tok-secret"}},
		{name: "token empty string", env: map[string]string{hookEnvPort: "45678", hookEnvToken: ""}},
		{name: "port empty string", env: map[string]string{hookEnvPort: "", hookEnvToken: "tok-secret"}},
		{
			name:        "unrelated env only",
			env:         map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "nope"},
			wantWritten: false,
		},
		{name: "empty work dir is a no-op even with env", env: hookEnv(), emptyWorkID: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			workDir := dir
			if tt.emptyWorkID {
				workDir = ""
			}

			installed, err := installQuestionHook(workDir, tt.env)
			if err != nil {
				t.Fatalf("installQuestionHook: %v", err)
			}
			if installed != tt.wantWritten {
				t.Errorf("installed = %v, want %v", installed, tt.wantWritten)
			}

			got, readErr := os.ReadFile(questionHookPath(dir))
			switch {
			case tt.wantWritten:
				if readErr != nil {
					t.Fatalf("asset not written: %v", readErr)
				}
				if !bytes.Equal(got, questionHookJS) {
					t.Errorf("written asset differs from the embedded bytes (%d vs %d)", len(got), len(questionHookJS))
				}
			case readErr == nil:
				t.Errorf("asset written at %s but should not have been", questionHookPath(dir))
			default:
				// Nothing written and nothing created: the whole .opencode tree
				// must be absent, not just the file.
				if _, err := os.Stat(filepath.Join(dir, questionHookRootDir)); err == nil {
					t.Errorf("%s created despite no injection", questionHookRootDir)
				}
			}
		})
	}
}

// TestInstallQuestionHookWritesPluralPluginsDir pins the exact injected path.
// opencode auto-loads `.opencode/plugins/` (PLURAL); the singular `.opencode/`
// the original plan sketched is NOT an auto-load directory, so a path typo here
// silently disables the whole feature.
func TestInstallQuestionHookWritesPluralPluginsDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := installQuestionHook(dir, hookEnv()); err != nil {
		t.Fatalf("installQuestionHook: %v", err)
	}
	want := filepath.Join(dir, ".opencode", "plugins", "bossd-question.js")
	if got := questionHookPath(dir); got != want {
		t.Errorf("questionHookPath = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected asset at %s: %v", want, err)
	}
}

// TestInstallQuestionHookIsIdempotent proves a re-run (retry, resume, or a
// crashed predecessor's leftovers) converges on the embedded bytes rather than
// appending or erroring. The asset is static, so overwriting is always correct.
func TestInstallQuestionHookIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := questionHookPath(dir)

	if _, err := installQuestionHook(dir, hookEnv()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// Simulate a stale/corrupt copy from an earlier run.
	if err := os.WriteFile(path, []byte("// stale garbage\n"), 0o600); err != nil {
		t.Fatalf("seed stale asset: %v", err)
	}
	if _, err := installQuestionHook(dir, hookEnv()); err != nil {
		t.Fatalf("second install: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if !bytes.Equal(got, questionHookJS) {
		t.Error("re-install did not restore the embedded bytes")
	}
}

// TestRemoveQuestionHook covers the cleanup contract: it deletes the asset,
// prunes only the directories it owns, is idempotent, and never touches
// pre-existing user content under .opencode/.
func TestRemoveQuestionHook(t *testing.T) {
	tests := []struct {
		name string
		// seed prepares the worktree; it returns paths that MUST survive cleanup.
		seed func(t *testing.T, dir string) []string
		// wantRootGone asserts the whole .opencode tree was pruned.
		wantRootGone bool
	}{
		{
			name: "removes the asset and prunes the dirs it created",
			seed: func(t *testing.T, dir string) []string {
				t.Helper()
				if _, err := installQuestionHook(dir, hookEnv()); err != nil {
					t.Fatalf("install: %v", err)
				}
				return nil
			},
			wantRootGone: true,
		},
		{
			name:         "no-op on a worktree that never had the hook",
			seed:         func(t *testing.T, _ string) []string { t.Helper(); return nil },
			wantRootGone: true,
		},
		{
			name: "keeps a pre-existing user .opencode file",
			seed: func(t *testing.T, dir string) []string {
				t.Helper()
				if _, err := installQuestionHook(dir, hookEnv()); err != nil {
					t.Fatalf("install: %v", err)
				}
				cfg := filepath.Join(dir, ".opencode", "opencode.json")
				if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("seed user config: %v", err)
				}
				return []string{cfg}
			},
		},
		{
			name: "keeps a sibling user plugin in the same plugins dir",
			seed: func(t *testing.T, dir string) []string {
				t.Helper()
				if _, err := installQuestionHook(dir, hookEnv()); err != nil {
					t.Fatalf("install: %v", err)
				}
				mine := filepath.Join(dir, ".opencode", "plugins", "mine.js")
				if err := os.WriteFile(mine, []byte("// user plugin\n"), 0o600); err != nil {
					t.Fatalf("seed user plugin: %v", err)
				}
				return []string{mine}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			survivors := tt.seed(t, dir)

			// Called TWICE: cleanup runs on several exit paths (run end,
			// RemoveAgentRunHook, failed start) and must be a silent no-op after
			// the first success.
			for i := range 2 {
				if err := removeQuestionHook(dir); err != nil {
					t.Fatalf("removeQuestionHook call %d: %v", i+1, err)
				}
			}

			if _, err := os.Stat(questionHookPath(dir)); err == nil {
				t.Error("asset still present after cleanup")
			}
			for _, path := range survivors {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("cleanup destroyed pre-existing user file %s: %v", path, err)
				}
			}
			if tt.wantRootGone {
				if _, err := os.Stat(filepath.Join(dir, questionHookRootDir)); err == nil {
					t.Errorf("%s survived cleanup but held only bossd's asset", questionHookRootDir)
				}
			}
		})
	}
}

// TestRemoveQuestionHookEmptyWorkDir guards the degenerate call the failed-start
// path can make.
func TestRemoveQuestionHookEmptyWorkDir(t *testing.T) {
	if err := removeQuestionHook(""); err != nil {
		t.Errorf("removeQuestionHook(\"\") = %v, want nil", err)
	}
}

// TestEmbeddedQuestionHookAsset asserts the shipped JS actually implements the
// contract the Go side advertises: it dispatches on the event names we handle,
// posts the notification_type values bossd's receiver keys on, and — critically
// — bakes in NO port or token. The secrets are read from process.env at runtime,
// which is what keeps them out of the session worktree; a templated placeholder
// here would mean someone reintroduced per-run rendering.
func TestEmbeddedQuestionHookAsset(t *testing.T) {
	js := string(questionHookJS)
	if len(js) == 0 {
		t.Fatal("embedded question hook asset is empty")
	}

	mustContain := []string{
		// Plugin shape + auto-load export.
		"export const BossdQuestion",
		"event: async ({ event })",
		// Handled event kinds.
		"'permission.asked'",
		"'question.asked'",
		"'session.idle'",
		// Receiver contract: permission_prompt is a needsHumanNotification type
		// in bossd's recordQuestionByType, so it is what SETS the signal.
		"notification_type: 'permission_prompt'",
		"notification_type: 'session_idle'",
		"cleared: true",
		"/hooks/question/",
		"127.0.0.1",
		// Secrets come from the environment, at runtime.
		"process.env.BOSS_HOOK_PORT",
		"process.env.BOSS_HOOK_TOKEN",
		// Sub-agent suppression + the early-exit guard.
		"parentID",
		"AbortSignal.timeout",
	}
	for _, want := range mustContain {
		if !strings.Contains(js, want) {
			t.Errorf("embedded asset missing %q", want)
		}
	}

	// No templating: the asset must be static so injection is byte-identical
	// for every run and no secret is ever written into the worktree.
	mustNotContain := []string{"{{", "}}", "%s", "%d", "__BOSS_HOOK", "<TOKEN>", "<PORT>"}
	for _, bad := range mustNotContain {
		if strings.Contains(js, bad) {
			t.Errorf("embedded asset contains placeholder %q — it must be static, with no baked port/token", bad)
		}
	}
}

// TestStartRunInjectsAndCleansUpQuestionHook drives the real StartRun → run-end
// path with a fake opencode shell: the asset must exist while the run is being
// launched and be gone once the process exits.
func TestStartRunInjectsAndCleansUpQuestionHook(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	// Echo a session id so SessionIDFromOutput re-keys the process, then sleep
	// briefly so the asset is still on disk when StartRun returns.
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"step_start","sessionID":"ses_hook0001abcd"}'; sleep 0.2; exit 0`)))
	onCleanup, awaitCleanup := hookCleanupWatcher(t, "ses_hook0001abcd")
	srv := &Server{logger: zerolog.Nop(), runner: r, onHookCleanup: onCleanup}

	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, LogPath: logPath, ExtraEnv: hookEnv(),
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.SessionId != "ses_hook0001abcd" {
		t.Fatalf("StartRun.SessionId = %q, want ses_hook0001abcd", start.SessionId)
	}
	if _, err := os.Stat(questionHookPath(dir)); err != nil {
		t.Fatalf("asset not injected before the run: %v", err)
	}

	awaitCleanup()
	if _, err := os.Stat(questionHookPath(dir)); err == nil {
		t.Error("asset survived the run; run-end cleanup did not fire")
	}
}

// TestStartRunWithoutHookEnvWritesNothing proves an opencode run with no
// loopback context still starts and leaves the worktree untouched.
func TestStartRunWithoutHookEnvWritesNothing(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"step_start","sessionID":"ses_noenv0001abc"}'; exit 0`)))
	var cleanupFired atomic.Bool
	srv := &Server{logger: zerolog.Nop(), runner: r, onHookCleanup: func(string) { cleanupFired.Store(true) }}

	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, LogPath: logPath, ExtraEnv: map[string]string{"UNRELATED": "1"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	waitExit(t, srv, start.SessionId)

	if _, err := os.Stat(filepath.Join(dir, questionHookRootDir)); err == nil {
		t.Errorf("%s created for a run with no hook env", questionHookRootDir)
	}
	awaitHookCleanupAbsent(t, &cleanupFired)
}

// TestStartRunSweepsAnAssetItCouldNotRewrite covers the install-FAILURE path,
// which is the one place stale or broken asset bytes can outlive the run.
//
// installQuestionHook returns injected=false on a write error, which arms
// NEITHER the failed-start sweep nor the run-end goroutine — so without the
// explicit sweep on the error path, whatever is at the asset path stays there
// and opencode loads it. That matters because os.WriteFile truncates before it
// writes (a mid-write failure leaves a partial file) and because a crashed
// earlier run can have left an asset the daemon can no longer refresh.
//
// The failure is forced through the real code path rather than by faking a
// return value: a read-only asset file makes the O_WRONLY open fail before it
// truncates, so the stale bytes are intact when the sweep runs.
func TestStartRunSweepsAnAssetItCouldNotRewrite(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	path := questionHookPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("seed plugins dir: %v", err)
	}
	// A stale asset from an earlier run that bossd can no longer overwrite.
	if err := os.WriteFile(path, []byte("// stale bytes from a crashed run\n"), 0o400); err != nil {
		t.Fatalf("seed stale asset: %v", err)
	}
	// root ignores the mode bits, which would make the install succeed and the
	// assertion below meaningless.
	if err := os.WriteFile(path, []byte("probe"), 0o400); err == nil {
		t.Skip("this process can write a 0400 file (running as root?); cannot force an install failure")
	}

	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"step_start","sessionID":"ses_partial01ab"}'; exit 0`)))
	var cleanupFired atomic.Bool
	srv := &Server{logger: zerolog.Nop(), runner: r, onHookCleanup: func(string) { cleanupFired.Store(true) }}

	// The run must still start: a hook that cannot be written is a degradation,
	// never a reason to fail the session.
	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, LogPath: logPath, ExtraEnv: hookEnv(),
	})
	if err != nil {
		t.Fatalf("StartRun failed on an un-writable hook path: %v", err)
	}
	waitExit(t, srv, start.SessionId)

	awaitHookCleanupAbsent(t, &cleanupFired)
	if _, err := os.Stat(path); err == nil {
		t.Error("the un-rewritable asset survived a failed install; opencode would load unknown bytes")
	}
}

// TestRemoveAgentRunHookDeletesAsset exercises the daemon-driven inverse: bossd
// calls this RPC for every agent after run completion, and for opencode it must
// now actually delete the injected plugin (and report supported).
func TestRemoveAgentRunHookDeletesAsset(t *testing.T) {
	dir := t.TempDir()
	if _, err := installQuestionHook(dir, hookEnv()); err != nil {
		t.Fatalf("install: %v", err)
	}
	srv := &Server{logger: zerolog.Nop()}

	for i := range 2 {
		resp, err := srv.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{
			WorkDir: dir, AgentSessionId: "ses_hook0001abcd",
		})
		if err != nil {
			t.Fatalf("RemoveAgentRunHook call %d: %v", i+1, err)
		}
		if !resp.IsSupported {
			t.Errorf("RemoveAgentRunHook.IsSupported = false, want true")
		}
	}
	if _, err := os.Stat(questionHookPath(dir)); err == nil {
		t.Error("asset survived RemoveAgentRunHook")
	}
}

// TestConfigureFinalizeHookStaysUnsupported pins a production-critical
// invariant. bossd keys finalize routing off this flag
// (agentClientHookSupport → services/bossd/internal/agent/poll_fallback.go):
// claiming support stops ExitStatus polling and makes the daemon wait for a
// Stop-hook POST that headless opencode never sends, hanging finalize forever.
// BOS-486's question hook is injected on the StartRun path precisely so this can
// stay false. Do NOT "fix" this test by flipping the expectation.
func TestConfigureFinalizeHookStaysUnsupported(t *testing.T) {
	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir:        t.TempDir(),
		SessionId:      "sess-1",
		AgentSessionId: "ses_hook0001abcd",
		HookToken:      "tok-secret",
		HookPort:       45678,
	})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook: %v", err)
	}
	if resp.IsSupported {
		t.Fatal("ConfigureFinalizeHook.IsSupported = true — this breaks finalize for every opencode session; see the RPC's doc comment")
	}
}

// hookCleanupWatcher returns a Server callback plus a wait func that blocks
// until the cleanup goroutine for THIS test's run has finished, so the
// assertion after it is deterministic rather than a poll.
//
// Each watcher is scoped to one session id, so a test cannot accidentally
// observe another run's cleanup: an unexpected id fails loudly.
func hookCleanupWatcher(t *testing.T, wantSession string) (func(string), func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	// Recorded rather than reported from the goroutine: a t.Errorf after the
	// test function returns panics the whole run, and this callback can in
	// principle outlive it. wait() surfaces it on the test's own goroutine.
	var unexpected atomic.Value
	cb := func(sessionID string) {
		if sessionID != wantSession {
			unexpected.Store(sessionID)
			return
		}
		once.Do(func() { close(done) })
	}
	wait := func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("question-hook cleanup did not finish within 5s")
		}
		if got := unexpected.Load(); got != nil {
			t.Errorf("cleanup also fired for run %q, want only %q", got, wantSession)
		}
	}
	return cb, wait
}

// awaitHookCleanupAbsent asserts no cleanup callback fires — the observable
// form of "nothing was injected, so no run-end cleanup was armed".
//
// Callers reach here after waitExit, so a wrongly-armed goroutine is already
// past its runner.Wait and has only an os.Remove left to do. The grace window
// gives it time to be scheduled: proving a NEGATIVE cannot be instantaneous,
// and checking the flag immediately would pass whether or not the goroutine
// existed. The window is generous relative to the work it is waiting on.
func awaitHookCleanupAbsent(t *testing.T, fired *atomic.Bool) {
	t.Helper()
	deadline := time.After(hookCleanupGrace)
	for {
		if fired.Load() {
			t.Error("run-end cleanup fired even though nothing was injected")
			return
		}
		select {
		case <-deadline:
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// hookCleanupGrace bounds awaitHookCleanupAbsent's negative proof.
const hookCleanupGrace = 250 * time.Millisecond
