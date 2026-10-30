package chatpicker

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type mergeHeadClient struct {
	Client
	heads []string
}

func (c *mergeHeadClient) MergeSession(_ context.Context, _ string, head string) (*pb.Session, string, error) {
	c.heads = append(c.heads, head)
	return &pb.Session{}, "", nil
}

// Proves chat-picker.AC20.
func TestMergeCmdPinsDisplayedHead_ChatPicker_AC20(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		pr         int32
		refuse     bool
	}{
		{name: "PR pins display", head: strings.Repeat("a", 40), pr: 23},
		{name: "PR missing head refuses", pr: 23, refuse: true},
		{name: "local branch needs no pin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mergeHeadClient{}
			model := Model{client: client, ctx: context.Background(), sessionID: "sess", session: &pb.Session{PrNumber: proto.Int32(tc.pr), PrDisplayHeadSha: tc.head}}
			cmd := model.mergeCmd()
			// Commands must carry the displayed snapshot, even if the model refreshes.
			model.session.PrDisplayHeadSha = strings.Repeat("b", 40)
			msg := cmd().(MergeResultMsg)
			if tc.refuse {
				if msg.Err == nil || !strings.Contains(msg.Err.Error(), "latest commit is not known yet") || len(client.heads) != 0 {
					t.Fatalf("msg=%+v heads=%v", msg, client.heads)
				}
				return
			}
			if msg.Err != nil || len(client.heads) != 1 || client.heads[0] != tc.head {
				t.Fatalf("msg=%+v heads=%v want=%q", msg, client.heads, tc.head)
			}
		})
	}
}
