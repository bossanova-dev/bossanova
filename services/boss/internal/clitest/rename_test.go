package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestCLI_Rename_UpdatesTitle(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)

	// Use a prefix of an existing session ("sess-aaa-111") to verify prefix resolution.
	res := h.Run("rename", "sess-aaa", "New", "Title", "Here")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}

	calls := h.Daemon.UpdateSessionCalls()
	if len(calls) == 0 {
		t.Fatal("UpdateSession was not called")
	}
	got := calls[len(calls)-1]
	if got.Id != "sess-aaa-111" {
		t.Errorf("UpdateSession id: got %q, want %q", got.Id, "sess-aaa-111")
	}
	if got.Title == nil || *got.Title != "New Title Here" {
		t.Errorf("UpdateSession title: got %v, want %q", got.Title, "New Title Here")
	}

	if !strings.Contains(res.Stdout, "New Title Here") {
		t.Errorf("stdout missing new title: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "sess-aaa-111") {
		t.Errorf("stdout missing session id: %q", res.Stdout)
	}
}

func TestCLI_Rename_EmptyTitle(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)

	// A whitespace-only title should fail before the RPC.
	res := h.Run("rename", "sess-aaa", "   ")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for empty title, got 0")
	}

	calls := h.Daemon.UpdateSessionCalls()
	if len(calls) != 0 {
		t.Errorf("UpdateSession should not have been called for empty title; got %d calls", len(calls))
	}

	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "empty") {
		t.Errorf("expected error message mentioning 'empty', got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestCLI_Rename_UnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("rename", "nosuchsession", "New Title")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown session, got 0")
	}

	if calls := h.Daemon.UpdateSessionCalls(); len(calls) != 0 {
		t.Errorf("UpdateSession should not be called for unknown id; got %d calls", len(calls))
	}
}
