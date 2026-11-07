package upstream

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestCloudRedactionSnapshotsAndDeltas(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  CloudControlPolicy
		private bool
	}{
		{"private", fixedCloudControl("private"), true},
		{"read-only", fixedCloudControl("read-only"), false},
		{"full", fixedCloudControl("full"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &pb.Session{Id: "session", Title: "session title", Plan: "operator prompt", BranchName: "branch", PrNumber: proto.Int32(42), BlockedReason: proto.String("blocked")}
			chat := &pb.AgentChatMetadata{Id: "chat", Title: "chat title", LastMessagePreview: "agent output"}
			originalSession := proto.Clone(session).(*pb.Session)
			originalChat := proto.Clone(chat).(*pb.AgentChatMetadata)
			wantSession := proto.Clone(session).(*pb.Session)
			wantChat := proto.Clone(chat).(*pb.AgentChatMetadata)
			if tc.private {
				wantSession.Plan = ""
				wantChat.LastMessagePreview = ""
			}
			client := newTestClient(StreamStores{
				Sessions: &fakeSessionReader{sessions: []*pb.Session{session}},
				Chats:    &fakeChatReader{chats: []*pb.AgentChatMetadata{chat}},
			})
			client.cloudControl = tc.policy
			snapshot, err := client.buildSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Sessions) != 1 || !proto.Equal(snapshot.Sessions[0], wantSession) {
				t.Fatalf("snapshot sessions = %v, want %v", snapshot.Sessions, wantSession)
			}
			if len(snapshot.Chats) != 1 || !proto.Equal(snapshot.Chats[0], wantChat) {
				t.Fatalf("snapshot chats = %v, want %v", snapshot.Chats, wantChat)
			}
			out := make(chan *pb.DaemonEvent, 1)
			client.forwardEvent(context.Background(), StreamEvent{Session: &SessionEvent{Kind: pb.SessionDelta_KIND_UPDATED, Session: session}}, out, nil)
			if delta := (<-out).GetSession(); delta.Kind != pb.SessionDelta_KIND_UPDATED || !proto.Equal(delta.Session, wantSession) {
				t.Fatalf("session delta = %v, want %v", delta, wantSession)
			}
			client.forwardEvent(context.Background(), StreamEvent{Chat: &ChatEvent{Kind: pb.ChatDelta_KIND_UPDATED, Chat: chat}}, out, nil)
			if delta := (<-out).GetChat(); delta.Kind != pb.ChatDelta_KIND_UPDATED || !proto.Equal(delta.Chat, wantChat) {
				t.Fatalf("chat delta = %v, want %v", delta, wantChat)
			}
			deletedSession := &pb.Session{Id: "deleted-session"}
			client.forwardEvent(context.Background(), StreamEvent{Session: &SessionEvent{Kind: pb.SessionDelta_KIND_DELETED, Session: deletedSession}}, out, nil)
			if delta := (<-out).GetSession(); delta.Kind != pb.SessionDelta_KIND_DELETED || !proto.Equal(delta.Session, deletedSession) {
				t.Fatalf("deleted session delta = %v", delta)
			}
			deletedChat := &pb.AgentChatMetadata{Id: "deleted-chat"}
			client.forwardEvent(context.Background(), StreamEvent{Chat: &ChatEvent{Kind: pb.ChatDelta_KIND_DELETED, Chat: deletedChat}}, out, nil)
			if delta := (<-out).GetChat(); delta.Kind != pb.ChatDelta_KIND_DELETED || !proto.Equal(delta.Chat, deletedChat) {
				t.Fatalf("deleted chat delta = %v", delta)
			}
			if !proto.Equal(session, originalSession) || !proto.Equal(chat, originalChat) {
				t.Fatal("snapshot or delta mutated store-owned input")
			}
		})
	}
}
