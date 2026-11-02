package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newTestServer returns a Server wired to a nil host (the host client is
// only used by paths the agent runner doesn't exercise in unit tests) and
// a default Runner. Helper kept colocated with the tests so each test reads
// like a one-liner.
func newTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	return newServer(nil, zerolog.Nop(), opts...)
}

// TestConfigureFinalizeHookReturnsUnsupported asserts that codex declines
// finalize-via-hook (no in-CLI Stop-hook surface). The daemon falls back
// to ExitStatus polling for codex sessions.
func TestConfigureFinalizeHookReturnsUnsupported(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir: t.TempDir(), SessionId: "s1", HookToken: "tkn", HookPort: 12345,
	})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook: %v", err)
	}
	if resp.IsSupported {
		t.Error("IsSupported = true, want false for codex")
	}
}

func TestRemoveAgentRunHookReturnsUnsupported(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{
		WorkDir: t.TempDir(), AgentSessionId: "agent-1",
	})
	if err != nil {
		t.Fatalf("RemoveAgentRunHook: %v", err)
	}
	if resp.IsSupported {
		t.Error("IsSupported = true, want false for codex")
	}
}

func TestRemoveAgentRunHookDispatch(t *testing.T) {
	client := newGeneratedTestClient(t)
	resp, err := client.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetIsSupported() {
		t.Fatal("stub hook unexpectedly supported")
	}
}

// TestGetInfoIncludesCodexUserSettings verifies the three codex-specific
// user settings (sandbox / approval / model) are advertised so the
// settings UI can surface them without a separate codex-aware path.
//
// sandbox and approval are advertised as ENUM with AllowedValues so the
// TUI cycle picker constrains user input to values codex actually
// accepts; model is STRING because codex permits arbitrary model
// identifiers.
func TestGetInfoIncludesCodexUserSettings(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.GetInfo(context.Background(), &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if resp.Info == nil {
		t.Fatal("Info nil")
	}
	if resp.Info.Name != "codex" {
		t.Errorf("Info.Name = %q, want codex", resp.Info.Name)
	}

	byKey := map[string]*bossanovav1.UserSetting{}
	for _, us := range resp.Info.UserSettings {
		byKey[us.Key] = us
	}

	for _, key := range []string{"sandbox", "approval", "model", "effort", "dangerously_bypass_approvals_and_sandbox"} {
		if byKey[key] == nil {
			t.Errorf("user setting %q missing from GetInfo", key)
		}
	}

	// dangerously_bypass_approvals_and_sandbox: BOOL, defaults to "false".
	bypass := byKey["dangerously_bypass_approvals_and_sandbox"]
	if bypass != nil {
		if bypass.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_BOOL {
			t.Errorf("bypass.Type = %v, want USER_SETTING_TYPE_BOOL", bypass.Type)
		}
		if bypass.DefaultValue != "false" {
			t.Errorf("bypass.DefaultValue = %q, want \"false\"", bypass.DefaultValue)
		}
	}

	// sandbox: ENUM, "" first, then the codex-accepted modes.
	sandbox := byKey["sandbox"]
	if sandbox != nil {
		if sandbox.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM {
			t.Errorf("sandbox.Type = %v, want USER_SETTING_TYPE_ENUM", sandbox.Type)
		}
		wantSandbox := []string{"", "read-only", "workspace-write", "danger-full-access"}
		if !equalStrings(sandbox.AllowedValues, wantSandbox) {
			t.Errorf("sandbox.AllowedValues = %v, want %v", sandbox.AllowedValues, wantSandbox)
		}
	}

	// approval: ENUM, "" first, then the codex-accepted policies.
	approval := byKey["approval"]
	if approval != nil {
		if approval.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM {
			t.Errorf("approval.Type = %v, want USER_SETTING_TYPE_ENUM", approval.Type)
		}
		wantApproval := []string{"", "untrusted", "on-failure", "on-request", "never"}
		if !equalStrings(approval.AllowedValues, wantApproval) {
			t.Errorf("approval.AllowedValues = %v, want %v", approval.AllowedValues, wantApproval)
		}
	}

	// model: STRING (codex accepts arbitrary identifiers, no fixed set).
	model := byKey["model"]
	if model != nil {
		if model.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING {
			t.Errorf("model.Type = %v, want USER_SETTING_TYPE_STRING", model.Type)
		}
		if len(model.AllowedValues) != 0 {
			t.Errorf("model.AllowedValues = %v, want empty (string-typed setting)", model.AllowedValues)
		}
	}

	// effort: ENUM, "" first, codex reasoning efforts only, and no "max".
	effort := byKey["effort"]
	if effort != nil {
		if effort.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM {
			t.Errorf("effort.Type = %v, want USER_SETTING_TYPE_ENUM", effort.Type)
		}
		if effort.DefaultValue != "medium" {
			t.Errorf("effort.DefaultValue = %q, want medium", effort.DefaultValue)
		}
		wantEffort := []string{"", "low", "medium", "high", "xhigh"}
		if !equalStrings(effort.AllowedValues, wantEffort) {
			t.Errorf("effort.AllowedValues = %v, want %v", effort.AllowedValues, wantEffort)
		}
	}
}

func equalStrings(a, b []string) bool {
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

// TestStopRunMissingSessionReturnsNotFound asserts the server maps an
// unknown-session Stop to codes.NotFound. Daemon callers rely on this
// status to distinguish "session never started" from a real internal
// error so they can suppress retries on already-stopped sessions.
func TestStopRunMissingSessionReturnsNotFound(t *testing.T) {
	s := newTestServer(t)
	_, err := s.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{
		SessionId: "session-that-never-started",
	})
	if err == nil {
		t.Fatal("StopRun for unknown session: want error, got nil")
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("StopRun status code = %v, want NotFound", got)
	}
}

// TestIsRunningReportsRunnerState asserts the IsRunning RPC reflects
// the runner's state; for a server with no started runs, every session
// ID reports false. (The complementary "true while running" path is
// covered by main_test.go's TestRunnerEndToEndWithFakeCodex which polls
// IsRunning until the fake exits.)
func TestIsRunningReportsRunnerState(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{
		SessionId: "anything",
	})
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if resp.Running {
		t.Error("IsRunning = true for unknown session, want false")
	}
}

// TestExitStatusUnknownHeadlessRunnerSessionIsComplete documents the
// headless StartAgentRun polling contract. Interactive tmux chats are launched via
// BuildInteractiveCommand and must not use this runner status as proof of chat
// completion.
func TestExitStatusUnknownHeadlessRunnerSessionIsComplete(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{
		SessionId: "session-the-runner-never-knew",
	})
	if err != nil {
		t.Fatalf("ExitStatus: %v", err)
	}
	if !resp.IsComplete {
		t.Error("IsComplete = false for unknown session, want true")
	}
	if resp.ExitError != "" {
		t.Errorf("ExitError = %q for unknown session, want empty", resp.ExitError)
	}
}

// TestHasQuestionPromptDelegatesToDetector exercises the RPC boundary
// against both pane shapes (approval menu / idle pane). The detector
// itself is unit-tested in question_test.go; this confirms the server
// passes PaneContent through unchanged.
func TestHasQuestionPromptDelegatesToDetector(t *testing.T) {
	s := newTestServer(t)

	// Real codex 0.129.0 footer triggers detection.
	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{
		PaneContent: []byte("  1. Yes\n  2. No\n\nPress enter to confirm or esc to cancel\n"),
	})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(menu): %v", err)
	}
	if !resp.HasPrompt {
		t.Error("HasPrompt = false for approval menu, want true")
	}

	// Idle pane (no menu, no footer) does not trigger.
	respIdle, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{
		PaneContent: []byte("just some output\n› placeholder for input\n"),
	})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(idle): %v", err)
	}
	if respIdle.HasPrompt {
		t.Error("HasPrompt = true for idle pane, want false")
	}
}

// TestBlocksInputIgnoresScrolledOffApproval is the regression BOS-600's delivery
// gate depends on. blocks_input is what the daemon refuses a send on, and the
// pane it hands us is captured WITH scrollback — so the question cannot be "does
// an approval footer appear anywhere in this buffer?" An approval the user
// answered long ago is still in the buffer; answering yes to that would refuse
// every subsequent delivery to a chat that is sitting idle at its composer.
//
// The fixture is the real captured approval pane (testdata/panes/question.txt),
// scrolled up by appending plain output rows — the same thing the TUI does when
// the agent keeps going after an approval is answered. Nothing about the
// approval text is edited, so this cannot pass by weakening the grammar.
func TestBlocksInputIgnoresScrolledOffApproval(t *testing.T) {
	s := newTestServer(t)

	live, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: live})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(live): %v", err)
	}
	if !resp.BlocksInput {
		t.Fatal("BlocksInput = false while the approval menu is on screen; the delivery gate would type into it")
	}

	scrolled := append(bytes.Clone(live), []byte(strings.Repeat("  wrote /tmp/codex-approval-test.txt\n", 40))...)
	respScrolled, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: scrolled})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(scrolled): %v", err)
	}
	if respScrolled.BlocksInput {
		t.Error("BlocksInput = true for an approval that has scrolled out of view; delivery to this chat would be refused forever")
	}
}

// TestBlocksInputSurvivesPanePadding is the other half of the tail bound, and
// the half that fails open. `tmux capture-pane` emits every row of the pane, so
// a capture carries as many trailing blank rows as the conversation is short —
// and codex draws its menu under the last output line, not at the pane bottom.
// A window counted from the raw end of the buffer is therefore spent on padding
// before it reaches the menu: the real 60-row fixture below has its approval
// footer on row 35, so four more blank rows would have hidden it. Hiding it
// means blocks_input goes false and the daemon types the payload plus Enter
// into a live approval menu — precisely the incident this branch exists to
// stop, reintroduced by the fix for the opposite one.
//
// The fixture text is again untouched; only the padding grows.
func TestBlocksInputSurvivesPanePadding(t *testing.T) {
	s := newTestServer(t)

	live, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	for _, blankRows := range []int{4, 12, 40, 200} {
		padded := append(bytes.Clone(live), []byte(strings.Repeat("\n", blankRows))...)
		resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: padded})
		if err != nil {
			t.Fatalf("HasQuestionPrompt(padded %d): %v", blankRows, err)
		}
		if !resp.BlocksInput {
			t.Errorf("BlocksInput = false with %d blank pane rows below an on-screen approval menu; the delivery gate would type into it", blankRows)
		}
	}
}

// TestBlocksInputMatchesTallRequestUserInputCard pins the one modal whose
// grammar spans more than its footer: the request_user_input picker is matched
// from a "Question N/M (K unanswered)" header down to its footer, so a card
// taller than the modal window would lose the header to the slice and read as
// "no modal" while filling the screen. The card is matched against the whole
// active pane once its footer proves it reaches the bottom; a card whose footer
// has scrolled away is stale and must still be ignored.
func TestBlocksInputMatchesTallRequestUserInputCard(t *testing.T) {
	s := newTestServer(t)

	card := "  Question 1/2 (2 unanswered)\n" +
		strings.Repeat("  ▌ some long question body line\n", 40) +
		"  tab to add notes | enter to submit answer | esc to interrupt\n"

	onScreen := card + strings.Repeat("\n", 8)
	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(onScreen)})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(on-screen card): %v", err)
	}
	if !resp.BlocksInput {
		t.Error("BlocksInput = false for a request_user_input card taller than the modal window; the delivery gate would type into it")
	}

	scrolled := card + strings.Repeat("  wrote /tmp/codex-answer.txt\n", 40)
	respScrolled, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(scrolled)})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(scrolled card): %v", err)
	}
	if respScrolled.BlocksInput {
		t.Error("BlocksInput = true for a request_user_input card that has scrolled out of view; delivery to this chat would be refused forever")
	}
}

// TestTallCardEvidenceStaysScopedToTheWindow guards the two ways the tall-card
// path — the one branch allowed to read above the modal window — can be talked
// out of its answer by scrollback. Both directions are failures the window
// bound exists to prevent, so widening the header search must not drag the rest
// of the decision up with it.
func TestTallCardEvidenceStaysScopedToTheWindow(t *testing.T) {
	s := newTestServer(t)

	body := strings.Repeat("  ▌ some long question body line\n", 40)
	footer := "  tab to add notes | enter to submit answer | esc to interrupt\n"
	padding := strings.Repeat("\n", 6)

	tests := []struct {
		name string
		pane string
		want bool
		why  string
	}{
		{
			name: "stale working spinner above the card",
			// A spinner from an earlier turn stays in the buffer until
			// "• Session Complete", which a long chat may never print. Vetoing
			// pane-wide on it would switch the tall card's only defence off.
			pane: "• Working (7s • esc to interrupt)\n" +
				"  Question 1/2 (2 unanswered)\n" + body + footer + padding,
			want: true,
			why:  "a spinner from an earlier turn must not unblock a card that is on screen now",
		},
		{
			name: "working spinner inside the window below the footer",
			// The other half of the veto's job: a spinner drawn under the footer
			// is codex saying the picker is finished and the turn has resumed,
			// so the composer is free even though the card is still on screen.
			pane: "  Question 1/2 (2 unanswered)\n" + body + footer +
				"  wrote /tmp/codex-answer.txt\n" +
				"• Working (7s • esc to interrupt)\n" + padding,
			want: false,
			why:  "a spinner under the footer means the picker is done; blocking here refuses a ready pane",
		},
		{
			name: "card body quoting a header does not relocate the card",
			// Card bodies are free-form agent prose and routinely quote earlier
			// terminal output. A quoted header must not pose as the start of the
			// bottom-most card, or a live picker reads as unblocked.
			pane: "  Question 1/1 (1 unanswered)\n" + body +
				"  ▌ should I keep the state where Question 3/3 (0 unanswered) is shown?\n" +
				body + footer + padding,
			want: true,
			why:  "a header quoted inside a card body must not be mistaken for a real card header",
		},
		{
			name: "header rendered but its footer not yet drawn",
			// A capture can land between the header row and the footer row of a
			// re-render. The previous frame's footer is still on screen, so the
			// picker still owns the composer; treating the footerless newest
			// header as "no card" would fail open on that frame.
			pane: "  Question 1/2 (2 unanswered)\n" + body + footer +
				"  Question 2/2 (1 unanswered)\n" +
				strings.Repeat("  ▌ some long question body line\n", 3) + padding,
			want: true,
			why:  "a half-drawn re-render must not read as an idle composer",
		},
		{
			name: "answered bottom card under an unanswered older one",
			// The bottom card owns the composer; it is fully answered, so the
			// picker is done. An older card's counter must not vouch for it, or
			// delivery is refused for the rest of the session.
			pane: "  Question 1/3 (2 unanswered)\n" + body + footer +
				strings.Repeat("  wrote /tmp/codex-answer.txt\n", 50) +
				"  Question 3/3 (0 unanswered)\n" + body + footer + padding,
			want: false,
			why:  "an older card's unanswered counter must not keep a finished picker blocking",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(tc.pane)})
			if err != nil {
				t.Fatalf("HasQuestionPrompt: %v", err)
			}
			if resp.BlocksInput != tc.want {
				t.Errorf("BlocksInput = %v, want %v: %s", resp.BlocksInput, tc.want, tc.why)
			}
		})
	}
}

// TestLastTurnIsUserMissingTranscriptCollapsesToFalse asserts that a
// session ID with no rollout file on disk reports IsUser=false (no error).
// The daemon treats false as "don't suppress the question state", so
// errors-as-false is the correct, conservative collapse.
func TestLastTurnIsUserMissingTranscriptCollapsesToFalse(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	s := newTestServer(t)
	resp, err := s.LastTurnIsUser(context.Background(), &bossanovav1.LastTurnIsUserRequest{
		WorkDir:        "/anywhere",
		AgentSessionId: "uuid-without-transcript",
	})
	if err != nil {
		t.Fatalf("LastTurnIsUser: %v", err)
	}
	if resp.IsUser {
		t.Error("IsUser = true for missing transcript, want false")
	}
}

// TestTranscriptExistsRPCMatchesDiskState asserts the RPC delegates to
// transcriptExists correctly: missing → false, present → true.
func TestTranscriptExistsRPCMatchesDiskState(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	s := newTestServer(t)

	// Missing.
	resp, err := s.TranscriptExists(context.Background(), &bossanovav1.TranscriptExistsRequest{
		WorkDir:        "/anywhere",
		AgentSessionId: "019e068c-0000-7000-8000-00000000000c",
	})
	if err != nil {
		t.Fatalf("TranscriptExists(missing): %v", err)
	}
	if resp.Exists {
		t.Error("Exists = true for missing rollout, want false")
	}

	// Present.
	uuid := "019e068c-0000-7000-8000-00000000000b"
	dst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), uuid)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)
	respHave, err := s.TranscriptExists(context.Background(), &bossanovav1.TranscriptExistsRequest{
		WorkDir:        sampleRolloutCWD,
		AgentSessionId: uuid,
	})
	if err != nil {
		t.Fatalf("TranscriptExists(present): %v", err)
	}
	if !respHave.Exists {
		t.Error("Exists = false for non-empty rollout, want true")
	}
}

// TestGetChatTitleSupportedEvenWithoutTranscript asserts GetChatTitle
// always reports Supported=true (the codex plugin can ALWAYS attempt
// title extraction) and returns an empty Title when the transcript is
// missing. The daemon uses Supported to decide whether to fall back to
// a generic placeholder; an empty Title with Supported=true tells it
// "we tried, transcript not ready yet".
func TestGetChatTitleSupportedEvenWithoutTranscript(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir:   "/anywhere",
		SessionId: "uuid-without-transcript",
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if !resp.Supported {
		t.Error("Supported = false, want true (codex plugin always supports title extraction)")
	}
	if resp.Title != "" {
		t.Errorf("Title = %q, want empty for missing transcript", resp.Title)
	}
	if resp.Explicit {
		t.Error("Explicit = true, want false when there is no session index thread name to claim authority for")
	}
}

// TestGetChatTitle_SessionIndexThreadNameIsNotExplicit pins that a session
// index thread_name only backfills a placeholder title. Codex writes its
// auto-generated thread name there exactly as it writes a native `/rename`, so
// an explicit claim let the auto-name ("Verify PR 3069") replace the title the
// chat was created with ("verify").
func TestGetChatTitle_SessionIndexThreadNameIsNotExplicit(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	sessionID := "019f3a91-a6bb-7492-9173-976fa6354cc0"
	sessionsRoot := filepath.Join(codexHome, "sessions")
	writeSessionMetaRollout(t, sessionsRoot, sessionID, t.TempDir(), "codex-tui", time.Now())

	index := `{"id":"older","thread_name":"Ignore me","updated_at":"2026-07-07T03:10:00Z"}` + "\n" +
		`{"id":"` + sessionID + `","thread_name":"Rename Test","updated_at":"2026-07-07T03:14:20Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(index), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: "/anywhere", SessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if resp.Title != "Rename Test" {
		t.Errorf("Title = %q, want session index thread name", resp.Title)
	}
	if resp.Explicit {
		t.Error("Explicit = true, want false: a codex auto-name lands in session_index.jsonl the same way a /rename does")
	}
}

// TestGetChatTitle_SessionIndexLatestSameIDWins locks the "latest wins"
// scan: if the index ever carries more than one entry for a session id, the
// last non-empty thread_name is the one surfaced.
func TestGetChatTitle_SessionIndexLatestSameIDWins(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	sessionID := "019f3a91-a6bb-7492-9173-976fa6354cc0"
	sessionsRoot := filepath.Join(codexHome, "sessions")
	writeSessionMetaRollout(t, sessionsRoot, sessionID, t.TempDir(), "codex-tui", time.Now())

	index := `{"id":"` + sessionID + `","thread_name":"First Name","updated_at":"2026-07-07T03:10:00Z"}` + "\n" +
		`{"id":"` + sessionID + `","thread_name":"Latest Name","updated_at":"2026-07-07T03:14:20Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(index), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: "/anywhere", SessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if resp.Title != "Latest Name" {
		t.Errorf("Title = %q, want latest thread name", resp.Title)
	}
}

// TestGetChatTitle_EmptyThreadNameFallsBackToRollout verifies that an empty
// thread_name falls back to the rollout first-message.
func TestGetChatTitle_EmptyThreadNameFallsBackToRollout(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")

	sessionID := "019f3a91-a6bb-7492-9173-976fa6354cc0"
	dst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), sessionID)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

	index := `{"id":"` + sessionID + `","thread_name":"","updated_at":"2026-07-07T03:14:20Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(tmpHome, ".codex", "session_index.jsonl"), []byte(index), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: sampleRolloutCWD, SessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if resp.Title != "say hello and exit" {
		t.Errorf("Title = %q, want rollout first message", resp.Title)
	}
	if resp.Explicit {
		t.Error("Explicit = true, want false when thread_name is empty")
	}
}

// TestListIgnoredDirtyFilesReturnsEmptySlice asserts the codex plugin's
// ignoredDirtyFiles is empty (no .claude/settings.local.json equivalent),
// and that the response carries an empty (non-nil) slice — the daemon
// type-asserts on Paths length.
func TestListIgnoredDirtyFilesReturnsEmptySlice(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ListIgnoredDirtyFiles(context.Background(), &bossanovav1.ListIgnoredDirtyFilesRequest{
		WorkDir: "/anywhere",
	})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}
	if len(resp.Paths) != 0 {
		t.Errorf("Paths = %v, want empty", resp.Paths)
	}
}

// TestHasQuestionPromptReportsReplyChoiceWithoutBlocking pins the pair the
// daemon actually reads for BOS-1180's pane: the poller raises QUESTION off
// HasPrompt while the delivery gate keeps typing into the live composer off
// BlocksInput. Asserting them together at the RPC boundary is what stops a
// future edit satisfying one and breaking the other.
func TestHasQuestionPromptReportsReplyChoiceWithoutBlocking(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/reply_choice.txt")
	if err != nil {
		t.Fatalf("read reply-choice pane fixture: %v", err)
	}
	s := newTestServer(t)
	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: data})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(reply-choice): %v", err)
	}
	if !resp.HasPrompt {
		t.Error("HasPrompt = false for the reply-choice pane, want true")
	}
	if resp.BlocksInput {
		t.Error("BlocksInput = true for the reply-choice pane, want false: the composer is live")
	}
}

func newGeneratedTestClient(t *testing.T) bossanovav1.AgentRunnerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	bossanovav1.RegisterAgentRunnerServiceServer(srv, newServer(nil, zerolog.Nop()))
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.Stop(); _ = lis.Close() })
	return bossanovav1.NewAgentRunnerServiceClient(conn)
}
