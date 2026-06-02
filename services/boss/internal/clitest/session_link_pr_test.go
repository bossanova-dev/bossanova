package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCLI_Session_LinkPR_Number(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("session", "link-pr", "sess-aaa-111", "42")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "https://github.com/owner/repo/pull/42") {
		t.Fatalf("stdout = %q, want linked PR URL", res.Stdout)
	}
	if !strings.HasSuffix(res.Stdout, ".\n") {
		t.Fatalf("stdout = %q, want trailing newline (no literal \\n)", res.Stdout)
	}
	sess := findSession(t, h, "sess-aaa-111")
	if sess.PrNumber == nil || *sess.PrNumber != 42 {
		t.Fatalf("PrNumber = %v, want 42", sess.PrNumber)
	}
	if sess.PrUrl == nil || *sess.PrUrl != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PrUrl = %v, want generated URL", sess.PrUrl)
	}
}

func TestCLI_Session_LinkPR_URL(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("session", "link-pr", "sess-aaa-111", "https://github.com/recurser/bossanova/pull/77")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	sess := findSession(t, h, "sess-aaa-111")
	if sess.PrNumber == nil || *sess.PrNumber != 77 {
		t.Fatalf("PrNumber = %v, want 77", sess.PrNumber)
	}
	if sess.PrUrl == nil || *sess.PrUrl != "https://github.com/recurser/bossanova/pull/77" {
		t.Fatalf("PrUrl = %v, want URL", sess.PrUrl)
	}
}

func findSession(t *testing.T, h *clitest.Harness, id string) *pb.Session {
	t.Helper()
	for _, sess := range h.Daemon.Sessions() {
		if sess.Id == id {
			return sess
		}
	}
	t.Fatalf("session %q not found", id)
	return nil
}
