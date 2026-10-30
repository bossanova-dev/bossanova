package rotation

import (
	"context"
	"sync"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

type sharedUsageRole struct {
	isPoller       bool
	throttledUntil time.Time
}

// UsageShare keeps the orchestrator's polling assignment and throttle hint.
// Unknown accounts retain local polling so disconnected daemons can recover.
type UsageShare struct {
	mu        sync.RWMutex
	accounts  map[string]sharedUsageRole
	report    func(context.Context, string, *domain.UsageSnapshot, time.Time)
	staleness time.Duration
}

func NewUsageShare(report func(context.Context, string, *domain.UsageSnapshot, time.Time), staleness ...time.Duration) *UsageShare {
	window := 30 * time.Minute
	if len(staleness) > 0 && staleness[0] > 0 {
		window = staleness[0]
	}
	return &UsageShare{accounts: make(map[string]sharedUsageRole), report: report, staleness: window}
}

func (s *UsageShare) Set(accountID string, isPoller bool, throttledUntil time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts == nil {
		s.accounts = make(map[string]sharedUsageRole)
	}
	s.accounts[accountID] = sharedUsageRole{isPoller: isPoller, throttledUntil: throttledUntil}
}

func (s *UsageShare) ShouldProbe(accountID string, usage *domain.UsageSnapshot, now time.Time, staleness time.Duration) bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	role, known := s.accounts[accountID]
	s.mu.RUnlock()
	if !known || role.isPoller {
		return true
	}
	if role.throttledUntil.After(now) {
		return false
	}
	return usage == nil || usage.FetchedAt == nil || now.Sub(*usage.FetchedAt) >= staleness
}

func (s *UsageShare) IsPoller(accountID string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accounts[accountID].isPoller
}

func (s *UsageShare) StalenessWindow() time.Duration {
	if s == nil || s.staleness <= 0 {
		return 30 * time.Minute
	}
	return s.staleness
}

// Report sends only the designated poller's results. The callback runs outside
// the lock because stream delivery can synchronously update an assignment.
func (s *UsageShare) Report(ctx context.Context, accountID string, usage *domain.UsageSnapshot, until time.Time) {
	if s == nil {
		return
	}
	s.mu.RLock()
	poller := s.accounts[accountID].isPoller
	report := s.report
	s.mu.RUnlock()
	if poller && report != nil {
		report(ctx, accountID, usage, until)
	}
}
