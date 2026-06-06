package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// Tests for Lifecycle.StartTmuxChat — the generalized form of the cron-only
// helper that previously lived in startCronTmuxChat. Cron-specific behavior
// stays in lifecycle_test.go (the cron test cluster around
// TestStartSession_CronJobID_*); this file targets the generic method
// directly so any future caller (repair, interactive UI button) gets the
// same coverage.

func TestChatInputRenderCommandUsesAgentPrefix(t *testing.T) {
	input := ChatInput{Command: "boss-repair"}
	if got := input.render("$"); got != "$boss-repair" {
		t.Fatalf("rendered command = %q, want $boss-repair", got)
	}
}

func TestChatInputRenderCommandDefaultsToSlashPrefix(t *testing.T) {
	input := ChatInput{Command: "boss-repair"}
	if got := input.render(""); got != "/boss-repair" {
		t.Fatalf("rendered command = %q, want /boss-repair", got)
	}
}

func TestChatInputRenderPromptPreservesRawText(t *testing.T) {
	input := ChatInput{Prompt: "/boss-repair"}
	if got := input.render("$"); got != "/boss-repair" {
		t.Fatalf("rendered prompt = %q, want /boss-repair", got)
	}
}

func TestCronChatInputFromPromptConvertsLeadingSlashCommand(t *testing.T) {
	input := cronChatInputFromPrompt("/bs-technical-debt")
	if input.Prompt != "" {
		t.Fatalf("Prompt = %q, want empty", input.Prompt)
	}
	if input.Command != "/bs-technical-debt" {
		t.Fatalf("Command = %q, want /bs-technical-debt", input.Command)
	}
}

func TestCronChatInputFromPromptConvertsLeadingDollarCommand(t *testing.T) {
	input := cronChatInputFromPrompt("$bs-mutation-test")
	if input.Prompt != "" {
		t.Fatalf("Prompt = %q, want empty", input.Prompt)
	}
	if input.Command != "$bs-mutation-test" {
		t.Fatalf("Command = %q, want $bs-mutation-test", input.Command)
	}
}

func TestCronChatInputFromPromptTrimsSurroundingWhitespace(t *testing.T) {
	input := cronChatInputFromPrompt("  /bs-mutation-test  ")
	if input.Prompt != "" {
		t.Fatalf("Prompt = %q, want empty", input.Prompt)
	}
	if input.Command != "/bs-mutation-test" {
		t.Fatalf("Command = %q, want /bs-mutation-test", input.Command)
	}
}

// Embedded commands must NOT be extracted: doing so silently truncates the
// surrounding free-text instruction, which is the user's actual cron plan.
func TestCronChatInputFromPromptKeepsEmbeddedCommandAsPrompt(t *testing.T) {
	prompt := "Run /bs-mutation-test"
	input := cronChatInputFromPrompt(prompt)
	if input.Prompt != prompt {
		t.Fatalf("Prompt = %q, want %q", input.Prompt, prompt)
	}
	if input.Command != "" {
		t.Fatalf("Command = %q, want empty", input.Command)
	}
}

// A single-line free-text prompt containing a slash (path/URL) or dollar
// (price) must stay a prompt rather than being truncated into a bogus command.
func TestCronChatInputFromPromptKeepsFreeTextWithSlashOrDollar(t *testing.T) {
	for _, prompt := range []string{
		"Review the auth changes in /internal/auth",
		"Add a $5 discount banner",
		"Summarize https://example.com/foo",
	} {
		input := cronChatInputFromPrompt(prompt)
		if input.Prompt != prompt {
			t.Fatalf("prompt %q: Prompt = %q, want unchanged", prompt, input.Prompt)
		}
		if input.Command != "" {
			t.Fatalf("prompt %q: Command = %q, want empty", prompt, input.Command)
		}
	}
}

func TestCronChatInputFromPromptKeepsMultilinePrompt(t *testing.T) {
	prompt := "/bs-mutation-test\nwith extra notes"
	input := cronChatInputFromPrompt(prompt)
	if input.Prompt != prompt {
		t.Fatalf("Prompt = %q, want %q", input.Prompt, prompt)
	}
	if input.Command != "" {
		t.Fatalf("Command = %q, want empty", input.Command)
	}
}

func TestCronChatInputFromPromptKeepsEmptyAndWhitespace(t *testing.T) {
	for _, prompt := range []string{"", "   ", "\t"} {
		input := cronChatInputFromPrompt(prompt)
		if input.Command != "" {
			t.Fatalf("prompt %q: Command = %q, want empty", prompt, input.Command)
		}
		if input.Prompt != prompt {
			t.Fatalf("prompt %q: Prompt = %q, want unchanged", prompt, input.Prompt)
		}
	}
}

// startTmuxChatHarness wires up everything Lifecycle.StartTmuxChat needs:
// in-memory stores, a fake tmux client, an agent runner client returning
// realistic argv, and an agentLogsDir. Each test instantiates one of
// these, optionally tweaks the failure-injection knobs, then calls
// StartTmuxChat directly.
type startTmuxChatHarness struct {
	t          *testing.T
	sessions   *mockSessionStore
	repos      *mockRepoStore
	chats      *mockAgentChatStore
	tmuxFake   *fakeTmux
	tmuxClient *tmux.Client
	agentFake  *fakeAgentForLifecycle
	agentRun   *mockAgentRunner
	wt         *mockWorktreeManager
	logsDir    string
	lc         *Lifecycle
}

func newStartTmuxChatHarness(t *testing.T) *startTmuxChatHarness {
	t.Helper()
	h := &startTmuxChatHarness{
		t:         t,
		sessions:  newMockSessionStore(),
		repos:     newMockRepoStore(),
		chats:     &mockAgentChatStore{},
		tmuxFake:  newFakeTmux(),
		agentFake: newFakeAgent(),
		agentRun:  newMockAgentRunner(),
		wt:        &mockWorktreeManager{},
		logsDir:   t.TempDir(),
	}
	h.tmuxClient = tmux.NewClient(tmux.WithCommandFactory(h.tmuxFake.factory))
	h.repos.repos["repo-abcdef12"] = &models.Repo{
		ID:                "repo-abcdef12",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	h.sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-abcdef12",
		Title:        "Some session",
		Plan:         "do the thing",
		BaseBranch:   "main",
		WorktreePath: "/tmp/worktrees/test/sess-1",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
	}
	h.lc = NewLifecycle(h.sessions, h.repos, h.chats, &stubCronJobStore{}, h.wt, h.agentRun, h.tmuxClient, newMockVCSProvider(), zerolog.Nop())
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake})
	h.lc.SetAgentLogsDir(h.logsDir)
	return h
}

// findCall returns the first recorded tmux call matching subcommand, or nil.
func (h *startTmuxChatHarness) findCall(subcommand string) *recordedTmuxCall {
	h.tmuxFake.mu.Lock()
	defer h.tmuxFake.mu.Unlock()
	for i := range h.tmuxFake.calls {
		if h.tmuxFake.calls[i].subcommand == subcommand {
			return &h.tmuxFake.calls[i]
		}
	}
	return nil
}

func writeRepairChatLogAt(t *testing.T, h *startTmuxChatHarness, agentSessionID string, modTime time.Time) {
	t.Helper()
	logPath := h.lc.agentLogPathFor(agentSessionID)
	if err := os.WriteFile(logPath, []byte("repair output\n"), 0o600); err != nil {
		t.Fatalf("write repair chat log: %v", err)
	}
	if err := os.Chtimes(logPath, modTime, modTime); err != nil {
		t.Fatalf("set repair chat log time: %v", err)
	}
}

func ptr[T any](v T) *T {
	return &v
}

func TestRepairChatStaleForReclaim_DefaultThresholdAndFailClosedCases(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		withTmuxName  bool
		withLogAt     *time.Time
		wantStale     bool
		wantReasonHas string
	}{
		{
			name:          "idle for nineteen minutes fifty nine seconds is not reclaimable",
			withTmuxName:  true,
			withLogAt:     ptr(now.Add(-(19*time.Minute + 59*time.Second))),
			wantStale:     false,
			wantReasonHas: "threshold is 20m0s",
		},
		{
			name:          "idle for twenty minutes is reclaimable",
			withTmuxName:  true,
			withLogAt:     ptr(now.Add(-20 * time.Minute)),
			wantStale:     true,
			wantReasonHas: "threshold is 20m0s",
		},
		{
			name:          "missing log is not reclaimable",
			withTmuxName:  true,
			wantStale:     false,
			wantReasonHas: "agent log for repair-agent-boundary is missing",
		},
		{
			name:          "chat without live tmux pointer is reclaimable immediately",
			wantStale:     true,
			wantReasonHas: "repair chat has no live tmux pointer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStartTmuxChatHarness(t)
			agentSessionID := "repair-agent-boundary"
			var tmuxName *string
			if tt.withTmuxName {
				tmuxName = ptr("boss-repair-boundary")
			}
			chat := &models.AgentChat{
				AgentSessionID:  agentSessionID,
				Title:           "Repair: boundary",
				TmuxSessionName: tmuxName,
			}
			if tt.withLogAt != nil {
				writeRepairChatLogAt(t, h, agentSessionID, *tt.withLogAt)
			}

			stale, reason, err := RepairChatStaleForReclaim(h.logsDir, chat, now)
			if err != nil {
				t.Fatalf("RepairChatStaleForReclaim error = %v", err)
			}
			if stale != tt.wantStale {
				t.Fatalf("stale = %v, want %v; reason=%q", stale, tt.wantStale, reason)
			}
			if !strings.Contains(reason, tt.wantReasonHas) {
				t.Fatalf("reason = %q, want substring %q", reason, tt.wantReasonHas)
			}
		})
	}
}

func TestKillChatTmuxSession_KillsLiveTmuxThenClearsChatPointer(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	tmuxName := "bossd-agent-run-existing"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {
			{
				SessionID:       "sess-1",
				AgentSessionID:  "agent-existing",
				TmuxSessionName: &tmuxName,
			},
		},
	}

	if err := h.lc.KillChatTmuxSession(t.Context(), "sess-1", "agent-existing"); err != nil {
		t.Fatalf("KillChatTmuxSession: %v", err)
	}
	call := h.findCall("kill-session")
	if call == nil {
		t.Fatal("expected tmux kill-session")
	}
	if got, want := strings.Join(call.args, " "), "-t "+tmuxName; got != want {
		t.Fatalf("kill-session args = %q, want %q", got, want)
	}
	if len(h.chats.tmuxNameUpdates) != 1 {
		t.Fatalf("tmux name updates = %d, want 1", len(h.chats.tmuxNameUpdates))
	}
	if got := h.chats.tmuxNameUpdates[0].agentSessionID; got != "agent-existing" {
		t.Fatalf("cleared agentSessionID = %q, want agent-existing", got)
	}
	if h.chats.tmuxNameUpdates[0].name != nil {
		t.Fatalf("cleared tmux name = %q, want nil", *h.chats.tmuxNameUpdates[0].name)
	}
}

func TestKillChatTmuxSession_DoesNotClearChatPointerWhenKillFails(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	tmuxName := "bossd-agent-run-existing"
	h.tmuxFake.failSubcommand["kill-session"] = true
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {
			{
				SessionID:       "sess-1",
				AgentSessionID:  "agent-existing",
				TmuxSessionName: &tmuxName,
			},
		},
	}

	if err := h.lc.KillChatTmuxSession(t.Context(), "sess-1", "agent-existing"); err == nil {
		t.Fatal("KillChatTmuxSession error = nil, want error")
	}
	if len(h.chats.tmuxNameUpdates) != 0 {
		t.Fatalf("tmux name updates = %d, want 0", len(h.chats.tmuxNameUpdates))
	}
}

// TestStartTmuxChat_HappyPath exercises the full extracted path: idempotency
// check finds nothing → BuildInteractiveCommand → tmux NewSession → row
// Create with the supplied title → UpdateTmuxSessionName → SendPlan
// (bracketed paste). Verifies the title is exactly what the caller passed
// (NOT cron's `Run "..."` template) and that the argv came from the agent
// plugin rather than a hardcoded slice.
func TestStartTmuxChat_HappyPath(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	const supplyTitle = "Repair: Some session"
	const supplyPrompt = "/boss-repair"

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: supplyPrompt}, supplyTitle, HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if agentSessionID == "" {
		t.Fatal("expected non-empty agentSessionID on success")
	}

	// agent_chats row was created with the supplied title (NOT cron's template).
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 agentChats.Create call, got %d", len(h.chats.createCalls))
	}
	if h.chats.createCalls[0].Title != supplyTitle {
		t.Errorf("Title = %q, want %q", h.chats.createCalls[0].Title, supplyTitle)
	}
	if h.chats.createCalls[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", h.chats.createCalls[0].SessionID)
	}
	if h.chats.createCalls[0].AgentSessionID != agentSessionID {
		t.Errorf("AgentSessionID = %q, want %q", h.chats.createCalls[0].AgentSessionID, agentSessionID)
	}
	if h.chats.createCalls[0].AgentName != "claude" {
		t.Errorf("AgentName = %q, want claude", h.chats.createCalls[0].AgentName)
	}

	// Tmux NewSession was called with argv from the agent plugin's
	// BuildInteractiveCommand, not a hardcoded slice. The fake agent
	// returns the bare claude argv (no shell wrapper) — the production
	// plugin must do the same so claude gets a real PTY on stdout; the
	// daemon now captures pane output via `tmux pipe-pane` post-spawn.
	newSess := h.findCall("new-session")
	if newSess == nil {
		t.Fatal("expected tmux new-session call")
	}
	joined := strings.Join(newSess.args, " ")
	if strings.Contains(joined, " sh -c ") || strings.Contains(joined, " bash -c ") {
		t.Errorf("new-session argv must NOT be shell-wrapped (breaks claude TTY detection), got: %s", joined)
	}
	if !strings.Contains(joined, "claude --session-id "+agentSessionID) {
		t.Errorf("expected new-session argv to embed claude --session-id %s, got: %s", agentSessionID, joined)
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetWorktreePath(); got != h.sessions.sessions["sess-1"].WorktreePath {
		t.Errorf("BuildInteractiveCommand WorktreePath = %q, want %q", got, h.sessions.sessions["sess-1"].WorktreePath)
	}

	// UpdateTmuxSessionName wrote a non-empty resolved tmux name onto the row.
	if len(h.chats.tmuxNameUpdates) != 1 {
		t.Fatalf("expected 1 UpdateTmuxSessionName call, got %d", len(h.chats.tmuxNameUpdates))
	}
	if h.chats.tmuxNameUpdates[0].agentSessionID != agentSessionID {
		t.Errorf("UpdateTmuxSessionName agentSessionID = %q, want %q", h.chats.tmuxNameUpdates[0].agentSessionID, agentSessionID)
	}
	if h.chats.tmuxNameUpdates[0].name == nil || *h.chats.tmuxNameUpdates[0].name == "" {
		t.Error("expected non-nil/non-empty tmux name persisted on chat row")
	}

	// SendPlan must have run: load-buffer + paste-buffer + send-keys.
	for _, sub := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if !h.tmuxFake.hasSubcommand(sub) {
			t.Errorf("expected tmux %s call from SendPlan, none recorded", sub)
		}
	}

	// pipe-pane must have armed: this is the new on-disk capture path
	// that replaces the broken bash-c-tee wrapping the claude plugin
	// used to apply in BuildInteractiveCommand. Without this, repair
	// chats (and any other unattended chat) would lose their log
	// history the moment the tmux session ended.
	pipe := h.findCall("pipe-pane")
	if pipe == nil {
		t.Fatal("expected tmux pipe-pane call after new-session")
	}
	pipeJoined := strings.Join(pipe.args, " ")
	if !strings.Contains(pipeJoined, "cat >>") {
		t.Errorf("pipe-pane args must use append-mode `cat >>` so re-spawns don't truncate prior capture, got: %s", pipeJoined)
	}
	if !strings.Contains(pipeJoined, agentSessionID+".log") {
		t.Errorf("pipe-pane log path must include agent_session_id+.log, got: %s", pipeJoined)
	}

	// No row deletions expected on the happy path.
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected 0 deletes on happy path, got %v", h.chats.deletedAgentSessionIDs)
	}
}

func TestStartTmuxChat_UsesAgentReadyMarker(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.ReadyMarker = "›"
	h.tmuxFake.capturePaneOutput = "OpenAI Codex\n›\n"

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair"}, "Repair: Some session", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if !h.tmuxFake.hasSubcommand("load-buffer") {
		t.Fatal("expected SendPlan to accept the agent ready marker and load the prompt")
	}
}

// TestStartTmuxChat_TmuxUnavailable verifies fail-closed behavior when tmux
// isn't on PATH: typed FailedPrecondition error, no chat row created.
func TestStartTmuxChat_TmuxUnavailable(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.available = false

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when tmux unavailable")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls when tmux unavailable, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_AgentRunnerNotLoaded verifies fail-closed behavior when
// the session's AgentName has no registered AgentRunnerClient.
func TestStartTmuxChat_AgentRunnerNotLoaded(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Replace the agent registry with an empty map so claude is unloaded.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{})

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when agent runner not loaded")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls when agent missing, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_NewSessionFails verifies that a tmux NewSession failure
// returns an error before any agent_chats row is created.
func TestStartTmuxChat_NewSessionFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.failSubcommand["new-session"] = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when tmux new-session fails")
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 agentChats.Create calls when new-session fails, got %d", len(h.chats.createCalls))
	}
	// new-session was attempted but kill-session was NOT — there's no orphan
	// to clean up because the spawn never succeeded.
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Error("expected new-session to have been attempted")
	}
	if h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("did not expect kill-session when new-session itself failed")
	}
}

// TestStartTmuxChat_EmptyArgvFails verifies that an empty argv from
// BuildInteractiveCommand is treated as a hard precondition failure
// before any tmux process is spawned.
func TestStartTmuxChat_EmptyArgvFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Make BuildInteractiveCommand return empty argv.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": &emptyArgvAgent{}})

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when BuildInteractiveCommand returns empty argv")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Error("did not expect tmux new-session when argv was empty")
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls on empty argv, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_ChatCreateFails verifies that an agentChats.Create
// failure after tmux is live tears tmux back down and leaves no row.
func TestStartTmuxChat_ChatCreateFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.createErr = fmt.Errorf("simulated DB failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when Create fails")
	}
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Error("expected tmux new-session before Create attempt")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session to clean up after Create failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected 0 deletes (Create itself failed, no row to delete), got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_UpdateTmuxSessionNameFails verifies that an
// UpdateTmuxSessionName failure tears tmux down AND preserves the
// agent_chats row with a start_error reason. Pre-#350 the row was
// deleted on failure, which made repeated repair attempts vanish from
// the chat list entirely; the row is now kept so the operator can see
// the attempt and read why it failed.
func TestStartTmuxChat_UpdateTmuxSessionNameFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.updateTmuxNameErr = fmt.Errorf("simulated update failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when UpdateTmuxSessionName fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after UpdateTmuxSessionName failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected NO row delete after UpdateTmuxSessionName failure (row must be preserved as failed-to-start), got %v", h.chats.deletedAgentSessionIDs)
	}
	if len(h.chats.markStartFailedCalls) != 1 {
		t.Fatalf("expected 1 MarkStartFailed call after UpdateTmuxSessionName failure, got %d", len(h.chats.markStartFailedCalls))
	}
	if !strings.Contains(h.chats.markStartFailedCalls[0].reason, "persist tmux session name failed") {
		t.Errorf("MarkStartFailed reason missing context, got %q", h.chats.markStartFailedCalls[0].reason)
	}
}

// TestStartTmuxChat_SendPlanFails verifies that a SendPlan failure
// tears tmux down AND preserves the agent_chats row stamped with a
// start_error. This is the exact failure mode the user hit on PRs
// #9499, #361, #362 — claude bailed before showing the ❯ ready marker,
// SendPlan timed out at 5 s, repair counter incremented to 321×, and
// the chat list showed zero entries because the row was being deleted.
// Preserving the row is what surfaces the "(failed to start)" badge in
// the chat list so the operator can finally see what each attempt did.
func TestStartTmuxChat_SendPlanFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Force load-buffer (the first stage of SendPlan) to fail. The
	// capture-pane ready-marker poll runs first; we leave that succeeding
	// so SendPlan reaches the real failure.
	h.tmuxFake.failSubcommand["load-buffer"] = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when SendPlan fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after SendPlan failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected NO row delete after SendPlan failure (row must be preserved as failed-to-start), got %v", h.chats.deletedAgentSessionIDs)
	}
	if len(h.chats.markStartFailedCalls) != 1 {
		t.Fatalf("expected 1 MarkStartFailed call after SendPlan failure, got %d", len(h.chats.markStartFailedCalls))
	}
	if !strings.Contains(h.chats.markStartFailedCalls[0].reason, "send plan failed") {
		t.Errorf("MarkStartFailed reason missing context, got %q", h.chats.markStartFailedCalls[0].reason)
	}
}

// TestStartTmuxChat_AlreadyExists_LiveTmux verifies daemon-restart re-entry:
// when an existing chat row's tmux session is still alive, StartTmuxChat
// returns AlreadyExists with the original agent_session_id returned in the
// success-shaped string slot alongside the typed error. No new row, no new
// tmux session.
func TestStartTmuxChat_AlreadyExists_LiveTmux(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	// Pre-populate a chat row whose tmux name the fake will report alive.
	existingTmuxName := "boss-repo-abc12345"
	existingAgentSessionID := "agent-existing-12345678"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-existing",
			SessionID:       "sess-1",
			AgentSessionID:  existingAgentSessionID,
			TmuxSessionName: &existingTmuxName,
		}},
	}
	// fakeTmux's HasSession is implemented via the factory: tmux has-session
	// returns 0 (success) when the subcommand is allowed. Default factory
	// returns "true" so HasSession returns true.

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected AlreadyExists when a live tmux chat is present")
	}
	if got := grpcstatus.Code(err); got != codes.AlreadyExists {
		t.Errorf("error code = %s, want AlreadyExists", got)
	}
	if agentSessionID != existingAgentSessionID {
		t.Errorf("agentSessionID = %q, want existing %q", agentSessionID, existingAgentSessionID)
	}

	// No new row, no new tmux session.
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls on AlreadyExists, got %d", len(h.chats.createCalls))
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Error("did not expect new tmux new-session when an alive row exists")
	}
	// The original row must NOT have been deleted.
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected 0 deletes on AlreadyExists, got %v", h.chats.deletedAgentSessionIDs)
	}
}

func TestStartTmuxChat_AllowSiblingChatBypassesLiveChatIdempotency(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	existingTmuxName := "boss-repo-abc12345"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-existing",
			SessionID:       "sess-1",
			AgentSessionID:  "agent-existing-12345678",
			TmuxSessionName: &existingTmuxName,
		}},
	}

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-finalize"}, "Finalize", HookOpts{
		AllowSiblingChat: true,
	})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if agentSessionID == "" || agentSessionID == "agent-existing-12345678" {
		t.Fatalf("agentSessionID = %q, want new sibling chat ID", agentSessionID)
	}
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 sibling Create call, got %d", len(h.chats.createCalls))
	}
	if h.chats.createCalls[0].Title != "Finalize" {
		t.Fatalf("sibling title = %q, want Finalize", h.chats.createCalls[0].Title)
	}
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Fatal("expected new tmux session for sibling chat")
	}
}

// TestStartTmuxChat_StaleTmux_PreservesRowAndStartsFresh verifies that an
// existing chat row whose tmux session has already exited is preserved as
// a historical record (its tmux_session_name is cleared so it no longer
// counts toward idempotency), while a fresh launch proceeds in parallel.
//
// Regression test for the repair-chat-visibility bug: previously the
// idempotency check deleted stale rows, which silently wiped historical
// repair chats every time the repair sweeper revisited a session. Now a
// stale row stays in the chat list (visible as a "stopped" historical
// chat) and only its tmux pointer is unlinked.
func TestStartTmuxChat_StaleTmux_PreservesRowAndStartsFresh(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	staleTmuxName := "boss-repo-stale123"
	staleAgentSessionID := "agent-stale-87654321"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-stale",
			SessionID:       "sess-1",
			AgentSessionID:  staleAgentSessionID,
			TmuxSessionName: &staleTmuxName,
		}},
	}
	// Make tmux report has-session=false for the stale name so the row is
	// classified as a completed historical run.
	h.tmuxFake.failSubcommand["has-session"] = true

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat after stale row: %v", err)
	}
	if agentSessionID == "" {
		t.Fatal("expected fresh agentSessionID alongside the preserved stale row")
	}
	if agentSessionID == staleAgentSessionID {
		t.Errorf("expected fresh agentSessionID, got the stale one (%q)", staleAgentSessionID)
	}

	// The stale row must NOT be deleted — it stays as a historical record
	// in the chat list. Deleting was the original bug.
	if slices.Contains(h.chats.deletedAgentSessionIDs, staleAgentSessionID) {
		t.Errorf("expected stale row %q to be preserved, but it was deleted; deletes=%v",
			staleAgentSessionID, h.chats.deletedAgentSessionIDs)
	}

	// Instead, the stale row's tmux_session_name must have been cleared
	// (set to nil) so it no longer interferes with future idempotency
	// checks for this session.
	var clearedStale bool
	for _, upd := range h.chats.tmuxNameUpdates {
		if upd.agentSessionID == staleAgentSessionID && upd.name == nil {
			clearedStale = true
			break
		}
	}
	if !clearedStale {
		t.Errorf("expected UpdateTmuxSessionName(%q, nil) to clear the stale row, got updates=%+v",
			staleAgentSessionID, h.chats.tmuxNameUpdates)
	}

	// Fresh launch still produces a new row + tmux session in parallel.
	if len(h.chats.createCalls) != 1 {
		t.Errorf("expected 1 Create call alongside the preserved stale row, got %d", len(h.chats.createCalls))
	}
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Error("expected fresh tmux new-session alongside the preserved stale row")
	}
}

func TestReclaimRepairChat_KillsTmuxAndMarksRow(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "repair-agent-1"
	tmuxName := "boss-test-repair"
	reason := "reclaimed stale repair chat after daemon restart"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-repair-1",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Repair: stale rejected PR",
			TmuxSessionName: &tmuxName,
		}},
	}
	writeRepairChatLogAt(t, h, agentSessionID, time.Now().Add(-(repairChatReclaimIdleThreshold + time.Minute)))

	res, err := h.lc.ReclaimRepairChat(ctx, "sess-1", agentSessionID, reason)
	if err != nil {
		t.Fatalf("ReclaimRepairChat: %v", err)
	}
	if !res.Reclaimed {
		t.Fatal("expected Reclaimed=true")
	}
	if res.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %q, want %q", res.TmuxSessionName, tmuxName)
	}

	call := h.findCall("kill-session")
	if call == nil {
		t.Fatal("expected tmux kill-session")
	}
	if got, want := call.args, []string{"-t", tmuxName}; !slices.Equal(got, want) {
		t.Fatalf("kill-session args = %q, want %q", got, want)
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName != nil {
		t.Fatalf("TmuxSessionName = %q, want nil", *chat.TmuxSessionName)
	}
	if chat.StartError == nil || *chat.StartError != reason {
		t.Fatalf("StartError = %v, want %q", chat.StartError, reason)
	}
}

func TestReplaceBlockingChatForRepair_KillsTmuxAndMarksRow(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "agent-blocking-finalize"
	tmuxName := "boss-sess-1-finalize"
	reason := "auto-repair replacing idle chat after plugin idle gate"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-blocking-finalize",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "$boss-finalize",
			TmuxSessionName: &tmuxName,
		}},
	}

	res, err := h.lc.ReplaceBlockingChatForRepair(ctx, "sess-1", agentSessionID, reason)
	if err != nil {
		t.Fatalf("ReplaceBlockingChatForRepair: %v", err)
	}
	if !res.Reclaimed {
		t.Fatal("expected Reclaimed=true")
	}
	if res.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %q, want %q", res.TmuxSessionName, tmuxName)
	}

	call := h.findCall("kill-session")
	if call == nil {
		t.Fatal("expected tmux kill-session")
	}
	if got, want := call.args, []string{"-t", tmuxName}; !slices.Equal(got, want) {
		t.Fatalf("kill-session args = %q, want %q", got, want)
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName != nil {
		t.Fatalf("TmuxSessionName = %q, want nil", *chat.TmuxSessionName)
	}
	if chat.StartError == nil {
		t.Fatal("StartError = nil, want replacement reason")
	}
	if !strings.Contains(*chat.StartError, "auto-repair replacing idle chat") {
		t.Fatalf("StartError = %q, want replacement reason", *chat.StartError)
	}
}

func TestReplaceBlockingChatForRepair_RefusesLiveRecentRepairChat(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "repair-replace-recent-agent"
	tmuxName := "boss-repair-replace-recent"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-repair-replace-recent",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Repair: recent repair",
			TmuxSessionName: &tmuxName,
		}},
	}
	writeRepairChatLogAt(t, h, agentSessionID, time.Now())

	_, err := h.lc.ReplaceBlockingChatForRepair(ctx, "sess-1", agentSessionID, "auto-repair replacing idle chat")
	if !errors.Is(err, ErrRepairChatActive) {
		t.Fatalf("ReplaceBlockingChatForRepair error = %v, want ErrRepairChatActive", err)
	}
	if h.findCall("kill-session") != nil {
		t.Fatalf("tmux session %q was killed without durable stale evidence", tmuxName)
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %v, want %q", chat.TmuxSessionName, tmuxName)
	}
	if chat.StartError != nil {
		t.Fatalf("StartError = %q, want nil", *chat.StartError)
	}
}

func TestReplaceBlockingChatForRepair_RejectsDifferentSession(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "agent-blocking-finalize"
	tmuxName := "boss-other-session-finalize"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"other-session": {{
			ID:              "chat-blocking-finalize",
			SessionID:       "other-session",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "$boss-finalize",
			TmuxSessionName: &tmuxName,
		}},
	}

	_, err := h.lc.ReplaceBlockingChatForRepair(ctx, "sess-1", agentSessionID, "auto-repair replacing idle chat")
	if !errors.Is(err, ErrRepairChatSessionMismatch) {
		t.Fatalf("error = %v, want ErrRepairChatSessionMismatch", err)
	}
	if h.findCall("kill-session") != nil {
		t.Fatalf("tmux session %q was killed for a different session", tmuxName)
	}
	if len(h.chats.markStartFailedCalls) != 0 {
		t.Fatalf("MarkStartFailed calls = %d, want 0", len(h.chats.markStartFailedCalls))
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %v, want %q", chat.TmuxSessionName, tmuxName)
	}
	if chat.StartError != nil {
		t.Fatalf("StartError = %q, want nil", *chat.StartError)
	}
}

func TestReclaimRepairChat_RefusesLiveRecentRepairChat(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "repair-recent-agent"
	tmuxName := "boss-repair-recent"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-repair-recent",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Repair: recent repair",
			TmuxSessionName: &tmuxName,
		}},
	}
	writeRepairChatLogAt(t, h, agentSessionID, time.Now())

	_, err := h.lc.ReclaimRepairChat(ctx, "sess-1", agentSessionID, "must not kill active repair")
	if !errors.Is(err, ErrRepairChatActive) {
		t.Fatalf("error = %v, want ErrRepairChatActive", err)
	}
	if h.findCall("kill-session") != nil {
		t.Fatalf("tmux session %q was killed for a recent repair", tmuxName)
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %v, want %q", chat.TmuxSessionName, tmuxName)
	}
	if chat.StartError != nil {
		t.Fatalf("StartError = %q, want nil for non-reclaimed active repair", *chat.StartError)
	}
}

func TestReclaimRepairChat_RefusesLiveRepairWhenLogMissing(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "repair-missing-log-agent"
	tmuxName := "boss-repair-missing-log"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-repair-missing-log",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Repair: missing log",
			TmuxSessionName: &tmuxName,
		}},
	}

	_, err := h.lc.ReclaimRepairChat(ctx, "sess-1", agentSessionID, "missing log must fail closed")
	if !errors.Is(err, ErrRepairChatActive) {
		t.Fatalf("error = %v, want ErrRepairChatActive", err)
	}
	if h.findCall("kill-session") != nil {
		t.Fatalf("tmux session %q was killed without durable stale evidence", tmuxName)
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %v, want %q", chat.TmuxSessionName, tmuxName)
	}
	if chat.StartError != nil {
		t.Fatalf("StartError = %q, want nil for non-reclaimed active repair", *chat.StartError)
	}
}

func TestReclaimRepairChat_RefusesWhenTmuxLivenessErrors(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "repair-agent-tmux-error"
	tmuxName := "boss-test-tmux-error"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-repair-tmux-error",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Repair: tmux unavailable",
			TmuxSessionName: &tmuxName,
		}},
	}
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "error connecting to /tmp/tmux/default"

	_, err := h.lc.ReclaimRepairChat(ctx, "sess-1", agentSessionID, "must fail closed")
	if !errors.Is(err, ErrRepairChatActive) {
		t.Fatalf("ReclaimRepairChat error = %v, want ErrRepairChatActive", err)
	}
	if h.findCall("kill-session") != nil {
		t.Fatal("did not expect kill-session when tmux liveness cannot be verified")
	}
	if len(h.chats.markStartFailedCalls) != 0 {
		t.Fatalf("MarkStartFailed calls = %d, want 0", len(h.chats.markStartFailedCalls))
	}
}

func TestReclaimRepairChat_RefusesNonRepairChat(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "manual-agent-1"
	tmuxName := "boss-test-manual"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-manual-1",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Manual debugging chat",
			TmuxSessionName: &tmuxName,
		}},
	}

	res, err := h.lc.ReclaimRepairChat(ctx, "sess-1", agentSessionID, "must not reclaim manual chat")
	if !errors.Is(err, ErrRepairChatNotReclaimable) {
		t.Fatalf("error = %v, want ErrRepairChatNotReclaimable", err)
	}
	if res.Reclaimed {
		t.Fatal("expected Reclaimed=false")
	}
	if h.tmuxFake.hasSubcommand("kill-session") {
		t.Fatal("did not expect tmux kill-session for non-repair chat")
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %v, want %q", chat.TmuxSessionName, tmuxName)
	}
	if chat.StartError != nil {
		t.Fatalf("StartError = %q, want nil", *chat.StartError)
	}
}

func TestReclaimRepairChat_ClearsDeadTmuxReference(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	agentSessionID := "repair-agent-dead"
	tmuxName := "boss-test-dead"
	reason := "cleared stale repair chat reference"

	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-repair-dead",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			AgentName:       "codex",
			Title:           "Repair: already dead",
			TmuxSessionName: &tmuxName,
		}},
	}
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session: boss-test-dead"

	res, err := h.lc.ReclaimRepairChat(ctx, "sess-1", agentSessionID, reason)
	if err != nil {
		t.Fatalf("ReclaimRepairChat: %v", err)
	}
	if !res.Reclaimed {
		t.Fatal("expected Reclaimed=true")
	}
	if res.TmuxSessionName != tmuxName {
		t.Fatalf("TmuxSessionName = %q, want %q", res.TmuxSessionName, tmuxName)
	}
	if h.findCall("kill-session") != nil {
		t.Fatal("did not expect kill-session for already-dead tmux reference")
	}

	chat, err := h.chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName != nil {
		t.Fatalf("TmuxSessionName = %q, want nil", *chat.TmuxSessionName)
	}
	if chat.StartError == nil || *chat.StartError != reason {
		t.Fatalf("StartError = %v, want %q", chat.StartError, reason)
	}
}

// TestStartTmuxChat_MissingAgentLogsDir verifies the fail-closed setter:
// an unconfigured agentLogsDir returns FailedPrecondition before any
// side effects.
func TestStartTmuxChat_MissingAgentLogsDir(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetAgentLogsDir("") // explicitly clear

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when agentLogsDir is unset")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Error("did not expect tmux new-session when agentLogsDir unset")
	}
}

// TestStartTmuxChat_NoWorktreePath verifies that a session with an empty
// worktree path can't host a tmux chat — fail-closed FailedPrecondition.
func TestStartTmuxChat_NoWorktreePath(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].WorktreePath = ""

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when session has no worktree path")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
}

// TestStartCronTmuxChat_WrapperPropagatesPlanAndCronTitle pins the wrapper
// contract: the cron entry point (startCronTmuxChat) must continue to call
// StartTmuxChat with prompt=session.Plan and title=`Run "<cron name>"`,
// regardless of how the underlying method evolves.
func TestStartCronTmuxChat_WrapperPropagatesPlanAndCronTitle(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	h.sessions.sessions["sess-1"].Plan = "Run the audit"
	h.sessions.sessions["sess-1"].Title = "Nightly audit"

	_, err := h.lc.startCronTmuxChat(ctx, "sess-1", StartSessionOpts{}, h.sessions.sessions["sess-1"], nil)
	if err != nil {
		t.Fatalf("startCronTmuxChat: %v", err)
	}

	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(h.chats.createCalls))
	}
	if got, want := h.chats.createCalls[0].Title, `Run "Nightly audit"`; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}

	// load-buffer carries the plan content into tmux. Verify by reading
	// its stdin (the fake records args, but plan goes via stdin); we settle
	// for confirming load-buffer + paste-buffer + send-keys all ran.
	for _, sub := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if !h.tmuxFake.hasSubcommand(sub) {
			t.Errorf("expected tmux %s call (SendPlan), none recorded", sub)
		}
	}
}

func TestStartCronTmuxChat_CommandAvoidsBracketedPaste(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.CommandPrefix = "$"

	h.sessions.sessions["sess-1"].Plan = "/bs-mutation-test"
	h.sessions.sessions["sess-1"].Title = "Nightly mutation test"

	_, err := h.lc.startCronTmuxChat(ctx, "sess-1", StartSessionOpts{}, h.sessions.sessions["sess-1"], nil)
	if err != nil {
		t.Fatalf("startCronTmuxChat: %v", err)
	}

	if h.tmuxFake.hasSubcommand("load-buffer") || h.tmuxFake.hasSubcommand("paste-buffer") {
		t.Fatal("cron command input should use literal send-keys, not bracketed paste")
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetInitialCommand(); got != "/bs-mutation-test" {
		t.Fatalf("InitialCommand = %q, want /bs-mutation-test", got)
	}

	var sendKeys []recordedTmuxCall
	h.tmuxFake.mu.Lock()
	for _, call := range h.tmuxFake.calls {
		if call.subcommand == "send-keys" {
			sendKeys = append(sendKeys, call)
		}
	}
	h.tmuxFake.mu.Unlock()

	if len(sendKeys) < 2 {
		t.Fatalf("expected literal text + Enter send-keys calls, got %d", len(sendKeys))
	}
	tmuxName := tmux.ChatSessionName("repo-abcdef12", h.chats.createCalls[0].AgentSessionID)
	textCall := sendKeys[len(sendKeys)-2]
	if !slices.Equal(textCall.args, []string{"-t", tmuxName, "-l", "$bs-mutation-test"}) {
		t.Fatalf("literal send-keys args = %v", textCall.args)
	}
	enterCall := sendKeys[len(sendKeys)-1]
	if !slices.Equal(enterCall.args, []string{"-t", tmuxName, "Enter"}) {
		t.Fatalf("Enter send-keys args = %v", enterCall.args)
	}
}

func TestStartTmuxChat_CommandUsesLiteralKeys(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.CommandPrefix = "$"

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair"}, "Repair: Some session", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}

	if h.tmuxFake.hasSubcommand("load-buffer") || h.tmuxFake.hasSubcommand("paste-buffer") {
		t.Fatal("command input should use literal send-keys, not bracketed paste")
	}

	var sendKeys []recordedTmuxCall
	h.tmuxFake.mu.Lock()
	for _, call := range h.tmuxFake.calls {
		if call.subcommand == "send-keys" {
			sendKeys = append(sendKeys, call)
		}
	}
	h.tmuxFake.mu.Unlock()

	if len(sendKeys) < 2 {
		t.Fatalf("expected literal text + Enter send-keys calls, got %d", len(sendKeys))
	}
	tmuxName := tmux.ChatSessionName("repo-abcdef12", h.chats.createCalls[0].AgentSessionID)
	textCall := sendKeys[len(sendKeys)-2]
	if !slices.Equal(textCall.args, []string{"-t", tmuxName, "-l", "$boss-repair"}) {
		t.Fatalf("literal send-keys args = %v", textCall.args)
	}
	enterCall := sendKeys[len(sendKeys)-1]
	if !slices.Equal(enterCall.args, []string{"-t", tmuxName, "Enter"}) {
		t.Fatalf("Enter send-keys args = %v", enterCall.args)
	}
	if h.agentFake.LastBuildInteractiveCommand.GetInitialCommand() != "boss-repair" {
		t.Fatalf("InitialCommand = %q, want boss-repair", h.agentFake.LastBuildInteractiveCommand.GetInitialCommand())
	}
}

func TestStartTmuxChat_ConsumedStartupInputSkipsPaneInjection(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.ConsumesInitialInput = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair"}, "Repair: Some session", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}

	newSess := h.findCall("new-session")
	if newSess == nil {
		t.Fatal("expected tmux new-session call")
	}
	if joined := strings.Join(newSess.args, " "); !strings.Contains(joined, "$boss-repair") {
		t.Fatalf("new-session argv should embed startup command, got: %s", joined)
	}
	if h.tmuxFake.hasSubcommand("load-buffer") || h.tmuxFake.hasSubcommand("paste-buffer") || h.tmuxFake.hasSubcommand("send-keys") {
		t.Fatalf("consumed startup input should not inject pane input; calls=%v", h.tmuxFake.calls)
	}
	if h.agentFake.LastBuildInteractiveCommand.GetInitialCommand() != "boss-repair" {
		t.Fatalf("InitialCommand = %q, want boss-repair", h.agentFake.LastBuildInteractiveCommand.GetInitialCommand())
	}
}

func TestStartTmuxChat_ConfiguresHookBeforeLaunchForConsumedStartupInput(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.ConsumesInitialInput = true

	configuredBeforeLaunch := false
	h.agentFake.OnConfigureHook = func() {
		configuredBeforeLaunch = !h.tmuxFake.hasSubcommand("new-session")
	}

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair"}, "Repair: Some session", HookOpts{Token: "tok"})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if h.agentFake.LastConfigureHookReq == nil {
		t.Fatal("expected ConfigureFinalizeHook request")
	}
	if !configuredBeforeLaunch {
		t.Fatal("ConfigureFinalizeHook should run before tmux new-session when startup input is embedded in argv")
	}
	if h.tmuxFake.hasSubcommand("load-buffer") || h.tmuxFake.hasSubcommand("paste-buffer") || h.tmuxFake.hasSubcommand("send-keys") {
		t.Fatalf("consumed startup input should not inject pane input; calls=%v", h.tmuxFake.calls)
	}
}

func TestStartTmuxChat_HooklessCommandDoesNotArmPollFallback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.IsSupported = false

	armer := &fakePollArmer{}
	h.lc.SetPollArmer(armer)
	h.lc.SetDaemonCtx(ctx)

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair"}, "Repair: Some session", HookOpts{
		Token: "repair-run-token",
	})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if agentSessionID == "" {
		t.Fatal("expected agent session id")
	}
	if armer.armCalled {
		t.Fatalf("poll fallback armed for tmux-hosted hookless command %q", agentSessionID)
	}
}

// TestStartTmuxChat_HookOptsToken_ConfiguresRunKeyedHook verifies that a
// non-empty HookOpts.Token causes StartTmuxChat to call
// ConfigureFinalizeHook with the agent_session_id, the supplied token, and
// the lifecycle's recorded hook port. This is the run-keyed hook the
// repair plugin's StartChatRun relies on for its WaitChatRun signal.
func TestStartTmuxChat_HookOptsToken_ConfiguresRunKeyedHook(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)

	const tok = "tok-run-12345"
	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{Token: tok})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}

	// ConfigureFinalizeHook was called with run-keyed args.
	got := h.agentFake.LastConfigureHookReq
	if got == nil {
		t.Fatal("expected ConfigureFinalizeHook to be called when HookOpts.Token is non-empty")
	}
	if got.GetAgentSessionId() != agentSessionID {
		t.Errorf("AgentSessionId = %q, want %q", got.GetAgentSessionId(), agentSessionID)
	}
	if got.GetHookToken() != tok {
		t.Errorf("HookToken = %q, want %q", got.GetHookToken(), tok)
	}
	if got.GetHookPort() != 54321 {
		t.Errorf("HookPort = %d, want 54321", got.GetHookPort())
	}
	if got.GetSessionId() != "sess-1" {
		t.Errorf("SessionId = %q, want sess-1", got.GetSessionId())
	}
}

// TestStartTmuxChat_HookOptsEmpty_DoesNotConfigureHook verifies the cron
// path's invariant: when HookOpts is zero, ConfigureFinalizeHook is NOT
// called from StartTmuxChat (cron wires its session-keyed hook earlier).
func TestStartTmuxChat_HookOptsEmpty_DoesNotConfigureHook(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if h.agentFake.LastConfigureHookReq != nil {
		t.Errorf("ConfigureFinalizeHook should not be called when HookOpts is empty; got %+v", h.agentFake.LastConfigureHookReq)
	}
}

// TestStartTmuxChat_HookOptsTokenWithoutHookPort_FailsClosed verifies that
// a token without a configured hook port is rejected with FailedPrecondition
// and tears the live tmux session down. The agent_chats row is preserved
// (with a start_error reason) rather than deleted so the chat list still
// shows the attempt.
func TestStartTmuxChat_HookOptsTokenWithoutHookPort_FailsClosed(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Deliberately don't call SetHookPort.

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{Token: "tok"})
	if err == nil {
		t.Fatal("expected error when hook port unset and HookOpts.Token non-empty")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after hook port precondition failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected NO row delete after hook port precondition failure (row preserved as failed-to-start), got %v", h.chats.deletedAgentSessionIDs)
	}
	if len(h.chats.markStartFailedCalls) != 1 {
		t.Fatalf("expected 1 MarkStartFailed call after hook port precondition failure, got %d", len(h.chats.markStartFailedCalls))
	}
	if !strings.Contains(h.chats.markStartFailedCalls[0].reason, "hook port not configured") {
		t.Errorf("MarkStartFailed reason missing context, got %q", h.chats.markStartFailedCalls[0].reason)
	}
}

// TestStartTmuxChat_HookConfigureFails_TearsDown verifies that a
// ConfigureFinalizeHook RPC failure tears tmux down. The agent_chats
// row is preserved (with a start_error reason) — mirrors the SendPlan
// preservation path, so all StartTmuxChat failures surface in the chat
// list rather than vanishing.
func TestStartTmuxChat_HookConfigureFails_TearsDown(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)
	h.agentFake.ConfigureHookErr = fmt.Errorf("simulated hook config failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{Token: "tok"})
	if err == nil {
		t.Fatal("expected error when ConfigureFinalizeHook fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after ConfigureFinalizeHook failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected NO row delete after ConfigureFinalizeHook failure (row preserved as failed-to-start), got %v", h.chats.deletedAgentSessionIDs)
	}
	if len(h.chats.markStartFailedCalls) != 1 {
		t.Fatalf("expected 1 MarkStartFailed call after ConfigureFinalizeHook failure, got %d", len(h.chats.markStartFailedCalls))
	}
	if !strings.Contains(h.chats.markStartFailedCalls[0].reason, "configure finalize hook failed") {
		t.Errorf("MarkStartFailed reason missing context, got %q", h.chats.markStartFailedCalls[0].reason)
	}
	// SendPlan should NOT have run — failure happens before step 9.
	if h.tmuxFake.hasSubcommand("load-buffer") {
		t.Error("did not expect SendPlan (load-buffer) after hook configure failure")
	}
}

// TestStartTmuxChatDoesNotArmPollWhenHookUnsupported verifies hookless
// interactive tmux chats do not poll plugin ExitStatus, which only observes
// plugin-runner processes rather than tmux-spawned processes.
func TestStartTmuxChatDoesNotArmPollWhenHookUnsupported(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.IsSupported = false // hookless agent

	armer := &fakePollArmer{}
	completer := &recordingPollCompleter{}
	h.lc.SetPollArmer(armer)
	h.lc.SetPollCompleter(completer)
	h.lc.SetDaemonCtx(ctx)
	h.lc.tmuxCompletionPollInterval = time.Millisecond

	id, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "the prompt"}, "the title", HookOpts{Token: "tok-2"})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if id == "" {
		t.Fatal("expected agent session id")
	}
	if armer.armCalled {
		t.Errorf("poll fallback armed for tmux-hosted hookless run %q", id)
	}
	h.tmuxFake.mu.Lock()
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session"
	h.tmuxFake.mu.Unlock()

	waitForCount(t, "SignalRunComplete", completer.count)
	calls := completer.callsCopy()
	if calls[0].agentSessionID != id {
		t.Fatalf("SignalRunComplete called for %q, want %q", calls[0].agentSessionID, id)
	}
	if calls[0].exitError != "" {
		t.Fatalf("SignalRunComplete exit error = %q, want empty", calls[0].exitError)
	}
}

// TestStartTmuxChatDoesNotArmPollWhenHookSupported verifies the existing
// claude path does NOT trigger the poll fallback.
func TestStartTmuxChatDoesNotArmPollWhenHookSupported(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	// fakeAgent.IsSupported defaults to true.

	armer := &fakePollArmer{}
	h.lc.SetPollArmer(armer)
	h.lc.SetDaemonCtx(ctx)

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p"}, "T", HookOpts{Token: "tok-2"}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if armer.armCalled {
		t.Error("poll fallback should NOT be armed when hook is supported")
	}
}

// TestStartTmuxChat_ResumeReusesIDAndSetsResume verifies that a resume request
// reuses the supplied agent session id (instead of minting a fresh one) and
// asks the agent plugin to resume rather than start fresh.
func TestStartTmuxChat_ResumeReusesIDAndSetsResume(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: "agent-session-prior"},
		"T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat resume: %v", err)
	}
	if id != "agent-session-prior" {
		t.Fatalf("returned id = %q, want %q (reused prior id)", id, "agent-session-prior")
	}
	if !h.agentFake.LastBuildInteractiveCommand.GetResume() {
		t.Error("expected BuildInteractiveCommand Resume=true on resume")
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetSessionId(); got != "agent-session-prior" {
		t.Errorf("BuildInteractiveCommand SessionId = %q, want %q", got, "agent-session-prior")
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetWorktreePath(); got != h.sessions.sessions["sess-1"].WorktreePath {
		t.Errorf("BuildInteractiveCommand WorktreePath = %q, want %q", got, h.sessions.sessions["sess-1"].WorktreePath)
	}
}

// TestStartTmuxChat_ResumeDeletesPriorRowNoDuplicate verifies that resuming
// deletes the stale prior chat row (whose agent_session_id is non-unique)
// before re-creating, so exactly one row carries the reused id.
func TestStartTmuxChat_ResumeDeletesPriorRowNoDuplicate(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	staleTmuxName := "boss-repo-prior123"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-prior",
			SessionID:       "sess-1",
			AgentSessionID:  "agent-session-prior",
			TmuxSessionName: &staleTmuxName,
		}},
	}
	// Force the prior tmux session to read as dead so idempotency clears it
	// and the launch proceeds.
	h.tmuxFake.failSubcommand["has-session"] = true

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: "agent-session-prior"},
		"T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat resume: %v", err)
	}
	if id != "agent-session-prior" {
		t.Fatalf("returned id = %q, want %q (reused prior id)", id, "agent-session-prior")
	}
	if !slices.Contains(h.chats.deletedAgentSessionIDs, "agent-session-prior") {
		t.Errorf("expected stale row %q to be deleted before re-create, deletes=%v",
			"agent-session-prior", h.chats.deletedAgentSessionIDs)
	}
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected exactly 1 Create call (no duplicate), got %d", len(h.chats.createCalls))
	}
	if got := h.chats.createCalls[0].AgentSessionID; got != "agent-session-prior" {
		t.Errorf("Create AgentSessionID = %q, want %q (reused, no duplicate)", got, "agent-session-prior")
	}
}

// TestStartTmuxChat_ResumeReusesLivePane verifies that a completed repair pane
// that is still alive at the prompt is reused instead of being rejected by the
// live-chat idempotency guard.
func TestStartTmuxChat_ResumeReusesLivePane(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.ConsumesInitialInput = true

	const agentSessionID = "agent-session-prior"
	tmuxName := tmux.ChatSessionName("repo-abcdef12", agentSessionID)
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-prior",
			SessionID:       "sess-1",
			AgentSessionID:  agentSessionID,
			TmuxSessionName: &tmuxName,
		}},
	}

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: agentSessionID},
		"T", HookOpts{Token: "tok-resume"})
	if err != nil {
		t.Fatalf("StartTmuxChat resume into live pane: %v", err)
	}
	if id != agentSessionID {
		t.Fatalf("returned id = %q, want %q", id, agentSessionID)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Fatalf("resume into live pane must not create a replacement tmux session; calls=%v", h.tmuxFake.calls)
	}
	if len(h.chats.createCalls) != 0 {
		t.Fatalf("resume into live pane must not create duplicate chat rows, got %d", len(h.chats.createCalls))
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Fatalf("resume into live pane must not delete the active chat row, deletes=%v", h.chats.deletedAgentSessionIDs)
	}
	if !h.agentFake.LastBuildInteractiveCommand.GetResume() {
		t.Error("expected BuildInteractiveCommand Resume=true on live-pane resume")
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetSessionId(); got != agentSessionID {
		t.Errorf("BuildInteractiveCommand SessionId = %q, want %q", got, agentSessionID)
	}
	if got := h.agentFake.LastConfigureHookReq; got == nil {
		t.Fatal("expected run-keyed hook to be configured for live-pane resume")
	} else if got.GetAgentSessionId() != agentSessionID || got.GetHookToken() != "tok-resume" {
		t.Fatalf("ConfigureFinalizeHook = %+v, want agent_session_id %q and token tok-resume", got, agentSessionID)
	}

	var sendKeys []recordedTmuxCall
	h.tmuxFake.mu.Lock()
	for _, call := range h.tmuxFake.calls {
		if call.subcommand == "send-keys" {
			sendKeys = append(sendKeys, call)
		}
	}
	h.tmuxFake.mu.Unlock()

	if len(sendKeys) < 2 {
		t.Fatalf("expected literal text + Enter send-keys calls, got %d", len(sendKeys))
	}
	textCall := sendKeys[len(sendKeys)-2]
	if !slices.Equal(textCall.args, []string{"-t", tmuxName, "-l", "$boss-repair"}) {
		t.Fatalf("literal send-keys args = %v", textCall.args)
	}
	enterCall := sendKeys[len(sendKeys)-1]
	if !slices.Equal(enterCall.args, []string{"-t", tmuxName, "Enter"}) {
		t.Fatalf("Enter send-keys args = %v", enterCall.args)
	}
}

func TestStartTmuxChat_ResumeReusesLivePaneByProviderSessionID(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.CommandPrefix = "$"

	const logicalAgentSessionID = "agent-session-logical"
	const providerSessionID = "codex-rollout-provider"
	tmuxName := tmux.ChatSessionName("repo-abcdef12", logicalAgentSessionID)
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:                "chat-codex",
			SessionID:         "sess-1",
			AgentSessionID:    logicalAgentSessionID,
			ProviderSessionID: ptr(providerSessionID),
			TmuxSessionName:   &tmuxName,
		}},
	}

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: providerSessionID},
		"T", HookOpts{Token: "tok-codex"})
	if err != nil {
		t.Fatalf("StartTmuxChat resume into Codex live pane: %v", err)
	}
	if id != logicalAgentSessionID {
		t.Fatalf("returned id = %q, want logical id %q for WaitChatRun registration", id, logicalAgentSessionID)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Fatalf("provider-id resume into live pane must not create a replacement tmux session; calls=%v", h.tmuxFake.calls)
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetSessionId(); got != providerSessionID {
		t.Errorf("BuildInteractiveCommand SessionId = %q, want provider id %q", got, providerSessionID)
	}
	if !h.agentFake.LastBuildInteractiveCommand.GetResume() {
		t.Error("expected BuildInteractiveCommand Resume=true on provider-id live-pane resume")
	}
	if got := h.agentFake.LastConfigureHookReq; got == nil {
		t.Fatal("expected run-keyed hook to be configured for provider-id live-pane resume")
	} else if got.GetAgentSessionId() != logicalAgentSessionID {
		t.Fatalf("ConfigureFinalizeHook AgentSessionId = %q, want logical id %q", got.GetAgentSessionId(), logicalAgentSessionID)
	}
	if len(h.chats.createCalls) != 0 {
		t.Fatalf("provider-id live-pane resume must not create duplicate chat rows, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_ResumeStillArmsCompletion is the #491 regression guard: a
// resumed, hookless run must still arm completion using the reused id so a
// later WaitChatRun resolves when tmux reports the pane dead.
func TestStartTmuxChat_ResumeStillArmsCompletion(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.IsSupported = false // hookless agent

	armer := &fakePollArmer{}
	completer := &recordingPollCompleter{}
	h.lc.SetPollArmer(armer)
	h.lc.SetPollCompleter(completer)
	h.lc.SetDaemonCtx(ctx)
	h.lc.tmuxCompletionPollInterval = time.Millisecond

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Prompt: "p", ResumeAgentSessionID: "agent-session-prior"},
		"the title", HookOpts{Token: "tok-2"})
	if err != nil {
		t.Fatalf("StartTmuxChat resume: %v", err)
	}
	if id != "agent-session-prior" {
		t.Fatalf("returned id = %q, want %q (reused prior id)", id, "agent-session-prior")
	}
	if armer.armCalled {
		t.Errorf("poll fallback armed for tmux-hosted hookless run %q", id)
	}

	h.tmuxFake.mu.Lock()
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session"
	h.tmuxFake.mu.Unlock()

	waitForCount(t, "SignalRunComplete", completer.count)
	calls := completer.callsCopy()
	if calls[0].agentSessionID != "agent-session-prior" {
		t.Fatalf("SignalRunComplete called for %q, want %q (reused id)", calls[0].agentSessionID, "agent-session-prior")
	}
}

// TestStartTmuxChat_ResumeDeleteErrorTearsDown verifies that a
// DeleteByAgentSessionID failure on the resume branch tears the just-spawned
// tmux session back down and returns an error, and that no Create call is made
// (the delete failed before we reached Create).
func TestStartTmuxChat_ResumeDeleteErrorTearsDown(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.deleteErr = errors.New("boom")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Prompt: "p", ResumeAgentSessionID: "agent-session-prior"},
		"T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when DeleteByAgentSessionID fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session to clean up after delete failure")
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls (delete failed before Create), got %d", len(h.chats.createCalls))
	}
}

// emptyArgvAgent is an AgentRunnerClient whose BuildInteractiveCommand
// returns no argv at all — used to drive the empty-argv fail-closed path.
type emptyArgvAgent struct {
	fakeAgentForLifecycle
}

func (a *emptyArgvAgent) BuildInteractiveCommand(_ context.Context, _ *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}

// Compile-time assertion that emptyArgvAgent still satisfies the interface
// — guards against the embedded fakeAgentForLifecycle's signature drifting.
var _ agent.AgentRunnerClient = (*emptyArgvAgent)(nil)
