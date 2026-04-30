package views

import (
	"testing"

	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestPTYStatusToChatStatus(t *testing.T) {
	cases := []struct {
		in   string
		want pb.ChatStatus
	}{
		{bosspty.StatusWorking, pb.ChatStatus_CHAT_STATUS_WORKING},
		{bosspty.StatusIdle, pb.ChatStatus_CHAT_STATUS_IDLE},
		{bosspty.StatusQuestion, pb.ChatStatus_CHAT_STATUS_QUESTION},
		{bosspty.StatusStopped, pb.ChatStatus_CHAT_STATUS_STOPPED},
		{"", pb.ChatStatus_CHAT_STATUS_UNSPECIFIED},
		{"garbage", pb.ChatStatus_CHAT_STATUS_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := ptyStatusToChatStatus(tc.in)
			if got != tc.want {
				t.Fatalf("ptyStatusToChatStatus(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
