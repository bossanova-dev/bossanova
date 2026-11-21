package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The live tests drive the installed, signed-in agy and spend real model
// quota, so they run only with AGY_LIVE=1:
//
//	AGY_LIVE=1 go test -run TestLive -v ./plugins/bossd-plugin-antigravity
//
// liveTurnBudget bounds one real agy turn, which takes seconds to start and
// tens of seconds to finish; livePollInterval paces the pane and transcript
// reads while it runs. These tests drive a real external process, so a
// synctest bubble cannot stand in for them.
const (
	liveTurnBudget   = 2 * time.Minute
	livePollInterval = time.Second
)

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("AGY_LIVE") != "1" {
		t.Skip("set AGY_LIVE=1 to run against the installed agy")
	}
	if _, err := exec.LookPath("agy"); err != nil {
		t.Skip("agy is not on PATH")
	}
}

func liveRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestLiveHeadlessRun(t *testing.T) {
	requireLive(t)
	s := newServer(nil, zerolog.Nop())
	logPath := filepath.Join(t.TempDir(), "run.log")
	resp, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: liveRepo(t), Plan: "Reply with the single word ok.\nDo not use any tools.", LogPath: logPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetSessionId()
	if !canonicalConversationID(id) {
		t.Fatalf("session id %q is not an agy conversation id", id)
	}
	waitRun(t, s, id)
	exit, _ := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: id})
	if exit.GetExitError() != "" {
		log, _ := os.ReadFile(logPath)
		t.Fatalf("run failed: %s\n%s", exit.GetExitError(), log)
	}
	transcript, err := s.ReadTranscript(context.Background(), &bossanovav1.ReadTranscriptRequest{AgentSessionId: id})
	if err != nil || !transcript.GetExists() {
		t.Fatalf("transcript = %v, %v", transcript, err)
	}
	if !strings.Contains(strings.ToLower(transcript.GetFinalAssistantText()), "ok") {
		t.Fatalf("final reply %q", transcript.GetFinalAssistantText())
	}
	if got := transcript.GetMessages()[0].GetText(); got != "Reply with the single word ok.\nDo not use any tools." {
		t.Fatalf("first user message %q is not the plan", got)
	}
	t.Logf("conversation %s replied %q", id, transcript.GetFinalAssistantText())
}

func TestLiveInteractiveLaunch(t *testing.T) {
	requireLive(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not on PATH")
	}
	s := newServer(nil, zerolog.Nop())
	repo := liveRepo(t)
	launched := time.Now()
	cmd, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		WorktreePath: repo, InitialPrompt: "Reply with the single word ok. Do not use any tools.",
	})
	if err != nil {
		t.Fatal(err)
	}
	name := "agy-live-" + strconv.Itoa(os.Getpid())
	tmux := func(args ...string) string {
		out, err := exec.Command("tmux", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", args, err, out)
		}
		return string(out)
	}
	tmux(append([]string{"new-session", "-d", "-s", name, "-x", "160", "-y", "45", "-c", repo}, cmd.GetArgv()...)...)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
	pid, err := strconv.Atoi(strings.TrimSpace(tmux("display", "-p", "-t", name, "#{pane_pid}")))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), liveTurnBudget)
	defer cancel()
	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()
	var id string
	for id == "" {
		resolved, _ := s.ResolveInteractiveSessionID(ctx, &bossanovav1.ResolveInteractiveSessionIDRequest{
			WorkDir: repo, PanePid: int32(pid), LaunchedAfter: timestamppb.New(launched),
		})
		id = resolved.GetSessionId()
		select {
		case <-ctx.Done():
			t.Fatalf("never resolved the conversation; pane:\n%s", tmux("capture-pane", "-p", "-t", name))
		case <-ticker.C:
		}
	}
	for {
		transcript, _ := s.ReadTranscript(ctx, &bossanovav1.ReadTranscriptRequest{AgentSessionId: id})
		pane := tmux("capture-pane", "-p", "-t", name)
		working, _ := s.HasWorkingIndicator(ctx, &bossanovav1.HasWorkingIndicatorRequest{PaneContent: []byte(pane)})
		if transcript.GetFinalAssistantText() != "" && !working.GetIsWorking() {
			question, _ := s.HasQuestionPrompt(ctx, &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(pane)})
			if question.GetHasPrompt() || question.GetBlocksInput() {
				t.Fatalf("idle pane reads as a question:\n%s", pane)
			}
			t.Logf("conversation %s replied %q", id, transcript.GetFinalAssistantText())
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("turn never finished; pane:\n%s", pane)
		case <-ticker.C:
		}
	}
}

func TestLiveSuggestPRTitleAndUsage(t *testing.T) {
	requireLive(t)
	s := newServer(nil, zerolog.Nop())
	title, err := s.SuggestPRTitle(context.Background(), &bossanovav1.SuggestPRTitleRequest{
		WorkDir: liveRepo(t), CurrentTitle: "wip", GitLog: "feat: add an Antigravity agent runner plugin", ShouldTrustProjectConfig: true,
	})
	if err != nil || !title.GetSupported() || title.GetTitle() == "" {
		t.Fatalf("title = %v, %v", title, err)
	}
	t.Logf("suggested title %q", title.GetTitle())

	usage, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{CredentialEnv: map[string]string{"AGY_CLI_HIDE_LOGO": "1"}})
	if err != nil || usage.GetStatus().GetStatus() == bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
		t.Fatalf("usage = %v, %v", usage, err)
	}
	t.Logf("usage status %v util_5h=%.2f windows=%d", usage.GetStatus().GetStatus(), usage.GetStatus().GetUtil_5H(), len(usage.GetStatus().GetModelWindows()))
}
