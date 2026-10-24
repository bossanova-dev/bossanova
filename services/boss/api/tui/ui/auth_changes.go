package ui

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type AuthChangeClient interface {
	NotifyAuthChange(context.Context, string) (*pb.NotifyAuthChangeResponse, error)
}

// AuthChangeQueue preserves the order in which local auth transitions finish
// when their best-effort daemon notifications run asynchronously. A stalled
// logout must reach bossd before a later login, or the stale logout can clear
// the daemon's newly refreshed credentials.
type AuthChangeQueue struct {
	mu                  sync.Mutex
	tail                <-chan struct{}
	NotificationTimeout time.Duration
}

const defaultAuthChangeNotificationTimeout = 5 * time.Second

func NewAuthChangeQueue() *AuthChangeQueue {
	done := make(chan struct{})
	close(done)
	return &AuthChangeQueue{
		tail:                done,
		NotificationTimeout: defaultAuthChangeNotificationTimeout,
	}
}

func (q *AuthChangeQueue) Notify(ctx context.Context, c AuthChangeClient, action string) tea.Cmd {
	if q == nil {
		return func() tea.Msg {
			notifyAuthChange(ctx, c, action, defaultAuthChangeNotificationTimeout)
			return nil
		}
	}

	q.mu.Lock()
	previous := q.tail
	done := make(chan struct{})
	q.tail = done
	timeout := q.NotificationTimeout
	q.mu.Unlock()

	return func() tea.Msg {
		defer close(done)
		<-previous
		notifyAuthChange(ctx, c, action, timeout)
		return nil
	}
}

// notifyAuthChange nudges the daemon and deliberately drops both return values.
//
// The daemon's post-login verdict is rendered by `boss login`, which owns the
// terminal at the moment of the login and can print a warning the operator will
// actually read. This path is the TUI's own logout/login bookkeeping, running as
// a background tea.Cmd with no output surface of its own; surfacing the verdict
// here would mean routing it into the view's message loop, which is out of scope
// for BOS-945. Dropping it keeps today's behaviour rather than half-reporting it.
func notifyAuthChange(ctx context.Context, c AuthChangeClient, action string, timeout time.Duration) {
	notifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = c.NotifyAuthChange(notifyCtx, action)
}
