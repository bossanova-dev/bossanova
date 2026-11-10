package upstream

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"google.golang.org/protobuf/proto"
)

func TestCloudRedactionSnapshotsAndDeltas_ChatPicker_AC238101(t *testing.T) {
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
			session := &pb.Session{Id: "session", RepoId: "repo", Title: "session title", Plan: "operator prompt", BranchName: "branch", PrNumber: proto.Int32(42), BlockedReason: proto.String("blocked")}
			chat := &pb.AgentChatMetadata{Id: "chat", SessionId: "session", Title: "chat title", LastMessagePreview: "agent output"}
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
			client.targetObjects = &targetFixture{objects: map[targetKey]CommandTargetObject{{"session", "session"}: {RepoID: "repo"}, {"repo", "repo"}: {RepoID: "repo"}}}
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

func TestCloudRedactionMixedRepos_Sessions_AC238101(t *testing.T) {
	c, _, targets := targetClient()
	c.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"a": config.CloudControlPrivate, "b": config.CloudControlFull}}
	c.targetObjects = targets
	c.stores = StreamStores{Sessions: &fakeSessionReader{sessions: []*pb.Session{{Id: "sa", RepoId: "a", Plan: "secret"}, {Id: "sb", RepoId: "b", Plan: "public"}}}, Chats: &fakeChatReader{chats: []*pb.AgentChatMetadata{{Id: "ca", SessionId: "sa", LastMessagePreview: "secret"}, {Id: "cb", SessionId: "sb", LastMessagePreview: "public"}}}}
	snap, err := c.buildSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Sessions[0].Plan != "" || snap.Sessions[1].Plan != "public" || snap.Chats[0].LastMessagePreview != "" || snap.Chats[1].LastMessagePreview != "public" {
		t.Fatalf("mixed snapshot leaked or over-redacted: %v", snap)
	}
	out := make(chan *pb.DaemonEvent, 1)
	c.forwardEvent(context.Background(), StreamEvent{Session: &SessionEvent{Session: &pb.Session{Id: "sa", RepoId: "a", Plan: "secret"}}}, out, nil)
	if (<-out).GetSession().Session.Plan != "" {
		t.Fatal("private delta leaked")
	}
	c.forwardEvent(context.Background(), StreamEvent{Chat: &ChatEvent{Chat: &pb.AgentChatMetadata{Id: "cb", SessionId: "sb", LastMessagePreview: "public"}}}, out, nil)
	if (<-out).GetChat().Chat.LastMessagePreview != "public" {
		t.Fatal("full delta redacted")
	}
}

func TestCloudRedactionMixedCommandResults(t *testing.T) {
	c, _, _ := targetClient()
	c.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"a": config.CloudControlPrivate, "b": config.CloudControlFull}}
	result := &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{Payload: &pb.CommandResult_ListNotes{ListNotes: &pb.ListNotesResponse{Notes: []*pb.Note{{Id: "na", RepoId: "a", Body: "secret"}, {Id: "nb", RepoId: "b", Body: "public"}, {Id: "unknown", Body: "unresolved"}}}}}}}
	cloned := proto.CloneOf(result)
	c.newCloudRedactor().redactForCloud(context.Background(), cloned)
	redacted := cloned.GetResult().GetListNotes()
	if redacted.Notes[0].Body != "" || redacted.Notes[1].Body != "public" || redacted.Notes[2].Body != "" {
		t.Fatalf("mixed result leaked or over-redacted: %v", redacted)
	}
	if result.GetResult().GetListNotes().Notes[0].Body != "secret" {
		t.Fatal("mutated result")
	}
}

func TestCloudRedactionBroadcastOrigin(t *testing.T) {
	c, _, _ := targetClient()
	c.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"a": config.CloudControlPrivate, "b": config.CloudControlFull}}
	for _, tc := range []struct{ chat, body string }{{"ca", ""}, {"cb", "body"}, {"unknown", ""}, {"", "body"}} {
		got := &pb.BroadcastEgress{OriginChatId: tc.chat, Message: "body"}
		c.newCloudRedactor().redactForCloud(context.Background(), got)
		if got.Message != tc.body {
			t.Fatalf("origin=%q body=%q want=%q", tc.chat, got.Message, tc.body)
		}
	}
}

func TestCloudRedactionQueuedDeltaAfterRepoRemoval(t *testing.T) {
	c, _, targets := targetClient()
	c.cloudControl = fixedCloudControl("full")
	delete(targets.objects, targetKey{"repo", "a"})
	out := make(chan *pb.DaemonEvent, 1)
	c.forwardEvent(context.Background(), StreamEvent{Session: &SessionEvent{Session: &pb.Session{Id: "sa", RepoId: "a", Plan: "secret"}}}, out, nil)
	if (<-out).GetSession().GetSession().GetPlan() != "" {
		t.Fatal("queued delta leaked content after its repo registration disappeared")
	}
}
