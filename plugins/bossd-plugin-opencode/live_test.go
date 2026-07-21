package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestLiveOpencodeFreshThenResume is the BOS-437 live-validation harness: it
// drives the REAL opencode CLI through the plugin's Server.StartRun → runner path
// (the exact code bossd's gRPC dispatch invokes) across two sessions — one FRESH,
// one RESUME-by-`ses_*`-id — and asserts the fragile seams hold against the
// installed binary:
//
//   - argv: buildArgv's version-aware permission flag actually runs (a rejected
//     flag would make opencode print usage and produce no session id);
//   - session-id capture: StartRun returns opencode's echoed `ses_*` id;
//   - resume continuity: session #2, resumed by that id, recalls a codeword only
//     stated in session #1 (proving it continues the prior conversation, not a
//     fresh one);
//   - clean worktree: `git status` after each live run is clean (part 3's
//     shadow-git / ListIgnoredDirtyFiles contract — opencode state lives outside
//     the worktree).
//
// It is opt-in (OPENCODE_LIVE=1) and self-skips when the binary or provider auth
// is absent, so CI — which has neither — stays green. Run locally with a real
// opencode + `~/.local/share/opencode/auth.json`:
//
//	OPENCODE_LIVE=1 go test ./plugins/bossd-plugin-opencode -run TestLiveOpencode -v
func TestLiveOpencodeFreshThenResume(t *testing.T) {
	requireLiveOpencode(t)

	dir := t.TempDir()
	gitInit(t, dir)
	// Logs live OUTSIDE the git worktree so the clean-worktree assertion checks
	// only opencode's own footprint, not the harness's capture files.
	logDir := t.TempDir()

	version := probeOpencodeVersion("")
	r := NewRunner(zerolog.Nop(), WithCLIVersion(version))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	const codeword = "BOSSANOVA437XYZ"

	// --- Session #1: FRESH ---
	log1 := filepath.Join(logDir, "run1.log")
	fresh, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:   dir,
		Plan:      "Remember this codeword for later: " + codeword + ". Reply with exactly the word OK and nothing else.",
		SessionId: "live-fresh-hint",
		LogPath:   log1,
	})
	if err != nil {
		t.Fatalf("fresh StartRun: %v", err)
	}
	if !strings.HasPrefix(fresh.SessionId, "ses_") {
		t.Fatalf("fresh SessionId = %q, want an echoed ses_* id", fresh.SessionId)
	}
	t.Logf("fresh session id: %s", fresh.SessionId)
	exit1 := waitLiveExit(t, srv, fresh.SessionId)
	if exit1.GetExitError() != "" {
		t.Fatalf("fresh run exit error: %s\nlog:\n%s", exit1.GetExitError(), readFile(t, log1))
	}
	assertWorktreeClean(t, dir, "after fresh run")

	// --- Session #2: RESUME by ses_* id ---
	log2 := filepath.Join(logDir, "run2.log")
	resume, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:  dir,
		Plan:     "What codeword did I ask you to remember? Reply with only the codeword.",
		ResumeId: &fresh.SessionId,
		LogPath:  log2,
	})
	if err != nil {
		t.Fatalf("resume StartRun: %v", err)
	}
	t.Logf("resume session id: %s", resume.SessionId)
	exit2 := waitLiveExit(t, srv, resume.SessionId)
	if exit2.GetExitError() != "" {
		t.Fatalf("resume run exit error: %s\nlog:\n%s", exit2.GetExitError(), readFile(t, log2))
	}
	assertWorktreeClean(t, dir, "after resume run")

	// Continuity: the resumed conversation must recall the codeword from #1.
	log2Body := readFile(t, log2)
	if !strings.Contains(log2Body, codeword) {
		t.Fatalf("resumed run did not recall the codeword %q — resume may not be continuing session #1.\nlog:\n%s", codeword, log2Body)
	}
	t.Logf("resume recalled the codeword — continuity confirmed")
}

func requireLiveOpencode(t *testing.T) {
	t.Helper()
	if os.Getenv("OPENCODE_LIVE") != "1" {
		t.Skip("OPENCODE_LIVE!=1 — skipping live opencode validation (opt-in)")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on PATH — skipping live validation")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir — skipping live validation")
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "opencode", "auth.json")); err != nil {
		t.Skip("opencode auth.json absent — skipping live validation")
	}
}

func waitLiveExit(t *testing.T, s *Server, sid string) *bossanovav1.AgentExitStatusResponse {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		exit, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: sid})
		if err == nil && exit.IsComplete {
			return exit
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("live run %s never completed within the deadline", sid)
	return nil
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "live@example.com"},
		{"config", "user.name", "live"},
		{"commit", "--allow-empty", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func assertWorktreeClean(t *testing.T, dir, when string) {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status %s: %v", when, err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("worktree dirty %s (opencode leaked agent-authored paths):\n%s", when, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
