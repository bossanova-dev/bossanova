package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// Tests for Lifecycle.StartTmuxChat — the generalized form of the cron-only
// helper that previously lived in startTmuxChat. Cron-specific behavior
// stays in lifecycle_test.go (the cron test cluster around
// TestStartSession_CronJobID_*); this file targets the generic method
// directly so any future caller (repair, interactive UI button) gets the
// same coverage.

func TestChatInputRenderCommandUsesAgentPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	input := ChatInput{Command: "boss-repair"}
	if got := input.render("$"); got != "$boss-repair" {
		t.Fatalf("rendered command = %q, want $boss-repair", got)
	}
}

func TestChatInputRenderCommandDefaultsToSlashPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	input := ChatInput{Command: "boss-repair"}
	if got := input.render(""); got != "/boss-repair" {
		t.Fatalf("rendered command = %q, want /boss-repair", got)
	}
}

func TestChatInputRenderPromptPreservesRawText(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	input := ChatInput{Prompt: "/boss-repair"}
	if got := input.render("$"); got != "/boss-repair" {
		t.Fatalf("rendered prompt = %q, want /boss-repair", got)
	}
}

func TestRenderBossCommandPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	writeTestSkill(t, filepath.Join(home, ".codex", "skills", "bossanova", "boss-repair"))
	writeTestSkill(t, filepath.Join(home, ".codex", "skills", "bossanova", "bs-sweep-mutation"))
	writeTestSkill(t, filepath.Join(home, ".codex", "skills", "bs-sweep-debt"))
	writeTestSkill(t, filepath.Join(home, ".claude", "skills", "bossanova", "boss-repair"))
	writeTestSkill(t, filepath.Join(worktree, ".codex", "skills", "api-review"))
	writeTestSkill(t, filepath.Join(worktree, ".codex", "skills", "tui-qa"))

	cases := []struct {
		name     string
		message  string
		prefix   string
		worktree string
		want     string
	}{
		{"codex boss command gets dollar prefix", "/boss-repair watch", "$", worktree, "$boss-repair watch"},
		{"claude boss command keeps slash prefix", "/boss-repair watch", "/", worktree, "/boss-repair watch"},
		{"empty prefix defaults to slash", "/boss-repair watch", "", worktree, "/boss-repair watch"},
		{"dollar boss command normalized to agent prefix", "$boss-repair watch", "/", worktree, "/boss-repair watch"},
		{"surrounding whitespace trimmed for boss command", "  /boss-repair watch  ", "$", worktree, "$boss-repair watch"},
		// Non-boss custom skills are custom commands too and must be rewritten.
		{"codex project custom command gets dollar prefix", "/api-review services/bossd", "$", worktree, "$api-review services/bossd"},
		{"codex project tui command gets dollar prefix", "/tui-qa", "$", worktree, "$tui-qa"},
		// bs-* sweep skills are Bossanova commands too and must be rewritten.
		{"codex bs sweep command gets dollar prefix", "/bs-sweep-mutation", "$", worktree, "$bs-sweep-mutation"},
		{"codex bs sweep command with args", "/bs-sweep-debt --dry-run", "$", worktree, "$bs-sweep-debt --dry-run"},
		// Native agent built-ins are NOT custom skills: never rewrite them, or a
		// codex "/status" becomes an invalid "$status".
		{"codex native slash command passes through", "/status", "$", worktree, "/status"},
		{"codex native command with args passes through", "/model gpt-5", "$", worktree, "/model gpt-5"},
		{"claude native slash command passes through", "/status", "/", worktree, "/status"},
		{"non-custom dollar token passes through", "$HOME/path", "$", worktree, "$HOME/path"},
		{"free text passes through unchanged", "please fix the flaky test", "$", worktree, "please fix the flaky test"},
		{"multi-line passes through unchanged", "/boss-repair watch\nsecond line", "$", worktree, "/boss-repair watch\nsecond line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderBossCommandPrefix(tc.message, tc.prefix, tc.worktree); got != tc.want {
				t.Fatalf("RenderBossCommandPrefix(%q, %q, %q) = %q, want %q", tc.message, tc.prefix, tc.worktree, got, tc.want)
			}
		})
	}
}

func writeTestSkill(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: test\n---\n"), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
}

func TestChatInputMechanicsFromPromptConvertsLeadingSlashCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	input := chatInputMechanicsFromPrompt("/bs-sweep-debt")
	if input.Prompt != "" {
		t.Fatalf("Prompt = %q, want empty", input.Prompt)
	}
	if input.Command != "/bs-sweep-debt" {
		t.Fatalf("Command = %q, want /bs-sweep-debt", input.Command)
	}
}

func TestChatInputMechanicsFromPromptConvertsLeadingDollarCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	input := chatInputMechanicsFromPrompt("$bs-sweep-mutation")
	if input.Prompt != "" {
		t.Fatalf("Prompt = %q, want empty", input.Prompt)
	}
	if input.Command != "$bs-sweep-mutation" {
		t.Fatalf("Command = %q, want $bs-sweep-mutation", input.Command)
	}
}

func TestChatInputMechanicsFromPromptTrimsSurroundingWhitespace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	input := chatInputMechanicsFromPrompt("  /bs-sweep-mutation  ")
	if input.Prompt != "" {
		t.Fatalf("Prompt = %q, want empty", input.Prompt)
	}
	if input.Command != "/bs-sweep-mutation" {
		t.Fatalf("Command = %q, want /bs-sweep-mutation", input.Command)
	}
}

// A leading slash/$ command with arguments must still dispatch as a command
// (the whole single line, args included) so it auto-runs via the
// submit-verified send-line path. Routing it to the paste path instead leaves
// the command loaded-but-not-executed in the headless cron's input box — the
// bug that stalled the "/wc-merge-review headless" sweep.
func TestChatInputMechanicsFromPromptConvertsLeadingCommandWithArgs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, tc := range []struct {
		prompt  string
		command string
	}{
		{"/wc-merge-review headless", "/wc-merge-review headless"},
		{"/bs-sweep-mutation a b", "/bs-sweep-mutation a b"},
		{"  /wc-merge-review headless  ", "/wc-merge-review headless"},
		{"$boss-repair now", "$boss-repair now"},
	} {
		input := chatInputMechanicsFromPrompt(tc.prompt)
		if input.Prompt != "" {
			t.Fatalf("prompt %q: Prompt = %q, want empty", tc.prompt, input.Prompt)
		}
		if input.Command != tc.command {
			t.Fatalf("prompt %q: Command = %q, want %q", tc.prompt, input.Command, tc.command)
		}
	}
}

// Embedded commands must NOT be extracted: doing so silently truncates the
// surrounding free-text instruction, which is the user's actual cron plan.
func TestChatInputMechanicsFromPromptKeepsEmbeddedCommandAsPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	prompt := "Run /bs-sweep-mutation"
	input := chatInputMechanicsFromPrompt(prompt)
	if input.Prompt != prompt {
		t.Fatalf("Prompt = %q, want %q", input.Prompt, prompt)
	}
	if input.Command != "" {
		t.Fatalf("Command = %q, want empty", input.Command)
	}
}

// A single-line free-text prompt containing a slash (path/URL) or dollar
// (price) must stay a prompt rather than being truncated into a bogus command.
func TestChatInputMechanicsFromPromptKeepsFreeTextWithSlashOrDollar(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, prompt := range []string{
		"Review the auth changes in /internal/auth",
		"Add a $5 discount banner",
		"Summarize https://example.com/foo",
	} {
		input := chatInputMechanicsFromPrompt(prompt)
		if input.Prompt != prompt {
			t.Fatalf("prompt %q: Prompt = %q, want unchanged", prompt, input.Prompt)
		}
		if input.Command != "" {
			t.Fatalf("prompt %q: Command = %q, want empty", prompt, input.Command)
		}
	}
}

func TestChatInputMechanicsFromPromptKeepsMultilinePrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	prompt := "/bs-sweep-mutation\nwith extra notes"
	input := chatInputMechanicsFromPrompt(prompt)
	if input.Prompt != prompt {
		t.Fatalf("Prompt = %q, want %q", input.Prompt, prompt)
	}
	if input.Command != "" {
		t.Fatalf("Command = %q, want empty", input.Command)
	}
}

func TestChatInputMechanicsFromPromptKeepsEmptyAndWhitespace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, prompt := range []string{"", "   ", "\t"} {
		input := chatInputMechanicsFromPrompt(prompt)
		if input.Command != "" {
			t.Fatalf("prompt %q: Command = %q, want empty", prompt, input.Command)
		}
		if input.Prompt != prompt {
			t.Fatalf("prompt %q: Prompt = %q, want unchanged", prompt, input.Prompt)
		}
	}
}

// chatInputMechanicsFromPrompt selects delivery MECHANICS only (command vs
// paste). It must never set a delivery intent — that is derived from session
// provenance at the call site — so every shape leaves Delivery at the safe
// PrefillOnly zero value.
func TestChatInputMechanicsFromPromptSetsNoDeliveryIntent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, prompt := range []string{
		"/bs-sweep-debt",
		"$bs-sweep-mutation",
		"/wc-merge-review headless",
		"do the thing",
		"Review /internal/auth",
		"/bs-sweep-mutation\nwith notes",
		"line one\nline two",
	} {
		if got := chatInputMechanicsFromPrompt(prompt).Delivery; got != DeliveryPrefillOnly {
			t.Errorf("prompt %q: Delivery = %v, want DeliveryPrefillOnly (mechanics must not set intent)", prompt, got)
		}
	}
}

// TestStartTmuxChat_ProvenanceDrivesSubmitIntent proves the provenance→intent
// derivation in startTmuxChat: an unattended session (cron OR tmux_unattended)
// has no human to press Enter, so its plan is delivered with DeliverySubmit —
// the composer is submitted (a bare-Enter send-keys is recorded). An
// interactive session yields the safe PrefillOnly default and presses no Enter.
func TestStartTmuxChat_ProvenanceDrivesSubmitIntent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	cronID := "cron-1"
	for _, tc := range []struct {
		name       string
		mutate     func(s *models.Session)
		opts       StartSessionOpts
		wantSubmit bool
	}{
		{
			name:       "cron session submits",
			mutate:     func(s *models.Session) { s.CronJobID = &cronID },
			opts:       StartSessionOpts{CronJobID: cronID},
			wantSubmit: true,
		},
		{
			name:       "tmux_unattended session submits",
			mutate:     func(s *models.Session) { s.IsTmuxUnattended = true },
			wantSubmit: true,
		},
		{
			name:       "detach session submits",
			mutate:     func(s *models.Session) {},
			opts:       StartSessionOpts{Detach: true},
			wantSubmit: true,
		},
		{
			name:       "interactive session prefills only",
			mutate:     func(s *models.Session) {},
			wantSubmit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newStartTmuxChatHarness(t)
			h.sessions.sessions["sess-1"].Plan = "do the audit"
			tc.mutate(h.sessions.sessions["sess-1"])

			if _, err := h.lc.startTmuxChat(ctx, "sess-1", tc.opts, h.sessions.sessions["sess-1"], nil); err != nil {
				t.Fatalf("startTmuxChat: %v", err)
			}
			got := h.tmuxFake.enterSendKeysCount()
			if tc.wantSubmit && got == 0 {
				t.Errorf("unattended provenance must submit (press Enter), got %d Enter send-keys", got)
			}
			if !tc.wantSubmit && got != 0 {
				t.Errorf("interactive provenance must prefill only (no Enter), got %d Enter send-keys", got)
			}
		})
	}
}

// TestIsUnattendedSession_Detach pins BOS-428: a durable tmux-hosted detach run
// (Detach=true) joins the unattended class alongside cron and tmux_unattended, so
// the completion gate admits its finalize Stop hook and the restart orphan sweep
// skips it. A plain interactive session (all flags false) is NOT unattended.
func TestIsUnattendedSession_Detach(t *testing.T) {
	cronID := "cron-1"
	for _, tc := range []struct {
		name string
		sess *models.Session
		want bool
	}{
		{"nil session", nil, false},
		{"detach", &models.Session{Detach: true}, true},
		{"tmux_unattended", &models.Session{IsTmuxUnattended: true}, true},
		{"cron", &models.Session{CronJobID: &cronID}, true},
		{"interactive", &models.Session{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnattendedSession(tc.sess); got != tc.want {
				t.Errorf("isUnattendedSession = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInjectTmuxChatInput_DeliveryMatrix drives injectTmuxChatInput across the
// {Submit, PrefillOnly} × {single-line command, single-line free text,
// multi-line} matrix. Submit paths press Enter (and, for the fake, verify a
// clean pane); PrefillOnly paths deliver into the composer but press NO Enter.
// The multi-line Submit case is the exact #1028/#1029 shape that previously
// no-op'd — it must now auto-submit.
func TestInjectTmuxChatInput_DeliveryMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	cmdResp := &bossanovav1.BuildInteractiveCommandResponse{ReadyMarker: "❯", CommandPrefix: "$"}
	for _, tc := range []struct {
		name  string
		input ChatInput
	}{
		{"single-line command", ChatInput{Command: "boss-repair"}},
		{"single-line free text", ChatInput{Prompt: "do the thing"}},
		{"multi-line", ChatInput{Prompt: "line one\nline two"}},
	} {
		for _, submit := range []bool{true, false} {
			label := tc.name
			input := tc.input
			if submit {
				input.Delivery = DeliverySubmit
				label += "/submit"
			} else {
				input.Delivery = DeliveryPrefillOnly
				label += "/prefill"
			}
			t.Run(label, func(t *testing.T) {
				ctx := context.Background()
				h := newStartTmuxChatHarness(t)
				if err := h.lc.injectTmuxChatInput(ctx, "bossd-agent-run-x", input, cmdResp); err != nil {
					t.Fatalf("injectTmuxChatInput: %v", err)
				}
				got := h.tmuxFake.enterSendKeysCount()
				if submit && got == 0 {
					t.Errorf("submit path must press Enter, got %d", got)
				}
				if !submit && got != 0 {
					t.Errorf("prefill path must press NO Enter, got %d", got)
				}
			})
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
	h.lc = newTestLifecycle(h.sessions, h.repos, h.chats, &stubCronJobStore{}, h.wt, h.agentRun, h.tmuxClient, newMockVCSProvider(), zerolog.Nop())
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

func ptr[T any](v T) *T {
	return &v
}

func TestKillChatTmuxSession_KillsLiveTmuxThenClearsChatPointer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
func TestStartTmuxChat_SendsModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].Model = "sonnet"

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetModel(); got != "sonnet" {
		t.Fatalf("BuildInteractiveCommand model = %q, want sonnet", got)
	}
}

// TestStartTmuxChat_CapabilityPreflightFailureIsTyped covers the authoritative
// capability probe, which runs here — after StartSession has already persisted
// the worktree and branch. StartSession keys its rollback off the error type, so
// this probe's rejection must arrive as capabilityPreflightError; otherwise the
// worktree and branch are stranded for the next attempt to collide with. The
// message must survive the wrapper verbatim, since it is what callers log.
func TestStartTmuxChat_CapabilityPreflightFailureIsTyped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentRun.preflightErr = grpcstatus.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if err == nil {
		t.Fatal("StartTmuxChat succeeded, want capability preflight rejection")
	}
	var preflightErr capabilityPreflightError
	if !errors.As(err, &preflightErr) {
		t.Fatalf("StartTmuxChat error = %v, want capabilityPreflightError so StartSession rolls back", err)
	}
	if !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
		t.Fatalf("StartTmuxChat error = %q, want the runner's message preserved verbatim", err)
	}
	if h.agentRun.preflightCalls != 1 {
		t.Fatalf("preflight calls = %d, want 1", h.agentRun.preflightCalls)
	}
	// A rejected launch must not leave a tmux session behind.
	if call := h.findCall("new-session"); call != nil {
		t.Fatal("tmux new-session ran despite a rejected capability preflight")
	}
}

// TestStartTmuxChat_UnspecifiedProfileSkipsPreflight pins the gate: a launch
// that requires no profile must not consult the dispatcher at all, so a runner
// that would reject the probe cannot fail an unprofiled (interactive, or
// non-Codex) launch that never asked for the capability.
func TestStartTmuxChat_UnspecifiedProfileSkipsPreflight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentRun.preflightErr = grpcstatus.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat with an unspecified profile: %v", err)
	}
	if h.agentRun.preflightCalls != 0 {
		t.Fatalf("preflight calls = %d, want 0 for an unspecified profile", h.agentRun.preflightCalls)
	}
}

// TestStartTmuxChat_AutonomousRunProfilesTheSpawningAgent covers the fail-open
// half of the divergence: a codex chat resumed inside a session whose persisted
// agent_name is still claude. A caller keying the profile on sess.AgentName
// reads "claude", sends UNSPECIFIED, and the gate never runs — the codex launch
// proceeds without its capability ever being probed. Declaring AutonomousRun
// instead defers the choice to here, the first point that knows codex is what
// will actually spawn.
//
// ResumeAgentSessionID is load-bearing: without it spawnAgentName never leaves
// sess.AgentName and the case passes vacuously against a fresh claude launch.
func TestStartTmuxChat_AutonomousRunProfilesTheSpawningAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// The session is claude; the chat being resumed is codex.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			SessionID:      "sess-1",
			AgentSessionID: "agent-1",
			AgentName:      "codex",
		}},
	}

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{
		Prompt:               "/boss-repair",
		Delivery:             DeliverySubmit,
		ResumeAgentSessionID: "agent-1",
	}, "title", HookOpts{AutonomousRun: true})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if h.agentRun.preflightCalls != 1 || len(h.agentRun.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each — a codex chat must be gated even inside a claude session",
			h.agentRun.preflightCalls, len(h.agentRun.preflights))
	}
	got := h.agentRun.preflights[0]
	if got.agentName != "codex" {
		t.Errorf("preflight agentName = %q, want codex (the agent that actually spawns)", got.agentName)
	}
	if got.profile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Errorf("preflight profile = %v, want TRACKER_PLAN_ATTACHMENT_V1", got.profile)
	}
}

// TestStartTmuxChat_AutonomousRunSkipsGateForNonCodexChat covers the opposite,
// fail-closed half: a claude chat resumed inside a session whose persisted
// agent_name is codex. A caller keying on sess.AgentName reads "codex" and
// hands claude a profile claude answers Unimplemented for, so the launch is
// refused outright. Keying on the spawning agent leaves the gate off and the
// launch alone.
func TestStartTmuxChat_AutonomousRunSkipsGateForNonCodexChat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// The session is codex; the chat being resumed is claude.
	h.sessions.sessions["sess-1"].AgentName = "codex"
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			SessionID:      "sess-1",
			AgentSessionID: "agent-1",
			AgentName:      "claude",
		}},
	}
	// A runner that rejects every probe: if the gate runs at all, the launch
	// fails, which is exactly the false refusal this change removes.
	h.agentRun.preflightErr = grpcstatus.Error(codes.Unimplemented, "PreflightHeadlessRun is not implemented")

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{
		Prompt:               "/boss-repair",
		Delivery:             DeliverySubmit,
		ResumeAgentSessionID: "agent-1",
	}, "title", HookOpts{AutonomousRun: true}); err != nil {
		t.Fatalf("StartTmuxChat for a claude chat inside a codex session: %v", err)
	}
	if h.agentRun.preflightCalls != 0 {
		t.Fatalf("preflight calls = %d, want 0 — a claude chat must not be gated on the session's codex name", h.agentRun.preflightCalls)
	}
}

// TestStartTmuxChat_ExplicitProfileWinsOverAutonomousRun pins the precedence:
// AutonomousRun only asks the seam to choose, so a caller that already computed
// a profile (SwitchAccount, which keys on the account being switched to and
// cannot publish that provider anywhere the seam could read it) keeps its value.
// Without this, the seam would silently overwrite the one site that knows more
// than it does.
func TestStartTmuxChat_ExplicitProfileWinsOverAutonomousRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// A claude session and a claude chat: the seam alone would choose
	// UNSPECIFIED, so a surviving profile can only be the explicit one.
	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{
		AutonomousRun:             true,
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if h.agentRun.preflightCalls != 1 || len(h.agentRun.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each", h.agentRun.preflightCalls, len(h.agentRun.preflights))
	}
	if got := h.agentRun.preflights[0].profile; got != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("preflight profile = %v, want the caller's explicit TRACKER_PLAN_ATTACHMENT_V1 preserved", got)
	}
}

// TestStartTmuxChat_NonPreflightFailureIsNotTyped scopes the wrapper. Every
// other StartTmuxChat failure keeps its existing no-rollback behavior, so an
// unrelated error must not match capabilityPreflightError — otherwise
// StartSession would archive a worktree over failures that never touched the
// capability probe.
func TestStartTmuxChat_NonPreflightFailureIsNotTyped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	_, err := h.lc.StartTmuxChat(ctx, "sess-missing", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if err == nil {
		t.Fatal("StartTmuxChat succeeded for an unknown session, want an error")
	}
	var preflightErr capabilityPreflightError
	if errors.As(err, &preflightErr) {
		t.Fatalf("StartTmuxChat error = %v, want an untyped failure (no rollback)", err)
	}
}

// TestEffectiveSpawnSession_ReSourcesFromPrimaryChat proves restart/rotation
// spawn paths (which hold only a session) read provider/account/model from the
// session's primary chat (BOS-381), and fall back to the session unchanged when
// no primary chat exists (the first-start seed).
func TestEffectiveSpawnSession_ReSourcesFromPrimaryChat(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	agentSessionID := "agent-1"
	acct := "acct-codex-2"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			SessionID:      "sess-1",
			AgentSessionID: agentSessionID,
			AgentName:      "codex",
			Model:          "gpt-5",
			AccountID:      &acct,
		}},
	}
	sess := &models.Session{ID: "sess-1", AgentName: "claude", Model: "sonnet", AgentSessionID: &agentSessionID}
	eff := h.lc.effectiveSpawnSession(context.Background(), sess)
	if eff.AgentName != "codex" {
		t.Errorf("eff.AgentName = %q, want codex (from primary chat)", eff.AgentName)
	}
	if eff.Model != "gpt-5" {
		t.Errorf("eff.Model = %q, want gpt-5 (from primary chat)", eff.Model)
	}
	if eff.AccountID == nil || *eff.AccountID != acct {
		t.Errorf("eff.AccountID = %v, want %q (from primary chat)", eff.AccountID, acct)
	}
	// The original session is not mutated.
	if sess.AgentName != "claude" || sess.Model != "sonnet" {
		t.Errorf("original session mutated: agent=%q model=%q", sess.AgentName, sess.Model)
	}

	// No primary chat (nil AgentSessionID) → session returned unchanged (seed path).
	seed := &models.Session{ID: "sess-x", AgentName: "claude", Model: "sonnet"}
	if got := h.lc.effectiveSpawnSession(context.Background(), seed); got != seed {
		t.Errorf("effectiveSpawnSession with no primary chat = %v, want the session unchanged", got)
	}
}

// TestStartTmuxChat_SeedsFirstChatAgentAccountModel proves the first chat's
// agent_chats row is seeded from the session's resolved provider/account/model
// (BOS-381): the chat becomes the runtime authority from the moment it exists.
func TestStartTmuxChat_SeedsFirstChatAgentAccountModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	acct := "acct-seed-1"
	h.sessions.sessions["sess-1"].Model = "sonnet"
	h.sessions.sessions["sess-1"].AccountID = &acct
	h.sessions.sessions["sess-1"].AgentName = "claude"

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("agent_chats Create calls = %d, want 1", len(h.chats.createCalls))
	}
	got := h.chats.createCalls[0]
	if got.AgentName != "claude" {
		t.Errorf("seed AgentName = %q, want claude", got.AgentName)
	}
	if got.Model != "sonnet" {
		t.Errorf("seed Model = %q, want sonnet", got.Model)
	}
	if got.AccountID == nil || *got.AccountID != acct {
		t.Errorf("seed AccountID = %v, want %q", got.AccountID, acct)
	}
}

func TestStartTmuxChat_PassesSelectedConfigHomeWithoutSessionSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	selectedHome := t.TempDir()
	h.lc.SetAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{
		"CODEX_HOME":     selectedHome,
		"ACCOUNT_SECRET": "must-not-reach-plugin",
	}})

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	got := h.agentFake.LastBuildInteractiveCommand.GetConfigHomeEnv()
	if got["CODEX_HOME"] != selectedHome {
		t.Fatalf("ConfigHomeEnv CODEX_HOME = %q, want selected account home %q", got["CODEX_HOME"], selectedHome)
	}
	if _, leaked := got["ACCOUNT_SECRET"]; leaked {
		t.Fatalf("ConfigHomeEnv must not pass session secrets to the plugin: %v", got)
	}
}

func TestStartTmuxChat_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	const supplyTitle = "Repair: Some session"
	const supplyPrompt = "/boss-repair"

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: supplyPrompt, Delivery: DeliverySubmit}, supplyTitle, HookOpts{})
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

	// SendPlan must have run. A single-line prompt ("/boss-repair") delivers via
	// literal keystrokes (send-keys -l), not bracketed paste.
	if h.tmuxFake.hasSubcommand("paste-buffer") {
		t.Error("single-line prompt must not use bracketed paste")
	}
	if !h.tmuxFake.hasLiteralSendKeys() {
		t.Error("expected literal send-keys -l delivery from SendPlan, none recorded")
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.ReadyMarker = "›"
	h.tmuxFake.capturePaneOutput = "OpenAI Codex\n›\n"

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "Repair: Some session", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if !h.tmuxFake.hasLiteralSendKeys() {
		t.Fatal("expected SendPlan to accept the agent ready marker and deliver the single-line prompt via literal keys")
	}
}

// TestStartTmuxChat_TmuxUnavailable verifies fail-closed behavior when tmux
// isn't on PATH: typed FailedPrecondition error, no chat row created.
func TestStartTmuxChat_TmuxUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.available = false

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Replace the agent registry with an empty map so claude is unloaded.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{})

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
// returns an error AND stamps a "(failed to start)" agent_chats row so the
// failure is visible to boss show / the TUI / external clients rather than only
// logged. The row is created here because the normal Step-7 creation never runs.
func TestStartTmuxChat_NewSessionFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.failSubcommand["new-session"] = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when tmux new-session fails")
	}
	// A failed-start row is created and stamped so the tmux launch failure is
	// not silently invisible.
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 agentChats.Create call to record the failed-start row, got %d", len(h.chats.createCalls))
	}
	if len(h.chats.markStartFailedCalls) != 1 {
		t.Fatalf("expected 1 MarkStartFailed call, got %d", len(h.chats.markStartFailedCalls))
	}
	if reason := h.chats.markStartFailedCalls[0].reason; !strings.Contains(reason, "tmux launch failed") {
		t.Errorf("MarkStartFailed reason = %q, want it to mention the tmux launch failure", reason)
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Make BuildInteractiveCommand return empty argv.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": &emptyArgvAgent{}})

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.createErr = fmt.Errorf("simulated DB failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.updateTmuxNameErr = fmt.Errorf("simulated update failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Force load-buffer (the first stage of SendPlan's bracketed-paste path) to
	// fail. The capture-pane ready-marker poll runs first; we leave that
	// succeeding so SendPlan reaches the real failure. A multi-line prompt keeps
	// delivery on the paste path (single-line prompts now use literal keys), so
	// the load-buffer failure injection still exercises the SendPlan error.
	h.tmuxFake.failSubcommand["load-buffer"] = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "line one\nline two", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-finalize", Delivery: DeliverySubmit}, "Finalize", HookOpts{
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

func TestReplaceBlockingChatForRepair_RejectsDifferentSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

func TestReclaimRepairChat_RefusesWhenTmuxLivenessErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetAgentLogsDir("") // explicitly clear

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].WorktreePath = ""

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when session has no worktree path")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
}

// TestStartCronTmuxChat_WrapperPropagatesPlanAndCronTitle pins the wrapper
// contract: the cron entry point (startTmuxChat with a CronJobID) must continue
// to call StartTmuxChat with prompt=session.Plan and title=`Run "<cron name>"`,
// regardless of how the underlying method evolves.
func TestStartCronTmuxChat_WrapperPropagatesPlanAndCronTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	h.sessions.sessions["sess-1"].Plan = "Run the audit"
	h.sessions.sessions["sess-1"].Title = "Nightly audit"

	_, err := h.lc.startTmuxChat(ctx, "sess-1", StartSessionOpts{CronJobID: "cron-1"}, h.sessions.sessions["sess-1"], nil)
	if err != nil {
		t.Fatalf("startTmuxChat: %v", err)
	}

	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(h.chats.createCalls))
	}
	if got, want := h.chats.createCalls[0].Title, `Run "Nightly audit"`; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}

	// A single-line plan ("Run the audit") is delivered via literal keystrokes
	// (send-keys -l), not bracketed paste; confirm the literal delivery ran.
	if h.tmuxFake.hasSubcommand("paste-buffer") {
		t.Error("single-line plan must not use bracketed paste")
	}
	if !h.tmuxFake.hasLiteralSendKeys() {
		t.Error("expected literal send-keys -l delivery (SendPlan), none recorded")
	}
}

func TestStartCronTmuxChat_CommandAvoidsBracketedPaste(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.CommandPrefix = "$"

	h.sessions.sessions["sess-1"].Plan = "/bs-sweep-mutation"
	h.sessions.sessions["sess-1"].Title = "Nightly mutation test"

	_, err := h.lc.startTmuxChat(ctx, "sess-1", StartSessionOpts{CronJobID: "cron-1"}, h.sessions.sessions["sess-1"], nil)
	if err != nil {
		t.Fatalf("startTmuxChat: %v", err)
	}

	if h.tmuxFake.hasSubcommand("load-buffer") || h.tmuxFake.hasSubcommand("paste-buffer") {
		t.Fatal("cron command input should use literal send-keys, not bracketed paste")
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetInitialCommand(); got != "/bs-sweep-mutation" {
		t.Fatalf("InitialCommand = %q, want /bs-sweep-mutation", got)
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
	if !slices.Equal(textCall.args, []string{"-t", tmuxName, "-l", "--", "$bs-sweep-mutation"}) {
		t.Fatalf("literal send-keys args = %v", textCall.args)
	}
	enterCall := sendKeys[len(sendKeys)-1]
	if !slices.Equal(enterCall.args, []string{"-t", tmuxName, "Enter"}) {
		t.Fatalf("Enter send-keys args = %v", enterCall.args)
	}
}

func TestStartTmuxChat_CommandUsesLiteralKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.CommandPrefix = "$"

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair", Delivery: DeliverySubmit}, "Repair: Some session", HookOpts{})
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
	if !slices.Equal(textCall.args, []string{"-t", tmuxName, "-l", "--", "$boss-repair"}) {
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.ConsumesInitialInput = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair", Delivery: DeliverySubmit}, "Repair: Some session", HookOpts{})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.ConsumesInitialInput = true

	configuredBeforeLaunch := false
	h.agentFake.OnConfigureHook = func() {
		configuredBeforeLaunch = !h.tmuxFake.hasSubcommand("new-session")
	}

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair", Delivery: DeliverySubmit}, "Repair: Some session", HookOpts{Token: "tok"})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.IsSupported = false

	armer := &fakePollArmer{}
	h.lc.SetPollArmer(armer)
	h.lc.SetDaemonCtx(ctx)

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Command: "boss-repair", Delivery: DeliverySubmit}, "Repair: Some session", HookOpts{
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)

	const tok = "tok-run-12345"
	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{Token: tok})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{}); err != nil {
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Deliberately don't call SetHookPort.

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{Token: "tok"})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)
	h.agentFake.ConfigureHookErr = fmt.Errorf("simulated hook config failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{Token: "tok"})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

	id, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "the prompt", Delivery: DeliverySubmit}, "the title", HookOpts{Token: "tok-2"})
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	// fakeAgent.IsSupported defaults to true.

	armer := &fakePollArmer{}
	h.lc.SetPollArmer(armer)
	h.lc.SetDaemonCtx(ctx)

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "p", Delivery: DeliverySubmit}, "T", HookOpts{Token: "tok-2"}); err != nil {
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: "agent-session-prior", Delivery: DeliverySubmit},
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: "agent-session-prior", Delivery: DeliverySubmit},
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: agentSessionID, Delivery: DeliverySubmit},
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
	if !slices.Equal(textCall.args, []string{"-t", tmuxName, "-l", "--", "$boss-repair"}) {
		t.Fatalf("literal send-keys args = %v", textCall.args)
	}
	enterCall := sendKeys[len(sendKeys)-1]
	if !slices.Equal(enterCall.args, []string{"-t", tmuxName, "Enter"}) {
		t.Fatalf("Enter send-keys args = %v", enterCall.args)
	}
}

func TestStartTmuxChat_ResumeReusesLivePaneByProviderSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: providerSessionID, Delivery: DeliverySubmit},
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

func TestStartTmuxChat_RespawnUsesStoredProviderSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].AgentName = "opencode"
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"opencode": h.agentFake})
	h.agentFake.ConsumesInitialInput = true

	const (
		logicalAgentSessionID = "agent-session-logical"
		providerSessionID     = "ses_opencode_provider"
	)
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:                "chat-opencode",
			SessionID:         "sess-1",
			AgentSessionID:    logicalAgentSessionID,
			ProviderSessionID: ptr(providerSessionID),
			AgentName:         "opencode",
		}},
	}

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: logicalAgentSessionID, Delivery: DeliverySubmit},
		"T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if id != logicalAgentSessionID {
		t.Fatalf("returned id = %q, want logical id %q", id, logicalAgentSessionID)
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetSessionId(); got != providerSessionID {
		t.Errorf("BuildInteractiveCommand SessionId = %q, want provider id %q", got, providerSessionID)
	}
	if !h.agentFake.LastBuildInteractiveCommand.GetResume() {
		t.Error("BuildInteractiveCommand Resume = false, want true")
	}
}

func TestStartTmuxChat_FreshLaunchPersistsProviderSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].AgentName = "opencode"
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"opencode": h.agentFake})
	h.agentFake.ConsumesInitialInput = true
	h.agentFake.ResolveInteractiveSessionIDFunc = func(req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
		if req.GetWorkDir() != h.sessions.sessions["sess-1"].WorktreePath {
			t.Errorf("ResolveInteractiveSessionID WorkDir = %q, want %q", req.GetWorkDir(), h.sessions.sessions["sess-1"].WorktreePath)
		}
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: "ses_opencode_fresh"}, nil
	}

	id, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "repair", Delivery: DeliverySubmit}, "T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if len(h.chats.providerSessionIDUpdates) != 1 {
		t.Fatalf("provider session updates = %d, want 1", len(h.chats.providerSessionIDUpdates))
	}
	got := h.chats.providerSessionIDUpdates[0]
	if got.agentSessionID != id {
		t.Errorf("provider update agent session id = %q, want %q", got.agentSessionID, id)
	}
	if got.providerSessionID == nil || *got.providerSessionID != "ses_opencode_fresh" {
		t.Errorf("provider update id = %v, want ses_opencode_fresh", got.providerSessionID)
	}
}

func TestStartTmuxChat_FreshLaunchRetriesProviderSessionIDResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].AgentName = "opencode"
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"opencode": h.agentFake})
	h.agentFake.ConsumesInitialInput = true

	resolveCalls := 0
	h.agentFake.ResolveInteractiveSessionIDFunc = func(_ *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
		}
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: "ses_opencode_after_startup"}, nil
	}

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "repair", Delivery: DeliverySubmit}, "T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if resolveCalls != 2 {
		t.Fatalf("ResolveInteractiveSessionID calls = %d, want 2", resolveCalls)
	}
	if len(h.chats.providerSessionIDUpdates) != 1 {
		t.Fatalf("provider session updates = %d, want 1", len(h.chats.providerSessionIDUpdates))
	}
	if got := h.chats.providerSessionIDUpdates[0].providerSessionID; got == nil || *got != "ses_opencode_after_startup" {
		t.Errorf("provider update id = %v, want ses_opencode_after_startup", got)
	}
}

// TestStartTmuxChat_ResumeStillArmsCompletion is the #491 regression guard: a
// resumed, hookless run must still arm completion using the reused id so a
// later WaitChatRun resolves when tmux reports the pane dead.
func TestStartTmuxChat_ResumeStillArmsCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
		ChatInput{Prompt: "p", ResumeAgentSessionID: "agent-session-prior", Delivery: DeliverySubmit},
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.deleteErr = errors.New("boom")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Prompt: "p", ResumeAgentSessionID: "agent-session-prior", Delivery: DeliverySubmit},
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

func TestAppendSystemPromptForFacts(t *testing.T) {
	job := "cron-42"
	cronSess := &models.Session{ID: "s1", Title: "Nightly triage", CronJobID: &job}
	plainSess := &models.Session{ID: "s2", Title: "Manual"}
	const agentSessionID = "agent-123"

	cronPrompt := AppendSystemPromptFor(cronSess, agentSessionID, "claude")
	for _, want := range []string{"s1", agentSessionID, cronAutonomyDirective, unattendedSubagentDirective, "rename"} {
		if !strings.Contains(cronPrompt, want) {
			t.Fatalf("cron prompt missing %q: %q", want, cronPrompt)
		}
	}

	zeroOutputPrompt := AppendSystemPromptFor(&models.Session{ID: "s3", CronJobID: &job, IsQuickChat: true}, agentSessionID, "claude")
	if !strings.Contains(zeroOutputPrompt, zeroOutputCronAutonomyDirective) || strings.Contains(zeroOutputPrompt, cronAutonomyDirective) {
		t.Fatalf("zero-output prompt = %q, want dedicated no-commit directive", zeroOutputPrompt)
	}
	// The subagent grant rides alongside the zero-output directive too: a
	// zero-output cron still runs skills that mandate awaited dispatch.
	if !strings.Contains(zeroOutputPrompt, unattendedSubagentDirective) {
		t.Fatalf("zero-output prompt missing the subagent directive: %q", zeroOutputPrompt)
	}

	plainPrompt := AppendSystemPromptFor(plainSess, agentSessionID, "claude")
	if !strings.Contains(plainPrompt, "s2") {
		t.Fatalf("plain prompt missing session id: %q", plainPrompt)
	}
	if strings.Contains(plainPrompt, cronAutonomyDirective) {
		t.Fatalf("plain prompt should not contain the cron directive: %q", plainPrompt)
	}
	// Permissive direction: an attended chat keeps the no-surprise-fan-out
	// default, so the grant must not leak outside the IsUnattended branch.
	if strings.Contains(plainPrompt, unattendedSubagentDirective) {
		t.Fatalf("attended prompt must not contain the subagent directive: %q", plainPrompt)
	}
	if AppendSystemPromptFor(nil, agentSessionID, "") != "" {
		t.Fatalf("nil session should yield empty prompt")
	}

	// Hardened guardrail must be present.
	if !strings.Contains(plainPrompt, "report") || !strings.Contains(plainPrompt, "blocked") {
		t.Fatalf("prompt missing the 'report blocked' guardrail: %q", plainPrompt)
	}

	// The prompt now points agents at `boss env` as the self-describing entry
	// point (BOS-94), while keeping `boss --help` as a fallback.
	if !strings.Contains(plainPrompt, "boss env") {
		t.Fatalf("prompt should reference `boss env` as the capability-discovery entry point: %q", plainPrompt)
	}

	// The subagent grant is only safe because it is bounded. A directive that
	// lost its bound would read as an unbounded fan-out licence, so pin the
	// bounding clause itself rather than merely its presence in the prompt.
	t.Run("subagent directive is bounded", func(t *testing.T) {
		for _, want := range []string{
			// Scoped to what the running skill already mandates…
			"protocol mandates",
			// …awaited, never backgrounded…
			"await every dispatch",
			"run_in_background",
			// …and non-delivery is greppable, not a silent clean report.
			"SUBAGENT-DISPATCH-UNAVAILABLE",
		} {
			if !strings.Contains(unattendedSubagentDirective, want) {
				t.Errorf("subagent directive lost its bounding clause %q: %q", want, unattendedSubagentDirective)
			}
		}
	})

	// This directive outranks a skill body, so an escalation worded as "cannot
	// dispatch" would override the inline fallback every dispatch-mandating
	// skill documents (boss-repair Strategy A/B/C, boss-build Step 6b/6c,
	// boss-finalize Steps 1-8) and let a transient tool error abandon work the
	// orchestrator could finish itself. The escalation must key on the mandated
	// work going UNDONE instead.
	t.Run("subagent directive preserves the documented inline fallback", func(t *testing.T) {
		for _, want := range []string{
			// The fallback is the sanctioned route, not a downgrade…
			"inline fallback",
			"clean result, not a downgrade",
			// …so the non-clean route is reserved for undone work.
			"Escalate only when the mandated work",
		} {
			if !strings.Contains(unattendedSubagentDirective, want) {
				t.Errorf("subagent directive lost its inline-fallback carve-out %q: %q", want, unattendedSubagentDirective)
			}
		}
		// Guard the specific regression: escalation must not be triggered by a
		// failed dispatch call on its own.
		if strings.Contains(unattendedSubagentDirective, "cannot dispatch a subagent the protocol mandates, do not report a clean result") {
			t.Errorf("escalation re-keyed on dispatch failure, overriding the skills' inline fallback: %q", unattendedSubagentDirective)
		}
	})
}

// bossd does not configure MCP servers for a chat: each harness discovers them
// natively and the repo declares them, so bossd cannot know whether this chat
// has the boss server and must never claim it. BOSS_MCP_BIN is a separate
// thing — an env export that still happens — so this asserts the tool claim is
// gone WITHOUT asserting the identifier is (that pairing is guarded by
// TestBossSessionContext_AdvertisesExactlyExportedIdentifiers).
func TestBuildAppendSystemPrompt_NeverAdvertisesBossMcpTools(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", AgentName: "claude", WorktreePath: t.TempDir()}
	text, classes := BuildAppendSystemPrompt(sess, "chat-1", "claude")
	if strings.Contains(text, "mcp__boss__") {
		t.Fatalf("prompt must not advertise mcp__boss__*:\n%s", text)
	}
	if strings.Contains(text, "MCP server is also wired into this chat") {
		t.Fatalf("prompt must not claim a wired MCP server:\n%s", text)
	}
	if len(classes) == 0 {
		t.Fatal("session-context instruction class must still be carried")
	}
}

// TestBossPromptHasNoStaleCapabilityList guards against re-introducing the
// hand-listed tool subset (the original bug). `boss env` (BOS-94) is not banned:
// it is the authoritative, self-describing entry point the prompt points at.
// This only asserts the stale hand-listed subset never returns.
func TestBossPromptHasNoStaleCapabilityList(t *testing.T) {
	prompt := AppendSystemPromptFor(&models.Session{ID: "s1"}, "agent-1", "claude")
	for _, banned := range []string{
		"list_sessions", "get_session", "create_session", "record_chat",
		"wake_chat", "update_session",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("prompt must not reference %q (Phase-1 drift guard): %q", banned, prompt)
		}
	}
}

// TestResolveRepoLocalBoss covers the BOS-230 repo-local fallback in isolation:
// it returns <worktree>/bin/boss only when that path exists and is an executable
// regular file, and "" for every other shape (no worktree, missing file, a
// directory, or a non-executable file).
func TestResolveRepoLocalBoss(t *testing.T) {
	if got := resolveRepoLocalBoss(""); got != "" {
		t.Fatalf("empty worktree should resolve nothing, got %q", got)
	}

	// Worktree with no bin/boss at all.
	empty := t.TempDir()
	if got := resolveRepoLocalBoss(empty); got != "" {
		t.Fatalf("worktree without bin/boss should resolve nothing, got %q", got)
	}

	// Worktree with an executable bin/boss → resolved.
	exe := t.TempDir()
	binDir := filepath.Join(exe, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bossPath := filepath.Join(binDir, "boss")
	if err := os.WriteFile(bossPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveRepoLocalBoss(exe); got != bossPath {
		t.Fatalf("executable repo bin/boss should resolve to %q, got %q", bossPath, got)
	}

	// Worktree whose bin/boss is not executable → not resolved.
	nonExe := t.TempDir()
	nonExeBin := filepath.Join(nonExe, "bin")
	if err := os.MkdirAll(nonExeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonExeBin, "boss"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveRepoLocalBoss(nonExe); got != "" {
		t.Fatalf("non-executable bin/boss must not resolve, got %q", got)
	}

	// Worktree whose bin/boss is a directory → not resolved.
	dirCase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirCase, "bin", "boss"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveRepoLocalBoss(dirCase); got != "" {
		t.Fatalf("directory bin/boss must not resolve, got %q", got)
	}
}

// TestResolveSessionFacts_RepoLocalBossFallback proves ResolveSessionFacts wires
// the repo-local fallback: with no trusted `boss` resolvable (PATH neutralized)
// but the session worktree carrying an executable bin/boss, BossBin is the repo
// build. Neither trusted nor repo-local → BossBin stays "".
func TestResolveSessionFacts_RepoLocalBossFallback(t *testing.T) {
	// Neutralize PATH so config.ResolveTrustedExecutable("boss") returns "".
	t.Setenv("PATH", t.TempDir())

	wt := t.TempDir()
	binDir := filepath.Join(wt, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bossPath := filepath.Join(binDir, "boss")
	if err := os.WriteFile(bossPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", AgentName: "claude", WorktreePath: wt}
	f := ResolveSessionFacts(sess, "agent-1", "claude")
	if f.BossBin != bossPath {
		t.Fatalf("expected repo-local fallback BossBin=%q, got %q", bossPath, f.BossBin)
	}

	// A worktree with no repo build resolves nothing.
	noBuild := &models.Session{ID: "s2", WorktreePath: t.TempDir()}
	if f := ResolveSessionFacts(noBuild, "agent-2", "claude"); f.BossBin != "" {
		t.Fatalf("no trusted or repo-local boss should leave BossBin empty, got %q", f.BossBin)
	}
}

// TestBossSessionContext_BossBinAdvertisedWhenResolved proves the resolved case:
// BossBin present → the identifier list advertises BOSS_BIN and the guidance
// points at the resolved binary, with no "not available"/`make build` fallback.
func TestBossSessionContext_BossBinAdvertisedWhenResolved(t *testing.T) {
	got := bossSessionContext(SessionFacts{SessionID: "s", BossBin: "/trusted/boss", McpBin: "/trusted/mcp"})
	// Byte-stable identifier tail from today (regression guard on the resolved case).
	if !strings.Contains(got, "BOSS_SETTINGS_PATH, BOSS_SOCKET, BOSS_BIN, BOSS_MCP_BIN (context for you, not an") {
		t.Fatalf("resolved prompt must advertise BOSS_BIN/BOSS_MCP_BIN in the identifier list: %q", got)
	}
	if !strings.Contains(got, "/trusted/boss env") {
		t.Fatalf("resolved prompt must point guidance at the resolved binary: %q", got)
	}
	if strings.Contains(got, "make build") || strings.Contains(got, "./bin/boss") {
		t.Fatalf("resolved prompt must not name the make build fallback: %q", got)
	}
}

// TestBossSessionContext_BossBinOmittedWhenUnresolved proves the BOS-230 honest
// prompt: no resolvable boss → BOSS_BIN is omitted from the identifier list and
// the guidance names the `make build` + `./bin/boss` fallback instead of an
// advertised-but-empty var.
func TestBossSessionContext_BossBinOmittedWhenUnresolved(t *testing.T) {
	got := bossSessionContext(SessionFacts{SessionID: "s"})
	if strings.Contains(got, "BOSS_BIN") {
		t.Fatalf("unresolved prompt must not advertise BOSS_BIN: %q", got)
	}
	// With neither binary resolved, the list ends at BOSS_SOCKET.
	if !strings.Contains(got, "BOSS_SETTINGS_PATH, BOSS_SOCKET (context for you, not an") {
		t.Fatalf("unresolved list must end at BOSS_SOCKET: %q", got)
	}
	if !strings.Contains(got, "make build") || !strings.Contains(got, "./bin/boss") {
		t.Fatalf("unresolved prompt must name the make build + ./bin/boss fallback: %q", got)
	}
}

// TestBossSessionContext_AdvertisesExactlyExportedIdentifiers guards the
// SessionFacts invariant that the prompt's advertised env-var list can never
// disagree with what managedSessionEnv actually exports (BOS-230). The advertised
// idVars list is a parallel copy of managedSessionEnv's export gating; this test
// is the drift guard that keeps the copy honest. For fully-resolved facts, every
// exported identifier (all keys except the behavioral cron vars) must appear in
// the prompt, and the two conditional vars must be omitted when their binary is
// unresolved — so a future unconditional export can't silently drift the prompt.
func TestBossSessionContext_AdvertisesExactlyExportedIdentifiers(t *testing.T) {
	f := SessionFacts{
		SessionID: "s", AgentSessionID: "a", RepoID: "r", Agent: "claude",
		Worktree: "/wt", SettingsPath: "/cfg", Socket: "/sock",
		BossBin: "/trusted/boss", McpBin: "/trusted/mcp",
		IsCron: true, IsUnattended: true, CronJobID: "j", CronName: "n",
	}
	// Behavioral (non-identifier) vars are intentionally never advertised.
	behavioral := map[string]bool{"BOSS_CRON": true, "BOSS_CRON_JOB_ID": true, "BOSS_CRON_NAME": true}

	prompt := bossSessionContext(f)
	for name := range managedSessionEnv(f) {
		if behavioral[name] {
			continue
		}
		if !strings.Contains(prompt, name) {
			t.Errorf("exported identifier %q is not advertised in the prompt — env/prompt drift", name)
		}
	}

	// The two conditional vars must vanish from the prompt when unexported.
	fNoBins := f
	fNoBins.BossBin = ""
	fNoBins.McpBin = ""
	noBinPrompt := bossSessionContext(fNoBins)
	if strings.Contains(noBinPrompt, "BOSS_BIN") {
		t.Errorf("BOSS_BIN advertised when not exported: %q", noBinPrompt)
	}
	if strings.Contains(noBinPrompt, "BOSS_MCP_BIN") {
		t.Errorf("BOSS_MCP_BIN advertised when not exported: %q", noBinPrompt)
	}
}

func TestBossSessionContextWarnsForDriftingInstalledSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	installed := filepath.Join(home, ".claude", "skills", libskillinstall.Namespace, "boss")
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "SKILL.md"), []byte("installed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(libskillinstall.Namespace, "boss"), filepath.Join(home, ".claude", "skills", "boss")); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(worktree, libskillinstall.SourceRelPath, "skills", "boss")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}

	facts := SessionFacts{SessionID: "s", Worktree: worktree, Agent: "claude"}
	if got := bossSessionContext(facts); !strings.Contains(got, "installed skill bodies may be out of date") {
		t.Fatalf("prompt missing drift warning: %q", got)
	}
	if err := os.WriteFile(filepath.Join(installed, "SKILL.md"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bossSessionContext(facts); strings.Contains(got, "installed skill bodies may be out of date") {
		t.Fatalf("matching prompt contains drift warning: %q", got)
	}
	if got := bossSessionContext(SessionFacts{SessionID: "s", Worktree: t.TempDir(), Agent: "claude"}); strings.Contains(got, "installed skill bodies may be out of date") {
		t.Fatalf("no-source prompt contains drift warning: %q", got)
	}
	if got := bossSessionContext(SessionFacts{SessionID: "s", Agent: "claude"}); strings.Contains(got, "installed skill bodies may be out of date") {
		t.Fatalf("empty-worktree prompt contains drift warning: %q", got)
	}
}

func TestResolveSessionFacts(t *testing.T) {
	sess := &models.Session{
		ID: "s1", RepoID: "r1", AgentName: "claude", WorktreePath: "/wt", Title: "Manual",
	}
	f := ResolveSessionFacts(sess, "agent-99", "claude")
	if f.SessionID != "s1" || f.AgentSessionID != "agent-99" ||
		f.RepoID != "r1" || f.Agent != "claude" || f.Worktree != "/wt" {
		t.Fatalf("identifiers not threaded: %+v", f)
	}
	if f.IsCron {
		t.Fatalf("non-cron session marked cron")
	}
	if f.SettingsPath == "" {
		t.Fatalf("settings path not resolved")
	}

	// agentName param wins over sess.AgentName.
	fCross := ResolveSessionFacts(sess, "agent-99", "codex")
	if fCross.Agent != "codex" {
		t.Fatalf("explicit agentName should win over sess.AgentName: got %q, want %q", fCross.Agent, "codex")
	}

	// empty agentName falls back to sess.AgentName.
	fFallback := ResolveSessionFacts(sess, "agent-99", "")
	if fFallback.Agent != "claude" {
		t.Fatalf("empty agentName should fall back to sess.AgentName: got %q, want %q", fFallback.Agent, "claude")
	}

	job := "cron-42"
	cron := &models.Session{ID: "s2", Title: "Nightly", CronJobID: &job}
	cf := ResolveSessionFacts(cron, "agent-1", "")
	if !cf.IsCron || cf.CronJobID != "cron-42" || cf.CronName != "Nightly" {
		t.Fatalf("cron facts wrong: %+v", cf)
	}
}

func TestManagedSessionEnv(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", AgentName: "claude", WorktreePath: "/wt", Title: "Manual"}
	// Pass "codex" as the chat agent to verify the param wins over sess.AgentName ("claude").
	env := ManagedSessionEnv(sess, "agent-7", "codex")
	for k, want := range map[string]string{
		"BOSS_SESSION_ID":       "s1",
		"BOSS_AGENT_SESSION_ID": "agent-7",
		"BOSS_REPO_ID":          "r1",
		"BOSS_AGENT":            "codex",
		"BOSS_WORKTREE":         "/wt",
	} {
		if env[k] != want {
			t.Fatalf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["BOSS_CRON"]; ok {
		t.Fatalf("non-cron session must not set BOSS_CRON")
	}
	if _, ok := env["BOSS_SETTINGS_PATH"]; !ok {
		t.Fatalf("BOSS_SETTINGS_PATH must be set")
	}

	// Empty agentName falls back to sess.AgentName.
	envFallback := ManagedSessionEnv(sess, "agent-7", "")
	if envFallback["BOSS_AGENT"] != "claude" {
		t.Fatalf("empty agentName should fall back to sess.AgentName: got %q, want %q", envFallback["BOSS_AGENT"], "claude")
	}

	job := "cron-42"
	cron := &models.Session{ID: "s2", Title: "Nightly", CronJobID: &job}
	cenv := ManagedSessionEnv(cron, "agent-1", "")
	if cenv["BOSS_CRON"] != "true" || cenv["BOSS_CRON_JOB_ID"] != "cron-42" || cenv["BOSS_CRON_NAME"] != "Nightly" {
		t.Fatalf("cron env wrong: %v", cenv)
	}
}

// TestManagedSessionEnv_IsTmuxUnattended proves a tmux_unattended session (no
// CronJobID) gets BOSS_CRON=true — so shell-mode/autonomy detection fires — but
// NOT BOSS_CRON_JOB_ID/BOSS_CRON_NAME, which are meaningless without a real
// scheduled job.
func TestManagedSessionEnv_IsTmuxUnattended(t *testing.T) {
	sess := &models.Session{ID: "s9", Title: "Epic child", IsTmuxUnattended: true}
	env := ManagedSessionEnv(sess, "agent-9", "claude")
	if env["BOSS_CRON"] != "true" {
		t.Fatalf("tmux_unattended session must set BOSS_CRON=true, got %q", env["BOSS_CRON"])
	}
	if _, ok := env["BOSS_CRON_JOB_ID"]; ok {
		t.Errorf("tmux_unattended session must not set BOSS_CRON_JOB_ID, got %q", env["BOSS_CRON_JOB_ID"])
	}
	if _, ok := env["BOSS_CRON_NAME"]; ok {
		t.Errorf("tmux_unattended session must not set BOSS_CRON_NAME, got %q", env["BOSS_CRON_NAME"])
	}
}

// TestAppendSystemPromptFor_IsTmuxUnattended proves the autonomy directive is
// appended for a tmux_unattended session (no CronJobID), just as for cron.
func TestAppendSystemPromptFor_IsTmuxUnattended(t *testing.T) {
	sess := &models.Session{ID: "s9", Title: "Epic child", IsTmuxUnattended: true}
	prompt := AppendSystemPromptFor(sess, "agent-9", "claude")
	if !strings.Contains(prompt, cronAutonomyDirective) {
		t.Fatal("tmux_unattended session must get the autonomy directive")
	}
	// Unattendedness, not cron-ness, is what grants the subagent dispatch: an
	// epic child has no CronJobID but runs the same dispatch-mandating skills.
	if !strings.Contains(prompt, unattendedSubagentDirective) {
		t.Fatal("tmux_unattended session must get the subagent directive")
	}
}

func TestMergeEnv_ManagedKeysWin(t *testing.T) {
	overlay := map[string]string{
		"PROOF_ANTHROPIC_API_KEY": "secret-anthropic",
		"BOSS_PROOF_R2_BUCKET":    "bossanova-proof-production",
		// A hostile/misconfigured overlay must never be able to shadow a
		// managed BOSS_* key.
		"BOSS_SESSION_ID": "OVERLAY-SHOULD-NOT-WIN",
	}

	cases := []struct {
		name string
		sess *models.Session
	}{
		{"non-cron", &models.Session{ID: "s1", RepoID: "r1", AgentName: "claude", WorktreePath: "/wt"}},
		{"cron", func() *models.Session {
			job := "cron-9"
			return &models.Session{ID: "s2", RepoID: "r2", AgentName: "codex", WorktreePath: "/wt2", Title: "Nightly", CronJobID: &job}
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := ManagedSessionEnv(tc.sess, "agent-x", "")
			merged := mergeEnv(base, overlay)

			// Managed key wins on conflict.
			if merged["BOSS_SESSION_ID"] != tc.sess.ID {
				t.Errorf("BOSS_SESSION_ID = %q, want managed %q (overlay must not win)", merged["BOSS_SESSION_ID"], tc.sess.ID)
			}
			// Overlay-only keys are added.
			if merged["PROOF_ANTHROPIC_API_KEY"] != "secret-anthropic" {
				t.Errorf("proof secret not merged: %v", merged)
			}
			if merged["BOSS_PROOF_R2_BUCKET"] != "bossanova-proof-production" {
				t.Errorf("proof constant not merged: %v", merged)
			}
			// Every managed key survives.
			for k, v := range base {
				if merged[k] != v {
					t.Errorf("managed key %q lost: got %q want %q", k, merged[k], v)
				}
			}
		})
	}
}

// fakeProofEnvResolver is a proofEnvResolver returning a fixed overlay
// without touching a real keyring.
type fakeProofEnvResolver struct{ env map[string]string }

func (f fakeProofEnvResolver) Resolve() map[string]string { return f.env }

// TestLifecycleTmuxEnvIncludesProofOverlay exercises the exact expression at
// the tmux NewSession call site — mergeEnv(ManagedSessionEnv(...),
// l.resolveProofEnv()) — with an injected fake resolver, asserting the proof
// overlay reaches the session env while managed BOSS_* keys stay authoritative.
func TestLifecycleTmuxEnvIncludesProofOverlay(t *testing.T) {
	l := &Lifecycle{}
	l.SetProofEnvResolver(fakeProofEnvResolver{env: map[string]string{
		"PROOF_ANTHROPIC_API_KEY": "secret-anthropic",
		"CLOUDFLARE_API_TOKEN":    "secret-cf",
		"BOSS_PROOF_R2_BUCKET":    "bossanova-proof-production",
		"BOSS_SESSION_ID":         "OVERLAY-MUST-NOT-WIN",
	}})

	sess := &models.Session{ID: "s1", RepoID: "r1", AgentName: "claude", WorktreePath: "/wt"}
	env := mergeEnv(ManagedSessionEnv(sess, "agent-7", sess.AgentName), l.resolveProofEnv())

	if env["PROOF_ANTHROPIC_API_KEY"] != "secret-anthropic" || env["CLOUDFLARE_API_TOKEN"] != "secret-cf" {
		t.Errorf("proof secrets missing from tmux env: %v", env)
	}
	if env["BOSS_PROOF_R2_BUCKET"] != "bossanova-proof-production" {
		t.Errorf("proof constant missing from tmux env: %v", env)
	}
	if env["BOSS_SESSION_ID"] != "s1" {
		t.Errorf("managed BOSS_SESSION_ID overwritten by overlay: %q", env["BOSS_SESSION_ID"])
	}
}

// TestLifecycleResolveProofEnvNilResolver confirms a lifecycle with no
// resolver wired degrades to nil (older/test wiring) rather than panicking.
func TestLifecycleResolveProofEnvNilResolver(t *testing.T) {
	l := &Lifecycle{}
	if got := l.resolveProofEnv(); got != nil {
		t.Errorf("expected nil overlay with no resolver, got %v", got)
	}
}

func TestMergeEnv_NilOverlayPreservesBase(t *testing.T) {
	base := map[string]string{"BOSS_SESSION_ID": "s1", "BOSS_AGENT": "claude"}
	merged := mergeEnv(base, nil)
	if len(merged) != len(base) {
		t.Fatalf("nil overlay changed size: %v", merged)
	}
	for k, v := range base {
		if merged[k] != v {
			t.Errorf("key %q = %q, want %q", k, merged[k], v)
		}
	}
}

// TestClampInt32 is the BOS-413 boundary table-test for the package-local
// gosec-G115 clamp helper: normal values pass through; out-of-range int inputs
// clamp to the int32 extremes instead of wrapping. int32 range is
// [-2147483648, 2147483647].
func TestClampInt32(t *testing.T) {
	const (
		maxI32 = 2147483647
		minI32 = -2147483648
	)
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"normal", 42, 42},
		{"zero", 0, 0},
		{"max", maxI32, maxI32},
		{"min", minI32, minI32},
		{"clampsHigh", maxI32 + 1, maxI32},
		{"clampsLow", minI32 - 1, minI32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampInt32(tt.in); got != tt.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestBuildAppendSystemPromptClasses pins the class list to the branches
// actually taken when building the suffix. bossd reports these names when a
// runner drops the suffix, so a class that drifts from the text it stands for
// would make the report lie.
func TestBuildAppendSystemPromptClasses(t *testing.T) {
	job := "cron-42"
	cases := []struct {
		name        string
		sess        *models.Session
		wantClasses []string
	}{
		{
			name:        "attended session carries session context only",
			sess:        &models.Session{ID: "s1", Title: "Manual"},
			wantClasses: []string{InstructionClassSessionContext},
		},
		{
			name:        "cron session adds the autonomy directive",
			sess:        &models.Session{ID: "s2", Title: "Nightly", CronJobID: &job},
			wantClasses: []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
		},
		{
			// The zero-output directive is different prose but the same class:
			// it is still the unattended autonomy instruction.
			name:        "zero-output cron session still reports the directive",
			sess:        &models.Session{ID: "s3", CronJobID: &job, IsQuickChat: true},
			wantClasses: []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
		},
		{
			name:        "tmux_unattended session adds the autonomy directive",
			sess:        &models.Session{ID: "s4", Title: "Epic child", IsTmuxUnattended: true},
			wantClasses: []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
		},
		{
			name:        "detached session adds the autonomy directive",
			sess:        &models.Session{ID: "s5", Title: "Headless", Detach: true},
			wantClasses: []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, classes := BuildAppendSystemPrompt(tc.sess, "agent-1", "claude")
			if strings.Join(classes, ",") != strings.Join(tc.wantClasses, ",") {
				t.Fatalf("classes = %v, want %v", classes, tc.wantClasses)
			}
			// The exported wrapper must stay byte-identical to the text the
			// class-bearing builder produces: argv is unchanged by this feature.
			if got := AppendSystemPromptFor(tc.sess, "agent-1", "claude"); got != text {
				t.Fatalf("AppendSystemPromptFor diverged from BuildAppendSystemPrompt:\n got %q\nwant %q", got, text)
			}
			// text-iff-class: the directive class is claimed exactly when some
			// directive prose is actually in the suffix.
			hasDirective := strings.Contains(text, cronAutonomyDirective) ||
				strings.Contains(text, zeroOutputCronAutonomyDirective)
			claimsDirective := slices.Contains(classes, InstructionClassAutonomyDirective)
			if hasDirective != claimsDirective {
				t.Fatalf("directive text present = %v but class claimed = %v: %q", hasDirective, claimsDirective, text)
			}
		})
	}
}

// A nil session builds nothing, so there is nothing to report on.
// The doc contract is "classes is nil exactly when the text is empty", which
// takes two assertions: the nil session is the only empty case, and a plain
// session yields both. Without the second half a regression that returned an
// empty suffix would still report a class, and bossd would log a drop for an
// instruction it never built.
func TestBuildAppendSystemPromptNilSession(t *testing.T) {
	text, classes := BuildAppendSystemPrompt(nil, "agent-1", "claude")
	if text != "" || classes != nil {
		t.Fatalf("nil session = (%q, %v), want empty text and nil classes", text, classes)
	}

	text, classes = BuildAppendSystemPrompt(&models.Session{ID: "s1"}, "agent-1", "claude")
	if text == "" || len(classes) == 0 {
		t.Fatalf("plain session = (%q, %v), want a non-empty suffix and its class", text, classes)
	}
}
