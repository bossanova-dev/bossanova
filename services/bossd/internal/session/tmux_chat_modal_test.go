package session

// Session-start half of the BOS-894 gate. The plugin's tests prove the codex
// grammar recognises the boot interstitial and internal/tmux's prove the four
// …WithModal wrappers refuse on it; these prove the wiring in between — that a
// session start, from either of its two delivery call sites, actually asks the
// chat's own runner client and actually refuses.
//
// The fixture is the REAL captured pane, mirrored from the codex plugin. It is
// the whole point of the exercise: a hand-written approximation of an update
// screen would pass these tests while proving nothing about the screen that
// killed a chat.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// codexUpdateInterstitialDigest pins the mirrored fixture to the plugin's copy.
//
// TRIPWIRE. services/bossd must not import a plugin module (see CLAUDE.md
// § Conventions), so the pane is COPIED from
// plugins/bossd-plugin-codex/testdata/panes/update_interstitial.txt rather than
// shared. A copy silently drifts; this digest makes it fail loudly instead.
// plugins/bossd-plugin-codex/question_boot_interstitial_test.go pins the same
// bytes from the other side, so a re-capture that updates only one copy breaks
// both packages' tests and names the file to re-copy.
const codexUpdateInterstitialDigest = "422ea7fabae5dc0ff1de6afc5d9d97e903301692d256b65849918c8f437e9e3f"

// readCodexUpdateInterstitial returns the mirrored pane, verifying the digest.
//
// The path is resolved from this source file rather than the working directory:
// the Bazel go_test for this package runs with rundir = ".", so a relative
// "testdata/..." open resolves against the runfiles root and not the package.
func readCodexUpdateInterstitial(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the testdata directory")
	}
	path := filepath.Join(filepath.Dir(thisFile), "testdata", "panes", "codex_update_interstitial.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != codexUpdateInterstitialDigest {
		t.Fatalf("fixture %s digest = %s, want %s; the codex plugin's "+
			"testdata/panes/update_interstitial.txt was re-captured — re-copy it here "+
			"and update codexUpdateInterstitialDigest", path, got, codexUpdateInterstitialDigest)
	}
	return string(raw)
}

// codexInterstitialMarker is the composer glyph codex draws on row 1 of the
// interstitial's menu ("› 1. Update now"). Configuring it as the ready marker is
// not a contrivance: it is codex's real marker, and the fact that the menu row
// carries it is precisely why row-anchored composer resolution alone accepts
// this pane. Every test below therefore starts from a gate that WOULD have
// delivered.
const codexInterstitialMarker = "›"

// newInterstitialHarness returns a harness whose pane renders the captured
// interstitial and whose runner client answers the modal probe with blocks.
func newInterstitialHarness(t *testing.T, blocks bool, probeErr error) *startTmuxChatHarness {
	t.Helper()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.capturePaneOutput = readCodexUpdateInterstitial(t)
	h.agentFake.ReadyMarker = codexInterstitialMarker
	h.agentFake.HasQuestionPromptFunc = func(*bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
		if probeErr != nil {
			return nil, probeErr
		}
		return &bossanovav1.HasQuestionPromptResponse{BlocksInput: blocks}, nil
	}
	return h
}

// assertRefusal checks the error carries the sentinel and the outcome, so a
// refusal cannot be mistaken for an ordinary send failure by callers that
// classify on either.
func assertRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("delivery error = nil, want a modal refusal")
	}
	// Logged, not merely asserted: the plan's primary proof artifact for this
	// change is the run output, and a passing test that prints nothing proves
	// only that nobody looked.
	t.Logf("BLOCKED_BY_MODAL: %v", err)
	if !errors.Is(err, tmux.ErrBlockedByModal) {
		t.Fatalf("errors.Is(err, ErrBlockedByModal) = false; err = %v", err)
	}
	if got := tmux.OutcomeOf(err); got != tmux.OutcomeBlockedByModal {
		t.Fatalf("OutcomeOf(err) = %v, want OutcomeBlockedByModal; err = %v", got, err)
	}
}

// subcommands lists the tmux subcommands recorded so far, newest last. Defined
// here rather than beside fakeTmux to keep this change additive: it exists only
// to put the argv into the run output as evidence.
func (f *fakeTmux) subcommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.subcommand)
	}
	return out
}

// assertNothingDelivered is the assertion the incident is actually about. The
// sentinel says we refused; only the recorded argv says nothing reached the
// pane. "Update now" is selected by an Enter, so an Enter send-keys — or a
// paste-buffer load — after a refusal would mean the gate reported one thing
// and did another.
func assertNothingDelivered(t *testing.T, h *startTmuxChatHarness) {
	t.Helper()
	t.Logf("recorded tmux subcommands after refusal: %v", h.tmuxFake.subcommands())
	if got := h.tmuxFake.enterSendKeysCount(); got != 0 {
		t.Errorf("refused delivery still sent %d Enter send-keys; calls=%v", got, h.tmuxFake.calls)
	}
	for _, sub := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if h.tmuxFake.hasSubcommand(sub) {
			t.Errorf("refused delivery still ran tmux %s; calls=%v", sub, h.tmuxFake.calls)
		}
	}
}

// TestStartTmuxChat_FreshStartRefusesBootInterstitial covers the fresh-start
// call site: a pane spawned for a brand-new chat that opens on the interstitial
// instead of a composer. Both delivery intents and both input kinds are driven,
// because the four wrappers are four separate code paths and a binding that
// missed one would leave that arm delivering into the menu.
func TestStartTmuxChat_FreshStartRefusesBootInterstitial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, tc := range []struct {
		name  string
		input ChatInput
	}{
		{"command/submit", ChatInput{Command: "boss-repair", Delivery: DeliverySubmit}},
		{"command/prefill", ChatInput{Command: "boss-repair", Delivery: DeliveryPrefillOnly}},
		{"prompt/submit", ChatInput{Prompt: "do the thing", Delivery: DeliverySubmit}},
		{"prompt/prefill", ChatInput{Prompt: "do the thing", Delivery: DeliveryPrefillOnly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newInterstitialHarness(t, true, nil)
			_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", tc.input, "T", HookOpts{})
			assertRefusal(t, err)
			assertNothingDelivered(t, h)
		})
	}
}

// TestStartTmuxChat_ResumeRefusesBootInterstitial covers the other call site:
// resuming into a pane that is already alive. It is a distinct binding —
// sendInputToLiveTmuxChat passes its own client and the chat row's agent name —
// so proving the fresh-start path proves nothing about it.
//
// This is also the likelier shape in practice: a long-lived pane that has sat
// idle is exactly where codex's periodic update check fires, so the interstitial
// can appear on a pane that was a healthy composer when the chat was created.
func TestStartTmuxChat_ResumeRefusesBootInterstitial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, tc := range []struct {
		name  string
		input ChatInput
	}{
		{"command/submit", ChatInput{Command: "boss-repair", Delivery: DeliverySubmit}},
		{"command/prefill", ChatInput{Command: "boss-repair", Delivery: DeliveryPrefillOnly}},
		{"prompt/submit", ChatInput{Prompt: "do the thing", Delivery: DeliverySubmit}},
		{"prompt/prefill", ChatInput{Prompt: "do the thing", Delivery: DeliveryPrefillOnly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newInterstitialHarness(t, true, nil)
			const agentSessionID = "agent-session-prior"
			tmuxName := tmux.ChatSessionName("repo-abcdef12", agentSessionID)
			h.chats.chatsBySession = map[string][]*models.AgentChat{
				"sess-1": {{
					ID:              "chat-prior",
					SessionID:       "sess-1",
					AgentSessionID:  agentSessionID,
					AgentName:       "codex",
					TmuxSessionName: &tmuxName,
				}},
			}
			input := tc.input
			input.ResumeAgentSessionID = agentSessionID

			_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", input, "T", HookOpts{})
			assertRefusal(t, err)
			assertNothingDelivered(t, h)
			if h.tmuxFake.hasSubcommand("new-session") {
				t.Errorf("resume refusal must not respawn the pane; calls=%v", h.tmuxFake.calls)
			}
		})
	}
}

// TestStartTmuxChat_RefusalRecordsStartError pins what the operator sees. A
// refusal is not a crash: the chat row survives with the reason on it, and the
// message names the pane text so the screen can be judged after it has moved on.
func TestStartTmuxChat_RefusalRecordsStartError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newInterstitialHarness(t, true, nil)
	_, err := h.lc.StartTmuxChat(context.Background(), "sess-1",
		ChatInput{Prompt: "do the thing", Delivery: DeliverySubmit}, "T", HookOpts{})
	assertRefusal(t, err)
	if !strings.Contains(err.Error(), "Update available!") {
		t.Errorf("refusal must embed the offending pane; err = %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was typed") {
		t.Errorf("refusal must state what did not happen; err = %v", err)
	}
}

// TestStartTmuxChat_ProbeFailureDeliversAsBefore is the fail-open half. A runner
// client that cannot answer must not become a new way for session start to fail:
// the gate degrades to its pre-BOS-600 behaviour and the payload is delivered.
//
// The pane here is the interstitial precisely because that is the worst case —
// the screen the gate exists to refuse — and it still delivers. That is the
// deliberate trade recorded in ModalDetector's contract: an agent whose plugin
// is momentarily unreachable keeps working exactly as it did before this gate.
func TestStartTmuxChat_ProbeFailureDeliversAsBefore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newInterstitialHarness(t, false, errors.New("plugin unreachable"))
	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1",
		ChatInput{Prompt: "do the thing", Delivery: DeliverySubmit}, "T", HookOpts{}); err != nil {
		t.Fatalf("a failing modal probe must fail open, got error: %v", err)
	}
	if got := h.tmuxFake.enterSendKeysCount(); got == 0 {
		t.Error("fail-open delivery must still press Enter on a submit")
	}
}

// TestStartTmuxChat_ProbeFailureIsReportedOnce pins that a degraded gate is
// SAID OUT LOUD. Failing open is only defensible if the operator can find out
// it happened, and the swallow point is deliberate: internal/tmux drops the
// detector's error to keep itself free of a logging dependency, so if the
// constructor here did not log it, a session that delivered ungated would be
// indistinguishable in the logs from one that passed the gate cleanly.
//
// Exactly one, not at-least-one, because the volume ceiling is the other half:
// the probe sits inside a readiness poll loop. A single start currently probes
// once (waitForReadyMarker asks on the capture that resolved a composer row, or
// once more at the deadline), so the sync.Once in the constructor is what keeps
// that true for a start that probes repeatedly — proven directly, over three
// failing probes, by TestNewModalPaneCheckerReportsFailureOnce in internal/agent.
// What this test adds is that the logger reaching that constructor is the
// Lifecycle's own, with the chat's agent named on the event.
func TestStartTmuxChat_ProbeFailureIsReportedOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	var logs strings.Builder
	var probeCalls atomic.Int64
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.capturePaneOutput = readCodexUpdateInterstitial(t)
	h.agentFake.ReadyMarker = codexInterstitialMarker
	h.agentFake.HasQuestionPromptFunc = func(*bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
		probeCalls.Add(1)
		return nil, errors.New("plugin unreachable")
	}
	h.lc.logger = zerolog.New(&logs)

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1",
		ChatInput{Prompt: "do the thing", Delivery: DeliverySubmit}, "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	// Without this the fail-open assertion above would also pass on a build that
	// never consulted the gate at all.
	if got := probeCalls.Load(); got == 0 {
		t.Fatal("the gate was never consulted; the fail-open path proves nothing")
	}
	if got := strings.Count(logs.String(), "modal check unavailable"); got != 1 {
		t.Fatalf("degraded-gate warnings = %d, want exactly 1; logs:\n%s", got, logs.String())
	}
	// "claude" is the harness's agent name, not a claim about whose pane this
	// is: the assertion is that the warning carries the agent it probed, and
	// the codex-shaped pane above is incidental to that.
	if !strings.Contains(logs.String(), `"agent":"claude"`) {
		t.Errorf("degraded-gate warning must name the agent; logs:\n%s", logs.String())
	}
}
