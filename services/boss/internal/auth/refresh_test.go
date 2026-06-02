package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryRefreshStore struct {
	mu      sync.Mutex
	tokens  *Tokens
	saveErr error
}

func (s *memoryRefreshStore) Save(tokens *Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	cpy := *tokens
	s.tokens = &cpy
	return nil
}

func (s *memoryRefreshStore) Load() (*Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return nil, errors.New("no tokens")
	}
	cpy := *s.tokens
	return &cpy, nil
}

func (s *memoryRefreshStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = nil
	return nil
}

type mutexRefreshLock struct {
	mu *sync.Mutex
}

func (l *mutexRefreshLock) Unlock() error {
	l.mu.Unlock()
	return nil
}

func withRefreshHooks(t *testing.T, lockMu *sync.Mutex, refresh func(context.Context, Config, string) (*Tokens, error)) {
	t.Helper()
	origAcquire := acquireRefreshLock
	origRefresh := refreshAccessToken
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		lockMu.Lock()
		return &mutexRefreshLock{mu: lockMu}, nil
	}
	refreshAccessToken = refresh
	t.Cleanup(func() {
		acquireRefreshLock = origAcquire
		refreshAccessToken = origRefresh
	})
}

func TestManagerRefreshSingleFlightReloadsAfterLock(t *testing.T) {
	store := &memoryRefreshStore{tokens: &Tokens{
		AccessToken:  "expired-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "old@example.com",
	}}
	var lockMu sync.Mutex
	refreshCalls := 0
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		refreshCalls++
		return &Tokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	})
	lockMu.Lock()
	waitingForLock := make(chan struct{}, 2)
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		waitingForLock <- struct{}{}
		lockMu.Lock()
		return &mutexRefreshLock{mu: &lockMu}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- NewManager(store, Config{}).Refresh(context.Background())
		}()
	}
	for range 2 {
		select {
		case <-waitingForLock:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for refresh callers to block on lock")
		}
	}
	lockMu.Unlock()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Refresh returned error: %v", err)
		}
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if store.tokens.RefreshToken != "fresh-refresh" {
		t.Fatalf("refresh token = %q", store.tokens.RefreshToken)
	}
}

func TestManagerAccessTokenSkipsRefreshWhenReloadFindsFreshToken(t *testing.T) {
	store := &memoryRefreshStore{tokens: &Tokens{
		AccessToken:  "expired-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}}
	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		t.Fatal("refresh should be skipped after lock reload")
		return nil, nil
	})
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		if err := store.Save(&Tokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("seed fresh token: %v", err)
		}
		lockMu.Lock()
		return &mutexRefreshLock{mu: &lockMu}, nil
	}

	got, err := NewManager(store, Config{}).AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken returned error: %v", err)
	}
	if got != "fresh-access" {
		t.Fatalf("AccessToken = %q, want fresh-access", got)
	}
}

func TestManagerRefreshReturnsSaveFailure(t *testing.T) {
	store := &memoryRefreshStore{
		tokens: &Tokens{
			AccessToken:  "expired-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour),
		},
		saveErr: errors.New("disk full"),
	}
	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		return &Tokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	})

	err := NewManager(store, Config{}).Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "save refreshed tokens") {
		t.Fatalf("Refresh error = %v, want save failure", err)
	}
}

func TestManagerRefreshDoesNotOverwriteNewLoginAfterHTTPRefresh(t *testing.T) {
	store := &memoryRefreshStore{tokens: &Tokens{
		AccessToken:  "expired-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "old@example.com",
	}}
	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(_ context.Context, _ Config, refreshToken string) (*Tokens, error) {
		if refreshToken != "old-refresh" {
			t.Fatalf("refresh token = %q, want old-refresh", refreshToken)
		}
		if err := store.Save(&Tokens{
			AccessToken:  "login-access",
			RefreshToken: "login-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
			Email:        "login@example.com",
		}); err != nil {
			t.Fatalf("save new login tokens: %v", err)
		}
		return &Tokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
			Email:        "old@example.com",
		}, nil
	})

	if err := NewManager(store, Config{}).Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if store.tokens.AccessToken != "login-access" {
		t.Fatalf("access token = %q, want login-access", store.tokens.AccessToken)
	}
	if store.tokens.RefreshToken != "login-refresh" {
		t.Fatalf("refresh token = %q, want login-refresh", store.tokens.RefreshToken)
	}
	if store.tokens.Email != "login@example.com" {
		t.Fatalf("email = %q, want login@example.com", store.tokens.Email)
	}
}

func TestManagerRefreshUsesBoundedRequestContext(t *testing.T) {
	store := &memoryRefreshStore{tokens: &Tokens{
		AccessToken:  "expired-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "old@example.com",
	}}
	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(ctx context.Context, _ Config, _ string) (*Tokens, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("refresh context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > 11*time.Second {
			t.Fatalf("refresh context deadline is %s from now, want about 10s", remaining)
		}
		return &Tokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
			Email:        "old@example.com",
		}, nil
	})

	if err := NewManager(store, Config{}).Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
}

func TestManagerRefreshPreservesEmail(t *testing.T) {
	store := &memoryRefreshStore{tokens: &Tokens{
		AccessToken:  "expired-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "old@example.com",
	}}
	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		return &Tokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	})

	if err := NewManager(store, Config{}).Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if store.tokens.Email != "old@example.com" {
		t.Fatalf("email = %q, want old@example.com", store.tokens.Email)
	}
}
