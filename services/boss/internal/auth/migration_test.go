package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// resetMigrationHintOnce lets tests exercise the hint more than once.
func resetMigrationHintOnce(w *bytes.Buffer) {
	migrationHintOnce = sync.Once{}
	migrationHintOut = w
}

func TestManagerStatus_PrintsMigrationHintOnCredentialsUnreadable(t *testing.T) {
	var buf bytes.Buffer
	resetMigrationHintOnce(&buf)

	store := &mockTokenStore{
		loadErr: ErrCredentialsUnreadable,
	}
	mgr := NewManager(store, Config{ClientID: "test"})

	got := mgr.Status()
	if got.LoggedIn {
		t.Fatalf("expected LoggedIn=false when credentials unreadable")
	}
	if !strings.Contains(buf.String(), "run 'boss logout && boss login' to reset") {
		t.Fatalf("expected re-login hint in output, got: %q", buf.String())
	}
}

func TestManagerAccessToken_PrintsMigrationHintOnCredentialsUnreadable(t *testing.T) {
	var buf bytes.Buffer
	resetMigrationHintOnce(&buf)

	store := &mockTokenStore{
		loadErr: ErrCredentialsUnreadable,
	}
	mgr := NewManager(store, Config{ClientID: "test"})

	tok, err := mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken returned error: %v", err)
	}
	if tok != "" {
		t.Fatalf("expected empty token when credentials unreadable, got %q", tok)
	}
	if !strings.Contains(buf.String(), "run 'boss logout && boss login' to reset") {
		t.Fatalf("expected re-login hint in output, got: %q", buf.String())
	}
}

func TestMaybeWarnCredentialsUnreadable_OnlyPrintsOnce(t *testing.T) {
	var buf bytes.Buffer
	resetMigrationHintOnce(&buf)

	maybeWarnCredentialsUnreadable(ErrCredentialsUnreadable)
	maybeWarnCredentialsUnreadable(ErrCredentialsUnreadable)
	maybeWarnCredentialsUnreadable(ErrCredentialsUnreadable)

	count := strings.Count(buf.String(), "run 'boss logout && boss login' to reset")
	if count != 1 {
		t.Fatalf("hint should print exactly once, got %d", count)
	}
}

func TestMaybeWarnCredentialsUnreadable_IgnoresUnrelatedErrors(t *testing.T) {
	var buf bytes.Buffer
	resetMigrationHintOnce(&buf)

	maybeWarnCredentialsUnreadable(errors.New("network timeout"))

	if buf.Len() != 0 {
		t.Fatalf("expected no output for unrelated error, got %q", buf.String())
	}
}
