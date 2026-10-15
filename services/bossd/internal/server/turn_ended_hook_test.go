package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

func startTurnEndedHookServer(t *testing.T, tracker *status.Tracker) string {
	t.Helper()
	hs := NewHookServer(HookServerConfig{PromptReceipts: &fakePromptReceipts{}, BackgroundAgents: tracker, Logger: zerolog.Nop()})
	if err := hs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = hs.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	})
	return fmt.Sprintf("http://127.0.0.1:%d", hs.Port())
}

func postHook(t *testing.T, url, auth, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestHookServer_TurnEndedRecordsBackgroundSubagents drives Claude's Stop
// payload through the turn-ended route: running subagents are recorded on the
// tracker the poller reads, shells and monitors are not (the pane shows them),
// a payload naming another chat or a bad token records nothing, and the next
// prompt clears the report.
func TestHookServer_TurnEndedRecordsBackgroundSubagents(t *testing.T) {
	tracker := status.NewTracker()
	base := startTurnEndedHookServer(t, tracker)
	turnEnded := func(id string) string { return base + "/hooks/turn-ended/" + id }
	stop := func(session, tasks string) string {
		return `{"session_id":"` + session + `","hook_event_name":"Stop","background_tasks":[` + tasks + `]}`
	}

	if got := postHook(t, turnEnded("agent-1"), "Bearer "+promptToken("agent-1"), stop("agent-1", `{"type":"shell"},{"type":"monitor"}`)); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if tracker.BackgroundAgentsRunning("agent-1") {
		t.Fatal("shells and monitors were recorded as background subagents")
	}

	for _, tc := range []struct{ name, auth, body string }{
		{"another chat's payload", "Bearer " + promptToken("agent-1"), stop("sibling", `{"type":"subagent"}`)},
		{"another chat's token", "Bearer " + promptToken("agent-2"), stop("agent-1", `{"type":"subagent"}`)},
		{"no token", "", stop("agent-1", `{"type":"subagent"}`)},
	} {
		postHook(t, turnEnded("agent-1"), tc.auth, tc.body)
		if tracker.BackgroundAgentsRunning("agent-1") {
			t.Fatalf("%s recorded a background subagent", tc.name)
		}
	}

	if got := postHook(t, turnEnded("agent-1"), "Bearer "+promptToken("agent-1"), stop("agent-1", `{"type":"subagent","agent_type":"Explore"},{"type":"shell"}`)); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if !tracker.BackgroundAgentsRunning("agent-1") {
		t.Fatal("a running background subagent was not recorded")
	}

	if got := postHook(t, base+"/hooks/prompt-submitted/agent-1", "Bearer "+promptToken("agent-1"), `{"session_id":"agent-1"}`); got != http.StatusOK {
		t.Fatalf("prompt-submitted status = %d, want 200", got)
	}
	if tracker.BackgroundAgentsRunning("agent-1") {
		t.Fatal("a new prompt left the last turn's background report standing")
	}
}

func TestHookServer_TurnEndedRejectsAMissingToken(t *testing.T) {
	base := startTurnEndedHookServer(t, status.NewTracker())
	if got := postHook(t, base+"/hooks/turn-ended/agent-1", "", `{}`); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
}
