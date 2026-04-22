package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestCLI_Archive_Success(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("archive", "sess-aaa-111")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Verify archive actually applied in the mock daemon's in-memory state —
	// the output-text assertion alone could silently pass if the RPC wasn't
	// invoked.
	var archived bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.ArchivedAt != nil {
			archived = true
		}
	}
	if !archived {
		t.Errorf("expected sess-aaa-111 to be archived in mock daemon state")
	}

	if !strings.Contains(res.Stdout, "archived") {
		t.Errorf("stdout missing 'archived' confirmation: %q", res.Stdout)
	}
}

func TestCLI_Archive_UnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("archive", "nosuchsession")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown session, got 0")
	}
}
