package testharness_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/statusdetect"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/resume"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/tmux"
)

// staleFooterBannerPane is the pane shape the BOS-889 investigation actually
// captured: a turn handed work to background agents, drew the
// "Waiting for N background agents to finish" footer, and then DIED. Nothing
// ever repainted the footer, so it is frozen in the transcript with the API
// error banner rendered below it.
//
// That is the compound case BOS-890 has to survive. The banner arms the
// transient-resume lane; the footer, read naively, pins the chat in WORKING
// forever. A WORKING chat fails the resumer's idle gate, and a gate that keeps
// saying "not yet" burns through MaxGateRechecks and abandons the cycle — so
// the chat shows an error banner, is armed for auto-resume, and is never
// resumed. Three panes sat like this for hours.
const staleFooterBannerPane = "⏺ Handing three sub-tasks to background agents...\n\n✻ Waiting for 3 background agents to finish\n\n  API Error: 502 Bad Gateway\n\n❯\n"

// liveFooterPane is staleFooterBannerPane with the banner removed: the same
// footer, on a turn that is genuinely still waiting. It is the negative control
// for the test below — without it, a fixture that reads as idle proves nothing,
// because a detector that had simply stopped recognising the footer at all
// would pass just as happily while un-pinning real working chats.
const liveFooterPane = "⏺ Handing three sub-tasks to background agents...\n\n✻ Waiting for 3 background agents to finish\n"

// statusdetectAgentClient is the harness mock with the ONE probe this test cares
// about delegated to the real detector. The status poller reaches the working
// grammar over the HasWorkingIndicator RPC, and the harness mock answers "not
// working" unconditionally — which would make the whole test vacuous, since the
// chat would land on IDLE no matter how badly the footer rule regressed.
// Delegating to statusdetect.HasWorkingIndicator is exactly what the claude
// plugin's handler does, so the classification under test is the production one
// and only the RPC transport is stubbed.
type statusdetectAgentClient struct {
	*testharness.MockAgentClient
}

func (statusdetectAgentClient) HasWorkingIndicator(_ context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{
		IsWorking: statusdetect.HasWorkingIndicator(req.GetPaneContent()),
	}, nil
}

// TestStaleWorkingFooterBeneathABannerStillAutoResumes is the BOS-890 compound
// regression (acceptance criterion 6). BOS-889 taught the working detector to
// discount a background-agent footer that a dead turn left behind; BOS-518 owns
// the auto-resume lane. Each shipped with its own tests, and neither has one
// that fails if the two stop composing.
//
// This is that test. It pins the end-to-end behaviour the operator sees: a pane
// carrying BOTH the banner and the stale footer gets exactly one resume prompt,
// rather than the resumer politely re-checking an idle gate three times against
// a chat that is wrongly WORKING and then giving up on it forever.
func TestStaleWorkingFooterBeneathABannerStillAutoResumes(t *testing.T) {
	// (a) Preconditions. The fixture has to carry both loads at once, and each
	// half is what the other half's assertion depends on, so pin them before
	// anything can pass vacuously.
	if !statusdetect.IsTransientAPIError([]byte(staleFooterBannerPane)) {
		t.Fatalf("fixture no longer reads as a transient API error:\n%s", staleFooterBannerPane)
	}
	if statusdetect.HasWorkingIndicator([]byte(staleFooterBannerPane)) {
		t.Fatalf("fixture reads as WORKING; the footer below a banner is stale:\n%s", staleFooterBannerPane)
	}
	// The negative control: the same footer WITHOUT the banner must still pin
	// the chat as WORKING. This is what keeps the assertion above honest — it
	// fails if the footer rule is fixed by deleting it.
	if !statusdetect.HasWorkingIndicator([]byte(liveFooterPane)) {
		t.Fatalf("control fixture no longer reads as WORKING; the footer rule has been widened away:\n%s", liveFooterPane)
	}

	fake := testharness.NewCronReadyTmuxFake()
	fake.CapturePaneOutput = staleFooterBannerPane

	h := testharness.NewWithOptions(t, testharness.Options{TmuxCommandFactory: fake.Factory()})
	ctx := h.Ctx()

	repoID := h.SeedRepo(t, "https://github.com/test/stale-footer-resume.git")
	sessionID := h.SeedSession(t, repoID, 890, bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN)

	const agentSessionID = "stale-footer-agent"
	tmuxName := "boss-test-stale-footer-resume"
	if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: agentSessionID,
		AgentName:      "claude",
		Title:          "Stale footer resume e2e",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if err := h.AgentChats.UpdateTmuxSessionName(ctx, agentSessionID, &tmuxName); err != nil {
		t.Fatalf("set tmux session name: %v", err)
	}
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
		// Compressed so the whole gate ladder plays out in milliseconds. This
		// also sharpens the regression: with a 75ms settle window a wrongly-
		// WORKING chat exhausts all MaxGateRechecks re-checks in roughly 300ms
		// and abandons its cycle, well inside the wait below — so the timeout is
		// a real verdict, not a slow machine.
		SettleWindow: 75 * time.Millisecond,
	})
	h.ChatTracker.SetOnTransientAPIErrorChange(resumer.OnTransientAPIError)

	poller := status.NewTmuxStatusPoller(h.ChatTracker, h.AgentChats, h.Sessions, h.Tmux,
		map[string]agent.AgentRunnerClient{"claude": statusdetectAgentClient{&testharness.MockAgentClient{}}},
		zerolog.Nop())
	poller.Bootstrap(ctx)

	// (b) The compound state itself: armed AND idle. Either half alone is the
	// bug — no marker means nothing is armed, WORKING means the idle gate will
	// stall the cycle out.
	if !h.ChatTracker.TransientAPIError(agentSessionID) {
		t.Fatal("expected the transient-API-error marker to be set after bootstrap")
	}
	entry := h.ChatTracker.Get(agentSessionID)
	if entry == nil || entry.Status != bossanovav1.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("expected the stale footer to be discounted and the chat seeded IDLE, got %v", entry)
	}

	// (c) A resume prompt, delivered through the real send path, and no second one
	// within the observed window. See the window's exact scope below — it is
	// narrower than "exactly one, ever".
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no auto-resume within 2s: the cycle burned its gate re-checks on a chat pinned WORKING by a dead turn's footer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A second prompt inside this window would mean the cycle re-armed on the
	// SETTLE window and resumed the same chat again. Be precise about the scope,
	// because "exactly one" is a claim this window cannot support: only
	// SettleWindow is compressed here (75ms), while BackoffBase is left at its
	// package default, so the ladder's second rung is two MINUTES out and is not
	// in evidence either way. What 500ms — roughly 6 settle windows — does rule
	// out is the failure mode this test is about: a gate re-check path (which
	// re-arms at one settle window) firing a duplicate prompt, and the delivery
	// itself looping. The unit suite owns the ladder's own bound.
	//
	// Poll rather than sleep-then-look so a duplicate ends the wait at the instant
	// it appears and the assertion below reports it, instead of the test idling
	// for the rest of the window.
	quiet := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(quiet) {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("auto-resume deliveries = %d, want exactly 1 within the observed window: %q", len(got), got)
	}
	if got[0] != resume.ResumeMessage {
		t.Fatalf("delivered message = %q, want %q", got[0], resume.ResumeMessage)
	}
}
