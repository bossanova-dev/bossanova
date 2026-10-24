package session

import (
	"encoding/json"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestChatsJSONCarriesRole(t *testing.T) {
	chats := []*pb.AgentChat{
		{AgentSessionId: "v", Title: "再確認する PR #3100", Role: pb.ChatRole_CHAT_ROLE_VERIFY},
		{AgentSessionId: "legacy", Title: "verify"},
		{AgentSessionId: "future", Role: pb.ChatRole(99)},
	}
	raw, err := json.Marshal(newChatsJSON(chats, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Chats []map[string]any `json:"chats"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"verify", "", ""} {
		if role, ok := got.Chats[i]["role"]; !ok || role != want {
			t.Errorf("row %d role = %v (present %v), want %q", i, role, ok, want)
		}
	}
	if _, ok := got.Chats[1]["reclaimed_at"]; !ok {
		t.Error("reclaimed_at missing; every chats --json field is always emitted")
	}
}
