package status

import (
	"context"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/rs/zerolog"
)

// idleOverBackgroundAgentPane is a Claude pane whose main loop finished its
// turn and sits at the prompt while a background subagent runs. The statusline
// reads "← 1 agent", which Claude also shows for a subagent that already
// failed, so the pane alone must read IDLE.
const idleOverBackgroundAgentPane = "" +
	"⏺ Dispatched the reviewer in the background; I'll pick up when it reports.\n" +
	"\n" +
	"✻ Worked for 2m 3s\n" +
	"────────────────────────────────────────\n" +
	"❯ \n" +
	"────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #7 · ← 1 agent\n"

// pollIdlePane runs one poll over a pane unchanged past IdleThreshold and
// returns the status the tracker recorded.
func pollIdlePane(t *testing.T, tracker *Tracker, agentSessionID string) pb.ChatStatus {
	t.Helper()
	tmuxName := "boss-" + agentSessionID
	chatStore := &mockChatStore{chats: map[string]*models.AgentChat{
		agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
	}}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: idleOverBackgroundAgentPane},
	}
	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmux.NewClient(tmux.WithCommandFactory(factory.factory)),
		map[string]agent.RunnerClient{"claude": &claudeFakeClient{}}, zerolog.Nop())
	poller.mu.Lock()
	poller.prevCaptures[agentSessionID] = captureEntry{content: idleOverBackgroundAgentPane, at: time.Now().Add(-2 * IdleThreshold)}
	poller.mu.Unlock()
	poller.pollOnce(context.Background())
	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("no entry after poll")
	}
	return entry.Status
}

// TestTmuxStatusPoller_BackgroundSubagentKeepsAnIdlePaneWorking pins (a): a
// chat whose main loop is idle at its prompt reads WORKING while its last turn
// reported a running background subagent, and IDLE again once a prompt clears
// the report. The "← 1 agent" statusline alone never makes it WORKING.
func TestTmuxStatusPoller_BackgroundSubagentKeepsAnIdlePaneWorking(t *testing.T) {
	tracker := NewTracker()
	const id = "claude-bg-agent"
	if got := pollIdlePane(t, tracker, id); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("pane alone = %v, want IDLE: the agent statusline is no evidence of live work", got)
	}

	tracker.SetBackgroundAgents(id, 1)
	if got := pollIdlePane(t, tracker, id); got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("with a running background subagent = %v, want WORKING", got)
	}
	if tracker.Stalled(id) {
		t.Fatal("a chat working through a background subagent was judged stalled by its finished main transcript")
	}

	tracker.SetBackgroundAgents(id, 0)
	if got := pollIdlePane(t, tracker, id); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("after the report cleared = %v, want IDLE", got)
	}
}

func TestTracker_BackgroundAgentsReportAgesOutAndClearsOnRemove(t *testing.T) {
	tracker := NewTracker()
	tracker.SetBackgroundAgents("a", 2)
	if !tracker.BackgroundAgentsRunning("a") {
		t.Fatal("fresh report not running")
	}
	tracker.mu.Lock()
	tracker.backgroundAgents["a"] = backgroundAgentsReport{count: 2, at: time.Now().Add(-BackgroundAgentsMaxAge - time.Minute)}
	tracker.mu.Unlock()
	if tracker.BackgroundAgentsRunning("a") {
		t.Fatal("a report older than BackgroundAgentsMaxAge still pins the chat working")
	}
	tracker.SetBackgroundAgents("b", 1)
	tracker.Remove("b")
	if tracker.BackgroundAgentsRunning("b") {
		t.Fatal("Remove left the background-agent report behind")
	}
}
