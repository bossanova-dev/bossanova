package models

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// agentChatWireGolden pins the binary and JSON encodings of every message that
// carries the chat, chat-metadata or turn type. A proto message name never
// reaches either wire (nothing here uses google.protobuf.Any), so renaming
// those types must leave these bytes unchanged. The golden was recorded from
// the pre-rename types; regenerate it only for a deliberate wire change, with
// BOSS_AGENT_CHAT_WIRE_GOLDEN=1.
const agentChatWireGolden = "testdata/agent_chat_wire.golden"

func wireTimestamp(sec int64) *timestamppb.Timestamp {
	return timestamppb.New(time.Unix(sec, 123456789).UTC())
}

func wireChat() *pb.AgentChat {
	return &pb.AgentChat{
		Id:                "chat-1",
		SessionId:         "sess-1",
		AgentSessionId:    "agent-session-1",
		Title:             "Initial implementation",
		DaemonId:          "daemon-1",
		CreatedAt:         wireTimestamp(1760000000),
		TmuxSessionName:   "boss-sess-1",
		AgentName:         "claude",
		ProviderSessionId: "provider-1",
		StartError:        "ready marker not seen",
		OwnerUserId:       "user-1",
		Role:              pb.ChatRole_CHAT_ROLE_BUILD,
		ReclaimedAt:       wireTimestamp(1760000600),
		ReclaimReason:     "idle build reclaimed",
	}
}

func wireChatMetadata() *pb.AgentChatMetadata {
	return &pb.AgentChatMetadata{
		Id:                 "chat-1",
		SessionId:          "sess-1",
		AgentSessionId:     "agent-session-1",
		Title:              "Initial implementation",
		DaemonId:           "daemon-1",
		CreatedAt:          wireTimestamp(1760000000),
		UpdatedAt:          wireTimestamp(1760000300),
		LastMessagePreview: "Done: tests pass",
		AgentName:          "codex",
	}
}

func wireTurn() *pb.AgentTurn {
	return &pb.AgentTurn{Role: "assistant", Content: "Done: tests pass", Timestamp: wireTimestamp(1760000300)}
}

// agentChatWireCases covers every field whose type is one of the renamed messages.
func agentChatWireCases() []struct {
	name string
	msg  proto.Message
} {
	return []struct {
		name string
		msg  proto.Message
	}{
		{"ProxyListChatsResponse", &pb.ProxyListChatsResponse{Chats: []*pb.AgentChat{wireChat()}}},
		{"ProxyRecordChatResponse", &pb.ProxyRecordChatResponse{Chat: wireChat()}},
		{"ListChatsResponse", &pb.ListChatsResponse{Chats: []*pb.AgentChat{wireChat()}}},
		{"RecordChatResponse", &pb.RecordChatResponse{Chat: wireChat()}},
		{"CommandResult.record_chat", &pb.CommandResult{
			CommandId: "cmd-1",
			Ok:        true,
			Payload:   &pb.CommandResult_RecordChat{RecordChat: wireChat()},
		}},
		{"ChatDelta", &pb.ChatDelta{Kind: pb.ChatDelta_KIND_UPDATED, Chat: wireChatMetadata()}},
		{"DaemonSnapshot.chats", &pb.DaemonSnapshot{Chats: []*pb.AgentChatMetadata{wireChatMetadata()}}},
		{"ProxyChatListSnapshot", &pb.ProxyChatListSnapshot{
			SessionId: "sess-1",
			DaemonId:  "daemon-1",
			Chats:     []*pb.AgentChatMetadata{wireChatMetadata()},
		}},
		{"ReportContext.recent_turns", &pb.ReportContext{
			BossVersion: "1.2.3",
			RecentTurns: []*pb.AgentTurn{wireTurn(), {Role: "user", Content: "Implement it"}},
		}},
	}
}

func encodeAgentChatWire(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, tc := range agentChatWireCases() {
		bin, err := proto.MarshalOptions{Deterministic: true}.Marshal(tc.msg)
		if err != nil {
			t.Fatalf("%s: binary marshal: %v", tc.name, err)
		}
		raw, err := protojson.Marshal(tc.msg)
		if err != nil {
			t.Fatalf("%s: json marshal: %v", tc.name, err)
		}
		// protojson deliberately randomises insignificant whitespace; compact it away.
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatalf("%s: compact json: %v", tc.name, err)
		}
		fmt.Fprintf(&out, "%s binary %s\n%s json %s\n", tc.name, hex.EncodeToString(bin), tc.name, compact.String())
	}
	return out.Bytes()
}

func TestAgentChatWireEncodingMatchesGolden(t *testing.T) {
	got := encodeAgentChatWire(t)
	if os.Getenv("BOSS_AGENT_CHAT_WIRE_GOLDEN") == "1" {
		if err := os.WriteFile(agentChatWireGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", agentChatWireGolden)
		return
	}
	want, err := os.ReadFile(agentChatWireGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if bytes.Equal(got, want) {
		return
	}
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Errorf("line %d differs:\n got: %s\nwant: %s", i+1, g, w)
		}
	}
}
