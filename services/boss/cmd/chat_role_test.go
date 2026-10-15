package main

import (
	"encoding/json"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestParseChatRole(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want pb.ChatRole
	}{
		{"operator", pb.ChatRole_CHAT_ROLE_OPERATOR},
		{"verify", pb.ChatRole_CHAT_ROLE_VERIFY},
		{" Repair ", pb.ChatRole_CHAT_ROLE_REPAIR},
		{"BUILD", pb.ChatRole_CHAT_ROLE_BUILD},
		{"verifier", pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{"", pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{"unspecified", pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
	} {
		in, want := tc.in, tc.want
		got, err := parseChatRole(in)
		if got != want {
			t.Errorf("parseChatRole(%q) = %v, want %v", in, got, want)
		}
		if (err != nil) != (want == pb.ChatRole_CHAT_ROLE_UNSPECIFIED) {
			t.Errorf("parseChatRole(%q) err = %v; only an unknown spelling is refused", in, err)
		}
	}
}

func TestChatsJSONCarriesRole(t *testing.T) {
	chats := []*pb.ClaudeChat{
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
	if role := newChatJSON(chats[0]).Chat.Role; role != "verify" {
		t.Errorf("chat new envelope role = %q, want verify", role)
	}
}
