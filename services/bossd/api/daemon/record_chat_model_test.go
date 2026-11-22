package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/dotenv"
	"github.com/recurser/bossd/internal/domain"
)

// launchingChatStore is recordChatStoreFake with the writes a first launch
// makes once its pane is up.
type launchingChatStore struct {
	*recordChatStoreFake
}

func (launchingChatStore) UpdateTmuxSessionName(context.Context, string, *string) error {
	return nil
}

// recordAndLaunch records a new claude chat in sess and returns what its
// first launch was built with.
func recordAndLaunch(t *testing.T, sess *models.Session, req *pb.RecordChatRequest) (*recordChatStoreFake, argvCall) {
	t.Helper()
	chats := &recordChatStoreFake{}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	srv := &Server{
		environmentOverlay: dotenv.OverlayWithRepo,
		agentChats:         launchingChatStore{chats},
		sessions:           &sessionStoreFake{sess: sess},
		chatTmux:           tmuxer,
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        builder,
		},
	}
	srv.initDeferrers()
	req.SessionId = sess.ID
	req.AgentSessionId = "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e70"
	if _, err := srv.RecordChat(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	return chats, builder.calls[0]
}

func TestRecordChat_NewChatWithNoModelLaunchesOnTheSessionsModel(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", Model: "opus-session"}
	_, launch := recordAndLaunch(t, sess, &pb.RecordChatRequest{Title: "New chat"})
	if launch.model != "opus-session" {
		t.Fatalf("launch model = %q, want the session's opus-session", launch.model)
	}
}

func TestRecordChat_NewChatLaunchesOnTheModelAndEffortItWasGiven(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", Model: "opus-session", EffectiveEffort: "high"}
	model, effort := "sonnet-chat", "low"
	chats, launch := recordAndLaunch(t, sess, &pb.RecordChatRequest{Title: "New chat", Model: &model, Effort: &effort})
	if launch.model != model || launch.effort != effort {
		t.Fatalf("launch model/effort = %q/%q, want the chat's %q/%q", launch.model, launch.effort, model, effort)
	}
	if chats.created.Model != model || chats.created.Effort != effort {
		t.Fatalf("recorded model/effort = %q/%q, want them kept on the chat so a wake launches the same", chats.created.Model, chats.created.Effort)
	}
}

func TestRecordChat_NewChatWithNoEffortLaunchesOnTheSessionsEffort(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", EffectiveEffort: "xhigh"}
	_, launch := recordAndLaunch(t, sess, &pb.RecordChatRequest{Title: "New chat"})
	if launch.effort != "xhigh" {
		t.Fatalf("launch effort = %q, want the session's xhigh", launch.effort)
	}
}

func TestWakeChat_LaunchesOnTheChatsOwnEffort(t *testing.T) {
	chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude", Effort: "low"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", EffectiveEffort: "high"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	s := &Server{
		environmentOverlay: dotenv.OverlayWithRepo,
		agentChats:         &chatStoreFake{chat: chat},
		sessions:           &sessionStoreFake{sess: sess},
		wakeHook:           wakeHook{spawner: tmuxer, transcripts: &fakeTranscriptOracle{exists: false}, argv: builder},
	}
	if _, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "agent-1"})); err != nil {
		t.Fatalf("WakeChat: %v", err)
	}
	if len(builder.calls) != 1 || builder.calls[0].effort != "low" {
		t.Fatalf("wake launches = %+v, want one on the chat's low effort", builder.calls)
	}
}
