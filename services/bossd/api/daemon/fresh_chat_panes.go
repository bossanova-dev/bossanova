package server

import (
	"sync"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

const freshChatPaneWindow = 10 * time.Minute

// The fact is transient: a daemon restart safely restores established timing.
// The zero value works for both the live server and handler tests.
type freshChatPanes struct {
	mu       sync.Mutex
	launched map[string]time.Time
}

func (s *Server) markFreshChatPane(chatID string, launched time.Time) {
	s.freshChatPanes.mu.Lock()
	defer s.freshChatPanes.mu.Unlock()
	if s.freshChatPanes.launched == nil {
		s.freshChatPanes.launched = make(map[string]time.Time)
	}
	s.freshChatPanes.launched[chatID] = launched
}

func (s *Server) isFreshChatPane(chatID string) bool {
	s.freshChatPanes.mu.Lock()
	defer s.freshChatPanes.mu.Unlock()
	now := time.Now()
	for id, launched := range s.freshChatPanes.launched {
		if now.Sub(launched) >= freshChatPaneWindow {
			delete(s.freshChatPanes.launched, id)
		}
	}
	_, fresh := s.freshChatPanes.launched[chatID]
	return fresh
}

func (s *Server) settleFreshChatPane(chatID string, willSubmit bool, sendErr error) {
	if !willSubmit {
		return
	}
	switch domain.OutcomeOf(sendErr) {
	case domain.OutcomeUnclassified, domain.OutcomeNotSubmitted, domain.OutcomeBlockedByModal, domain.OutcomeComposerOccupied:
		return
	case domain.OutcomeSubmitted, domain.OutcomeQueued, domain.OutcomeUnconfirmed:
		s.freshChatPanes.mu.Lock()
		delete(s.freshChatPanes.launched, chatID)
		s.freshChatPanes.mu.Unlock()
	}
}

func (s *Server) isFreshChatSubmit(chatID string, willSubmit, isLive bool) bool {
	return willSubmit && (!isLive || s.isFreshChatPane(chatID))
}
