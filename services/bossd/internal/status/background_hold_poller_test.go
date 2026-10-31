package status

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/plugin/statusdetect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func newBackgroundHoldPoller(t *testing.T, pane string) (*TmuxStatusPoller, *Tracker) {
	t.Helper()
	const id = "background-hold-chat"
	tmuxName := "boss-" + id
	tracker := NewTracker()
	store := &mockChatStore{chats: map[string]*domain.AgentChat{id: {AgentSessionID: id, AgentName: "claude", TmuxSessionName: &tmuxName}}}
	factory := &mockTmuxFactory{sessions: map[string]bool{tmuxName: true}, captures: map[string]string{tmuxName: pane}}
	poller := NewTmuxStatusPoller(tracker, store, nil, tmux.NewClient(tmux.WithCommandFactory(factory.factory)), map[string]agent.RunnerClient{"claude": &claudeFakeClient{}}, zerolog.Nop())
	return poller, tracker
}

func finishedBackgroundShellPane(t *testing.T) string {
	t.Helper()
	// Like migrationsDir, anchor at the source file for both native package
	// working directories and Bazel's repository-root rundir.
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate background pane fixture")
	}
	pane, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../../../lib/bossalib/contract/plugin/statusdetect/testdata/claude_turn_done_background_shell_plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(pane)
}

func TestTmuxStatusPoller_ChatPicker_AC23_ChatPicker_AC24_BackgroundHoldBoundary(t *testing.T) {
	pane := finishedBackgroundShellPane(t)
	poller, tracker := newBackgroundHoldPoller(t, pane)
	const id = "background-hold-chat"
	for _, age := range []time.Duration{time.Minute, BackgroundWorkHoldMax + time.Second} {
		changed := time.Now().Add(-age)
		normalized, _ := statusdetect.NormalizeSpinner([]byte(pane))
		poller.prevCaptures[id] = captureEntry{content: pane, at: changed, normalized: string(normalized), substantiveAt: changed}
		poller.pollOnce(context.Background())
		entry := tracker.Get(id)
		until := tracker.BackgroundHold(id)
		if age < BackgroundWorkHoldMax {
			if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING || !until.Equal(changed.Add(BackgroundWorkHoldMax)) {
				t.Fatalf("fresh footer: %+v, hold %v", entry, until)
			}
		} else if entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE || !until.IsZero() {
			t.Fatalf("expired footer: %+v, hold %v", entry, until)
		}
		if tracker.Stalled(id) {
			t.Fatal("finished main turn judged stalled")
		}
	}
}

func TestTmuxStatusPoller_ChatPicker_AC24_LiveWaitHasNoHold(t *testing.T) {
	for _, pane := range []string{"✻ Thinking… (1m 0s · ↓ 3 tokens)", "✻ Waiting for 1 background agent to finish"} {
		poller, tracker := newBackgroundHoldPoller(t, pane)
		const id = "background-hold-chat"
		changed := time.Now().Add(-5 * time.Hour)
		poller.prevCaptures[id] = captureEntry{content: pane, at: changed}
		poller.pollOnce(context.Background())
		if entry := tracker.Get(id); entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
			t.Fatalf("live wait %q: %+v", pane, entry)
		}
		if !tracker.BackgroundHold(id).IsZero() {
			t.Fatalf("live wait %q background-held", pane)
		}
	}
}

func TestTmuxStatusPoller_BackgroundHoldBootstrap(t *testing.T) {
	for _, report := range []bool{false, true} {
		pane := finishedBackgroundShellPane(t)
		if report {
			pane = idleOverBackgroundAgentPane
		}
		poller, tracker := newBackgroundHoldPoller(t, pane)
		const id = "background-hold-chat"
		if report {
			tracker.SetBackgroundAgents(id, 1)
		}
		poller.Bootstrap(context.Background())
		first := tracker.Get(id)
		hold := tracker.BackgroundHold(id)
		if hold.IsZero() || first.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
			t.Fatalf("bootstrap: %+v, %v", first, hold)
		}
		if report && !hold.Equal(tracker.backgroundAgentsUntil(id)) {
			t.Fatal("report deadline changed")
		}
		poller.pollOnce(context.Background())
		if tracker.Get(id).Status != first.Status || !tracker.BackgroundHold(id).Equal(hold) {
			t.Fatal("bootstrap and poll disagree for same pane age")
		}
	}
}
