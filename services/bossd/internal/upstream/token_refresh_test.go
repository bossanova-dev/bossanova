package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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

// markedTokenProvider is a TokenProvider carrying a persisted re-login
// marker, the shape a provider reloaded at daemon startup has: no bearer
// token, no expiry, and a reason the openers must surface.
type markedTokenProvider struct {
	fakeTokenProvider
	reason string
}

func (m *markedTokenProvider) ReloginReason() string { return m.reason }

// TestReloginReasonForError maps each terminal sentinel onto the enumerated
// reason persisted alongside the retained keychain record.
func TestReloginReasonForError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "unknown outcome", err: ErrRefreshOutcomeUnknown, want: reloginReasonRefreshOutcomeUnknown},
		{name: "wrapped unknown outcome", err: errors.Join(errors.New("ctx deadline"), ErrRefreshOutcomeUnknown), want: reloginReasonRefreshOutcomeUnknown},
		{name: "rejected", err: ErrRefreshTokenRejected, want: reloginReasonRefreshTokenRejected},
		{name: "already exchanged", err: ErrRefreshTokenAlreadyExchanged, want: reloginReasonRefreshTokenRejected},
		{name: "bare umbrella", err: ErrAuthExpired, want: ""},
		{name: "transient", err: errors.New("workos 503"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reloginReasonForError(tc.err); got != tc.want {
				t.Fatalf("reloginReasonForError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestReloginPauseMessage pins the single sanitized warning the openers and
// the refresher emit: it must say the credentials were retained, point at
// `boss login`, and never label an ambiguous outcome as invalid_grant.
func TestReloginPauseMessage(t *testing.T) {
	unknown := reloginPauseMessage(reloginReasonRefreshOutcomeUnknown)
	if !strings.Contains(unknown, "could not be confirmed") {
		t.Errorf("unknown-outcome message does not describe the ambiguity: %q", unknown)
	}
	rejected := reloginPauseMessage(reloginReasonRefreshTokenRejected)
	if !strings.Contains(rejected, "rejected") {
		t.Errorf("rejected message does not describe the rejection: %q", rejected)
	}
	if unknown == rejected {
		t.Errorf("both reasons render the same message: %q", unknown)
	}
	for _, msg := range []string{unknown, rejected, reloginPauseMessage("")} {
		if strings.Contains(msg, "invalid_grant") {
			t.Errorf("pause message labels the outcome invalid_grant: %q", msg)
		}
		if !strings.Contains(msg, "credentials retained") {
			t.Errorf("pause message does not state credentials were retained: %q", msg)
		}
		if !strings.Contains(msg, "boss login") {
			t.Errorf("pause message does not direct the user to boss login: %q", msg)
		}
	}
}

// TestNoCredentialsPauseMessage covers the daemon-startup path: a provider
// reloaded from a marked record exposes no bearer token, and the opener must
// still explain the retained state rather than reporting "no credentials".
func TestNoCredentialsPauseMessage(t *testing.T) {
	marked := &markedTokenProvider{reason: reloginReasonRefreshOutcomeUnknown}
	got := noCredentialsPauseMessage(marked)
	if !strings.Contains(got, "could not be confirmed") || !strings.Contains(got, "credentials retained") {
		t.Fatalf("marked provider message = %q, want the retained re-login wording", got)
	}

	plain := noCredentialsPauseMessage(&fakeTokenProvider{})
	if strings.Contains(plain, "credentials retained") {
		t.Fatalf("never-logged-in message = %q, must not claim credentials were retained", plain)
	}
	if !strings.Contains(plain, "login") {
		t.Fatalf("never-logged-in message = %q, want a login prompt", plain)
	}
	if nilMsg := noCredentialsPauseMessage(nil); nilMsg != plain {
		t.Fatalf("nil provider message = %q, want %q", nilMsg, plain)
	}
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
	waitForTimer(clock, 200*time.Millisecond)

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

func TestTokenRefresh_DefaultThresholdDoesNotRefreshWithMoreThanSixtySecondsRemaining(t *testing.T) {
	clock := newFakeClock()
	tp := &fakeTokenProvider{
		token:     "old",
		expiresAt: clock.Now().Add(2 * time.Minute),
		refreshFn: func(_ context.Context) (string, error) { return "new", nil },
	}
	client := NewStreamClient(StreamClientConfig{
		TokenProvider:   tp,
		Logger:          zerolog.Nop(),
		Clock:           clock,
		RefreshInterval: 50 * time.Millisecond,
	})
	if client.refreshThreshold != 60*time.Second {
		t.Fatalf("default refreshThreshold = %v, want 60s", client.refreshThreshold)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan *pb.DaemonEvent, 4)

	errCh := make(chan error, 1)
	go func() { errCh <- client.runTokenRefresher(ctx, outbound) }()
	waitForTimer(clock, 200*time.Millisecond)
	clock.Advance(100 * time.Millisecond)

	select {
	case ev := <-outbound:
		t.Fatalf("unexpected refresh event: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	tp.mu.Lock()
	refreshCalls := tp.refreshCalls
	tp.mu.Unlock()
	if refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0", refreshCalls)
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

	waitForTimer(clock, 200*time.Millisecond)
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

// TestTokenRefresh_TerminalReloginErrorMarksAuthStateBeforeReturning proves the
// BOS-659 contract: both terminal re-login errors flip the shared AuthState
// before runTokenRefresher hands the error back, so the Run loop takes its
// intentional-pause branch instead of counting a stream error and backing off.
// The uncertain refresh token is never retried — exactly one Refresh happens.
func TestTokenRefresh_TerminalReloginErrorMarksAuthStateBeforeReturning(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "ambiguous outcome", err: ErrRefreshOutcomeUnknown},
		{name: "authoritative rejection", err: ErrRefreshTokenRejected},
		{name: "already exchanged", err: ErrRefreshTokenAlreadyExchanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			refreshErr := tc.err
			tp := &fakeTokenProvider{
				token:     "old",
				expiresAt: clock.Now().Add(1 * time.Minute),
				refreshFn: func(_ context.Context) (string, error) { return "", refreshErr },
			}
			authState := NewAuthState()
			client := NewStreamClient(StreamClientConfig{
				TokenProvider:    tp,
				AuthState:        authState,
				Logger:           zerolog.Nop(),
				Clock:            clock,
				RefreshInterval:  50 * time.Millisecond,
				RefreshThreshold: 10 * time.Minute,
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			outbound := make(chan *pb.DaemonEvent, 4)
			errCh := make(chan error, 1)
			go func() { errCh <- client.runTokenRefresher(ctx, outbound) }()

			waitForTimer(clock, 200*time.Millisecond)
			clock.Advance(100 * time.Millisecond)

			select {
			case err := <-errCh:
				if !errors.Is(err, refreshErr) {
					t.Fatalf("refresher error = %v, want wrapped %v", err, refreshErr)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("refresher did not return after terminal re-login error")
			}

			// The mark must already be in place by the time the error is
			// observable — otherwise the Run loop races into its error branch.
			if !authState.NeedsLogin() {
				t.Fatal("terminal re-login error did not mark the shared AuthState")
			}
			tp.mu.Lock()
			calls := tp.refreshCalls
			tp.mu.Unlock()
			if calls != 1 {
				t.Fatalf("Refresh calls = %d, want exactly 1 (the uncertain token must not be retried)", calls)
			}
		})
	}
}

// TestTokenRefresh_TransientFailureDoesNotMarkAuthState pins the other half:
// an ordinary refresh failure still closes the stream for a normal reconnect.
func TestTokenRefresh_TransientFailureDoesNotMarkAuthState(t *testing.T) {
	clock := newFakeClock()
	tp := &fakeTokenProvider{
		token:     "old",
		expiresAt: clock.Now().Add(1 * time.Minute),
		refreshFn: func(_ context.Context) (string, error) { return "", errors.New("workos 503") },
	}
	authState := NewAuthState()
	client := NewStreamClient(StreamClientConfig{
		TokenProvider:    tp,
		AuthState:        authState,
		Logger:           zerolog.Nop(),
		Clock:            clock,
		RefreshInterval:  50 * time.Millisecond,
		RefreshThreshold: 10 * time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.runTokenRefresher(ctx, make(chan *pb.DaemonEvent, 4)) }()

	waitForTimer(clock, 200*time.Millisecond)
	clock.Advance(100 * time.Millisecond)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error from runTokenRefresher")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("refresher did not return after failed refresh")
	}
	if authState.NeedsLogin() {
		t.Fatal("a transient refresh failure must not pause the stream for re-login")
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

// TestLogReloginPauseDoesNotClaimRemovedCredentialsWereRetained pins the one
// case where the shared "credentials retained" wording would be a lie: the
// record was DELETED by an explicit logout, not retained behind a marker.
// Every other pause keeps that wording (TestReloginPauseMessage).
func TestLogReloginPauseDoesNotClaimRemovedCredentialsWereRetained(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	logReloginPause(&logger, "terminal: ", fmt.Errorf("refresh: %w", errCredentialsRemoved))

	got := buf.String()
	if strings.Contains(got, "credentials retained") {
		t.Fatalf("removed-credentials pause claims they were retained: %q", got)
	}
	if !strings.Contains(got, "terminal: ") {
		t.Fatalf("pause dropped the caller prefix: %q", got)
	}
	if !strings.Contains(got, "boss login") {
		t.Fatalf("pause does not direct the user to boss login: %q", got)
	}
	if strings.Contains(got, "relogin_reason") {
		t.Fatalf("a removal is not one of the enumerated persisted reasons: %q", got)
	}
}
