package server

import (
	"context"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
)

// updateChatTitleStoreFake is a minimal db.AgentChatStore that overrides only
// UpdateTitleByAgentSessionID — the single store call UpdateChatTitle makes. It
// keeps the stored title so a test can start from a non-placeholder name and
// observe the overwrite, and records the last (agentSessionID, title) pair plus
// a call count so the negative cases can assert the store was never reached.
type updateChatTitleStoreFake struct {
	db.AgentChatStore
	mu                 sync.Mutex
	title              string
	calls              int
	lastAgentSessionID string
	lastTitle          string
}

func (f *updateChatTitleStoreFake) UpdateTitleByAgentSessionID(_ context.Context, agentSessionID string, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastAgentSessionID = agentSessionID
	f.lastTitle = title
	f.title = title
	return nil
}

func (f *updateChatTitleStoreFake) snapshot() (calls int, agentSessionID, title, stored string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastAgentSessionID, f.lastTitle, f.title
}

// TestUpdateChatTitle_ExplicitWriteOverwritesExistingTitle pins writer 1 of the
// five chat-title writers (BOS-839): the UpdateChatTitle RPC is **deliberately
// unguarded**. It validates only that the id and title are non-empty and then
// writes straight through to the store, with no placeholder check and no
// comparison against the title already recorded.
//
// That is intentional, not an oversight. Every *explicit* rename surface routes
// through this one handler — the TUI `r` shortcut, `boss chat rename`, and the
// MCP `update_chat_title` tool — so each of them is a user (or an agent acting
// on the user's behalf) naming the chat on purpose. The chosen rule is
// last-explicit-write-wins: a deliberate rename always lands, even over a name
// the user set moments earlier. Adding a precedence guard here was considered
// and explicitly rejected in favour of that simpler rule.
//
// The guarded writers are elsewhere and are pinned separately: the poller's
// auto-derived title (`shouldRefreshChatTitle` in
// services/bossd/internal/status/tmux_poller.go) and the TUI's client-side
// backfill (`backfillTitles` in services/boss/internal/views/chatpicker_cmds.go)
// both refuse to clobber a non-placeholder title. If a future change adds a
// guard *here*, it breaks every explicit rename surface at once — this test is
// what catches that.
func TestUpdateChatTitle_ExplicitWriteOverwritesExistingTitle(t *testing.T) {
	const (
		agentSessionID = "claude-abc-123"
		newTitle       = "the new name"
	)

	// The chat already carries a real, user-visible name — not a placeholder.
	// A guarded writer would leave this alone; this one must not.
	store := &updateChatTitleStoreFake{title: "Manual investigation"}
	srv := &Server{agentChats: store}

	resp, err := srv.UpdateChatTitle(context.Background(), connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: agentSessionID,
		Title:          newTitle,
	}))
	if err != nil {
		t.Fatalf("UpdateChatTitle returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("UpdateChatTitle returned a nil response")
	}

	calls, gotAgentSessionID, gotTitle, stored := store.snapshot()
	if calls != 1 {
		t.Fatalf("store calls = %d, want exactly 1 (the handler writes through unconditionally)", calls)
	}
	if gotAgentSessionID != agentSessionID {
		t.Errorf("store agent_session_id = %q, want %q", gotAgentSessionID, agentSessionID)
	}
	if gotTitle != newTitle {
		t.Errorf("store title = %q, want %q", gotTitle, newTitle)
	}
	if stored != newTitle {
		t.Errorf("stored title = %q, want %q — the non-placeholder title must be overwritten", stored, newTitle)
	}
}

// TestUpdateChatTitle_RejectsEmptyArguments is the falsification half of the
// test above. Without it, a handler that rejected *every* request — or one that
// wrote nothing at all — would still satisfy an "overwrites the title" assertion
// only by accident of how the fake is read. Here the two validation paths must
// each return CodeInvalidArgument and reach the store zero times, which proves
// the store call in the test above was caused by a request the handler actually
// accepted rather than by unconditional fake behaviour.
func TestUpdateChatTitle_RejectsEmptyArguments(t *testing.T) {
	tests := []struct {
		name           string
		agentSessionID string
		title          string
	}{
		{
			name:           "empty agent_session_id",
			agentSessionID: "",
			title:          "the new name",
		},
		{
			name:           "empty title",
			agentSessionID: "claude-abc-123",
			title:          "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &updateChatTitleStoreFake{title: "Manual investigation"}
			srv := &Server{agentChats: store}

			_, err := srv.UpdateChatTitle(context.Background(), connect.NewRequest(&pb.UpdateChatTitleRequest{
				AgentSessionId: tt.agentSessionID,
				Title:          tt.title,
			}))
			if err == nil {
				t.Fatal("UpdateChatTitle returned nil error, want CodeInvalidArgument")
			}
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Errorf("error code = %v, want %v", got, connect.CodeInvalidArgument)
			}

			calls, _, _, stored := store.snapshot()
			if calls != 0 {
				t.Errorf("store calls = %d, want 0 — validation must reject before touching the store", calls)
			}
			if stored != "Manual investigation" {
				t.Errorf("stored title = %q, want it untouched at %q", stored, "Manual investigation")
			}
		})
	}
}
