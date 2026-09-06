package server

import (
	"context"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
)

type deleteChatStoreFake struct {
	mu         sync.Mutex
	chat       *models.AgentChat
	getErr     error
	deleteErr  error
	operations *[]string
	// clearOnDelete models the real store, which stops resolving a row once it
	// is deleted and reports the miss as a wrapped db.ErrAgentChatNotFound
	// rather than a nil chat (internal/db/agent_chat_store.go). Opt-in so the
	// other tests in this file keep their simpler always-resolves fake.
	clearOnDelete bool
}

func (f *deleteChatStoreFake) Create(context.Context, db.CreateAgentChatParams) (*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) GetByAgentSessionID(context.Context, string) (*models.AgentChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.chat, nil
}

func (f *deleteChatStoreFake) ListBySession(context.Context, string) ([]*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) ListBySessions(context.Context, []string) (map[string][]*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) UpdateTitle(context.Context, string, string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateTitleByAgentSessionID(context.Context, string, string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateTmuxSessionName(context.Context, string, *string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateProviderSessionID(context.Context, string, *string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateAccountIDByAgentSessionID(context.Context, string, *string) error {
	return nil
}

func (f *deleteChatStoreFake) MarkStartFailed(context.Context, string, string) error {
	return nil
}

func (f *deleteChatStoreFake) RebindResumedChat(_ context.Context, _ string, _ db.RebindResumedChatParams) error {
	return nil
}

func (f *deleteChatStoreFake) DeleteByAgentSessionID(_ context.Context, agentSessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete:"+agentSessionID)
	}
	if f.deleteErr == nil && f.clearOnDelete {
		f.chat = nil
		f.getErr = db.ErrAgentChatNotFound
	}
	return f.deleteErr
}

func (f *deleteChatStoreFake) ListWithTmuxSession(context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) ListRoutableChats(context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

func TestDeleteChat_KillsTmuxSessionBeforeDeletingRow(t *testing.T) {
	ctx := context.Background()
	tmuxName := "boss-repo1234-agent5678"
	agentSessionID := "agent-5678"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			chat: &models.AgentChat{
				SessionID:       "session-1",
				AgentSessionID:  agentSessionID,
				TmuxSessionName: &tmuxName,
			},
		},
		chatStatus: status.NewTracker(),
		tmux: tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "kill-session" {
				target := ""
				if len(args) >= 3 && args[1] == "-t" {
					target = args[2]
				}
				operations = append(operations, "kill:"+target)
			}
			return exec.CommandContext(ctx, "true")
		})),
	}

	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	}))
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	want := []string{"kill:" + tmuxName, "delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

func TestDeleteChat_RejectsChatFromADifferentSession(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-5678"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			chat: &models.AgentChat{
				SessionID:      "session-OWNER",
				AgentSessionID: agentSessionID,
			},
		},
		chatStatus: status.NewTracker(),
	}

	// Caller authorized "session-OTHER" but the chat belongs to
	// "session-OWNER": the daemon must refuse and must NOT delete anything.
	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
		SessionId:      "session-OTHER",
	}))
	if err == nil {
		t.Fatal("expected DeleteChat to reject cross-session delete, got nil error")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("error code = %v, want NotFound", connect.CodeOf(err))
	}
	if len(operations) != 0 {
		t.Fatalf("expected no delete/kill operations on rejection, got %#v", operations)
	}
}

func TestDeleteChat_AllowsMatchingSessionScope(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-5678"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			chat: &models.AgentChat{
				SessionID:      "session-1",
				AgentSessionID: agentSessionID,
			},
		},
		chatStatus: status.NewTracker(),
	}

	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
		SessionId:      "session-1",
	}))
	if err != nil {
		t.Fatalf("DeleteChat with matching scope: %v", err)
	}
	want := []string{"delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

func TestDeleteChat_MissingChatIsIdempotentAndDoesNotKillTmux(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-missing"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			getErr:     db.ErrAgentChatNotFound,
		},
		chatStatus: status.NewTracker(),
		tmux: tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "kill-session" {
				operations = append(operations, "kill")
			}
			return exec.CommandContext(ctx, "true")
		})),
	}

	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	}))
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	want := []string{"delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

// AC3, and the ordering the whole criterion rests on. DeleteChat clears the
// cached status at server.go:4607 -- while the chat row is still present --
// and only deletes the row afterwards. The eviction hook therefore fires with
// the row still resolvable, so the recompute finds its session and the label
// settles; had Remove been called after the delete, the lookup would miss and
// the session's label would stay frozen with nothing left to re-emit it.
//
// The fake stops resolving the row once it is deleted, exactly as the real
// store does, so a regression that moves the eviction after the delete records
// recompute-missed instead of recompute and fails on both count and order.
func TestDeleteChat_RecomputesWhileTheChatRowIsStillPresent(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-5678"
	operations := []string{}

	store := &deleteChatStoreFake{
		operations:    &operations,
		clearOnDelete: true,
		chat: &models.AgentChat{
			SessionID:      "session-1",
			AgentSessionID: agentSessionID,
		},
	}

	tracker := status.NewTracker()
	// A live entry, so DeleteChat's Remove has something to evict and the
	// eviction hook fires at all.
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	// Stands in for cmd's wireEvictionRecompute, which lives in package main and
	// is out of reach here. Same shape: resolve each evicted id to its session,
	// then recompute that session.
	tracker.SetOnEntriesEvicted(func(agentSessionIDs []string) {
		for _, id := range agentSessionIDs {
			chat, err := store.GetByAgentSessionID(ctx, id)
			if err != nil || chat == nil {
				operations = append(operations, "recompute-missed:"+id)
				continue
			}
			operations = append(operations, "recompute:"+chat.SessionID)
		}
	})

	srv := &Server{agentChats: store, chatStatus: tracker}

	if _, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	})); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	want := []string{"recompute:session-1", "delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}
