package testharness_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/statusdetect"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/resume"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/tmux"
)

// transientBannerPane is the scripted pane the fake tmux serves for every
// capture-pane in this test: a turn that started work and died on a retryable
// 5xx banner, with the composer glyph beneath it. It carries three loads at
// once — statusdetect must read it as a transient API error, the status poller
// must read the chat as IDLE off it, and the tmux submit-verifier must read its
// empty "❯" row as a cleared composer — so the pane text is deliberately shaped
// like a real capture rather than a minimal fixture.
const transientBannerPane = "⏺ Running the build...\n\n  API Error: 502 Bad Gateway\n\n❯\n"

// TestTransientAPIErrorAutoResumeDeliversExactlyOnce is the BOS-518 end-to-end
// gate: a pane showing a transient 5xx banner produces exactly ONE resume
// prompt, delivered through the real Server.SendChatMessage → tmux path.
//
// "Exactly one" is the whole point. The lane is a loop by construction — the
// marker stays set until the pane changes, and the resumer re-arms after every
// attempt — so the failure mode that matters is not "never resumes" but
// "resumes forever", spraying prompts into a wedged chat. The assertions
// therefore bracket the count on BOTH sides: wait for the first delivery, then
// stay quiet and prove no second one lands.
//
// The pieces the daemon wires in cmd/main.go are assembled by hand here
// (poller → tracker hook → resumer → SendChatMessage) because the harness runs
// the server without a status poller; TestTransientResumeSeamsWired in
// services/bossd/cmd covers the wiring itself.
func TestTransientAPIErrorAutoResumeDeliversExactlyOnce(t *testing.T) {
	// (a) Precondition: the fixture is what the detector actually fires on. If
	// this ever drifts the rest of the test would pass vacuously (no marker, no
	// hook, no delivery — and "no second delivery" would be trivially true), so
	// pin it first.
	if !statusdetect.IsTransientAPIError([]byte(transientBannerPane)) {
		t.Fatalf("fixture is not detected as a transient API error:\n%s", transientBannerPane)
	}

	// The shared CronReadyTmuxFake is extended with CapturePaneOutput rather
	// than defining a per-test fake: this test needs the fake's has-session
	// bookkeeping (the poller's Bootstrap skips a chat whose tmux session is not
	// alive) and its Calls() recorder (to assert the send-keys actually fired),
	// both of which a fresh minimal fake would have to re-implement. The
	// override is opt-in and the zero value keeps every existing caller on the
	// original canned pane.
	fake := testharness.NewCronReadyTmuxFake()
	fake.CapturePaneOutput = transientBannerPane

	h := testharness.NewWithOptions(t, testharness.Options{TmuxCommandFactory: fake.Factory()})
	ctx := h.Ctx()

	repoID := h.SeedRepo(t, "https://github.com/test/transient-resume.git")
	sessionID := h.SeedSession(t, repoID, 518, bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN)

	const agentSessionID = "transient-resume-agent"
	tmuxName := "boss-test-transient-resume"
	if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: agentSessionID,
		AgentName:      "claude",
		Title:          "Transient resume e2e",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if err := h.AgentChats.UpdateTmuxSessionName(ctx, agentSessionID, &tmuxName); err != nil {
		t.Fatalf("set tmux session name: %v", err)
	}
	// Marks the session live in the fake, which is what makes the poller's
	// has-session probe (and SendChatMessage's liveness check, which would
	// otherwise take the wake path) succeed.
	if err := h.Tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    tmuxName,
		WorkDir: t.TempDir(),
		Command: []string{"true"},
	}); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	t.Cleanup(func() { _ = h.Tmux.KillSession(context.Background(), tmuxName) })

	var mu sync.Mutex
	var delivered []string
	resumer := resume.NewTransientResumer(resume.TransientResumerDeps{
		Logger:     zerolog.Nop(),
		MarkerSet:  h.ChatTracker.TransientAPIError,
		AuthFailed: h.ChatTracker.AuthFailed,
		ChatState: func(id string) (bossanovav1.ChatStatus, time.Time, bool) {
			entry := h.ChatTracker.Get(id)
			if entry == nil {
				return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED, time.Time{}, false
			}
			return entry.Status, entry.LastOutputAt, true
		},
		SessionResumable: func(ctx context.Context, id string) bool {
			chat, err := h.AgentChats.GetByAgentSessionID(ctx, id)
			if err != nil || chat == nil {
				return false
			}
			sess, err := h.Sessions.Get(ctx, chat.SessionID)
			if err != nil || sess == nil {
				return false
			}
			return sess.ArchivedAt == nil &&
				sess.State != machine.Merged &&
				sess.State != machine.Closed &&
				sess.State != machine.Orphaned
		},
		Deliver: func(ctx context.Context, id, message string) error {
			mu.Lock()
			delivered = append(delivered, message)
			mu.Unlock()
			_, err := h.Server.SendChatMessage(ctx, connect.NewRequest(&bossanovav1.SendChatMessageRequest{
				AgentSessionId: id,
				Message:        message,
				Submit:         true,
				WakeIfAsleep:   true,
			}))
			return err
		},
		// The settle window is compressed so the test runs in milliseconds; the
		// backoff base is left at its production default (1m) ON PURPOSE. That is
		// what makes "exactly one" a real assertion instead of a race: the second
		// attempt is scheduled a minute out, far beyond the quiet period below, so
		// a count of 2 can only mean a genuine double-arm bug.
		SettleWindow: 75 * time.Millisecond,
	})
	// Wired BEFORE Bootstrap: the marker is set during the bootstrap capture, and
	// the tracker fires this hook only on the transition, so a hook installed
	// afterwards would never see the edge at all.
	h.ChatTracker.SetOnTransientAPIErrorChange(resumer.OnTransientAPIError)

	poller := status.NewTmuxStatusPoller(h.ChatTracker, h.AgentChats, h.Sessions, h.Tmux, nil, zerolog.Nop())
	// Bootstrap alone is sufficient and the 3s Run loop is deliberately not
	// started: a single bootstrap pass sets the marker, fires the hook, and seeds
	// IDLE with LastOutputAt already older than the settle window — everything
	// the resumer gates on — at zero wall-clock cost. Passing nil agentClients
	// leaves the working/question/limit probes unregistered, so they fail open
	// and the chat lands on IDLE, matching a real pane parked at its prompt.
	poller.Bootstrap(ctx)

	// (b) The marker is the resumer's trigger AND its live re-check at fire time;
	// if bootstrap did not set it, nothing downstream can be trusted.
	if !h.ChatTracker.TransientAPIError(agentSessionID) {
		t.Fatal("expected the transient-API-error marker to be set after bootstrap")
	}
	if entry := h.ChatTracker.Get(agentSessionID); entry == nil || entry.Status != bossanovav1.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("expected bootstrap to seed IDLE, got %v", entry)
	}

	// (c) Exactly one delivery, carrying the verbatim resume prompt.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no auto-resume delivery within 2s of the transient banner")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Quiet period: long enough to catch a cycle that re-armed off the settle
	// window (75ms) instead of the backoff base, which is the realistic
	// double-delivery bug.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("auto-resume deliveries = %d, want exactly 1: %q", len(got), got)
	}
	if got[0] != resume.ResumeMessage {
		t.Fatalf("delivered message = %q, want %q", got[0], resume.ResumeMessage)
	}

	// (d) The delivery reached tmux for real rather than stopping at the seam.
	// A single-line payload is typed with send-keys -l (not paste-buffer), and
	// the submit-verifier is satisfied by the fixture's empty "❯" row, so this
	// is deterministic — no fighting the verifier.
	var sawPayload bool
	for _, call := range fake.Calls() {
		if len(call) == 0 || call[0] != "send-keys" {
			continue
		}
		if strings.Contains(strings.Join(call, " "), resume.ResumeMessage) {
			sawPayload = true
			break
		}
	}
	if !sawPayload {
		t.Fatalf("expected a tmux send-keys carrying the resume prompt; calls: %v", fake.Calls())
	}
}
