package vcs

import (
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// rateLimitLogWindow is how long a rate-limited error with no known reset
// time demotes repeats at its log site.
const rateLimitLogWindow = 5 * time.Minute

// IsRateLimited reports whether err is a refusal for an exhausted API quota:
// a provider error wrapping ErrRateLimited, or the host's ResourceExhausted
// status as a plugin sees it over gRPC.
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrRateLimited) || grpcstatus.Code(err) == codes.ResourceExhausted
}

// RateLimitLog keeps a background loop's log quiet while a quota is
// exhausted: each log site reports the first refusal of a quota window and
// demotes the rest until the window ends. The zero value is ready to use.
type RateLimitLog struct {
	// Now replaces the wall clock in tests.
	Now func() time.Time

	mu      sync.Mutex
	windows map[string]time.Time
}

// Repeat reports whether err is a rate-limited error that site has already
// logged in the current quota window. A first refusal opens the window, which
// ends at the error's reset time when it carries one and five minutes later
// otherwise. Any other error is never a repeat.
func (l *RateLimitLog) Repeat(site string, err error) bool {
	if !IsRateLimited(err) {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if end, ok := l.windows[site]; ok && now.Before(end) {
		return true
	}
	end := now.Add(rateLimitLogWindow)
	if reset, ok := RateLimitResetAt(err); ok && reset.After(now) {
		end = reset
	}
	if l.windows == nil {
		l.windows = make(map[string]time.Time)
	}
	l.windows[site] = end
	return false
}

// Level is the level to log err at site: Debug for a repeat (see Repeat),
// Warn otherwise.
func (l *RateLimitLog) Level(site string, err error) zerolog.Level {
	if l.Repeat(site, err) {
		return zerolog.DebugLevel
	}
	return zerolog.WarnLevel
}

func (l *RateLimitLog) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}
