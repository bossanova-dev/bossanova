package chat

import (
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

func TestChatNewJSONCarriesRole(t *testing.T) {
	if role := newChatJSON(&pb.AgentChat{Role: pb.ChatRole_CHAT_ROLE_VERIFY}).Chat.Role; role != "verify" {
		t.Errorf("chat new envelope role = %q, want verify", role)
	}
}
