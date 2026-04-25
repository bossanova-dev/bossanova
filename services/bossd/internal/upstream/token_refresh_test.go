package upstream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// fakeTokenProvider is a goroutine-safe mutable TokenProvider used to
// script refresh scenarios. Tests set expiresAt / refreshFn / refreshed
// directly; the provider returns them on the next call.
type fakeTokenProvider struct {
	mu           sync.Mutex
	token        string
	expiresAt    time.Time
	refreshFn    func(ctx context.Context) (string, error)
	refreshCalls int
}

func (f *fakeTokenProvider) Token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

func (f *fakeTokenProvider) ExpiresAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expiresAt
}

func (f *fakeTokenProvider) Refresh(ctx context.Context) (string, error) {
	f.mu.Lock()
	f.refreshCalls++
	fn := f.refreshFn
	f.mu.Unlock()
	if fn == nil {
		return "", errors.New("refreshFn not set")
	}
	return fn(ctx)
}

// newRefresherClient wires a StreamClient with just a token provider
// and a fake clock, enough for runTokenRefresher to be exercised
// without any of the stream plumbing.
func newRefresherClient(clock *fakeClock, tp TokenProvider) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		TokenProvider:    tp,
		Logger:           zerolog.Nop(),
		Clock:            clock,
		RefreshInterval:  50 * time.Millisecond,
		RefreshThreshold: 10 * time.Minute,
	})
}

func TestTokenRefresh_BeforeExpiry_EmitsRefreshEvent(t *testing.T) {
	clock := newFakeClock()
	tp := &fakeTokenProvider{
		token:     "old",
		expiresAt: clock.Now().Add(5 * time.Minute), // < 10min threshold
		refreshFn: func(_ context.Context) (string, error) { return "new", nil },
	}
	client := newRefresherClient(clock, tp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan *pb.DaemonEvent, 4)

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.runTokenRefresher(ctx, outbound)
	}()

	// Let the refresher reach its first After() call so the fake
	// clock actually has a timer to fire. Without the wait, Advance
	// runs before AfterFunc registers and the refresher never wakes.
	waitForTimers(clock, 1, 200*time.Millisecond)

	// Advance virtual time past the refresh interval. The AfterFunc
	// callback in the fake clock pushes a timestamp onto the channel
	// the real runTokenRefresher is waiting on.
	clock.Advance(100 * time.Millisecond)

	select {
	case ev := <-outbound:
		if tr := ev.GetTokenRefresh(); tr == nil || tr.GetAccessToken() != "new" {
			t.Fatalf("expected TokenRefresh{access_token:new}, got %v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("no refresh event emitted")
	}

	cancel()
	<-errCh
}

func TestTokenRefresh_FailureClosesStream(t *testing.T) {
	clock := newFakeClock()
	refreshErr := errors.New("workos down")
	tp := &fakeTokenProvider{
		token:     "old",
		expiresAt: clock.Now().Add(1 * time.Minute),
		refreshFn: func(_ context.Context) (string, error) { return "", refreshErr },
	}
	client := newRefresherClient(clock, tp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan *pb.DaemonEvent, 4)

	errCh := make(chan error, 1)
	go func() { errCh <- client.runTokenRefresher(ctx, outbound) }()

	waitForTimers(clock, 1, 200*time.Millisecond)
	clock.Advance(100 * time.Millisecond)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected non-nil error from runTokenRefresher")
		}
		if !errors.Is(err, refreshErr) {
			t.Fatalf("expected wrapped refreshErr, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("refresher did not return after failed refresh")
	}
}

func TestTokenRefresh_StopsOnContextCancel(t *testing.T) {
	clock := newFakeClock()
	tp := &fakeTokenProvider{
		token:     "old",
		expiresAt: clock.Now().Add(1 * time.Hour), // too far to trigger
		refreshFn: func(_ context.Context) (string, error) { return "new", nil },
	}
	client := newRefresherClient(clock, tp)

	ctx, cancel := context.WithCancel(context.Background())
	outbound := make(chan *pb.DaemonEvent, 4)

	errCh := make(chan error, 1)
	go func() { errCh <- client.runTokenRefresher(ctx, outbound) }()

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected nil on ctx cancel, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("refresher did not return on ctx cancel")
	}
}
