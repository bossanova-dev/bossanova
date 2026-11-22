package server

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

// ChatCounts is what Session.chat_count and Session.working_chat_count report.
type ChatCounts struct {
	Chats   int32
	Working int32
}

// CountChats counts a session's chats and the ones working. Working is a fresh
// CHAT_STATUS_WORKING heartbeat; status.PromoteWaiting never moves a working
// chat elsewhere, so an armed callback does not change the count.
func CountChats(tracker *status.Tracker, chats []*domain.AgentChat) ChatCounts {
	counts := ChatCounts{Chats: clampInt32(len(chats))}
	if tracker == nil {
		return counts
	}
	for _, chat := range chats {
		if e := tracker.Get(chat.AgentSessionID); e != nil && e.Status == pb.ChatStatus_CHAT_STATUS_WORKING {
			counts.Working++
		}
	}
	return counts
}

// Stamp sets both count fields on p.
func (c ChatCounts) Stamp(p *pb.Session) {
	p.ChatCount = &c.Chats
	p.WorkingChatCount = &c.Working
}
